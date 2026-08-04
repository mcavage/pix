// `serve` is the plugin supervisor. It runs the long-running HTTP host services
// (memory :11435, plus the dormant broker slot), resolving each
// capability slot from config: a "builtin" impl runs IN-PROCESS exactly as
// before (memoryMux()); a non-builtin impl is launched ONCE at
// startup as a go-plugin subprocess and the HTTP shim proxies to it. Plugins
// never bind ports — the supervisor owns the listeners and the stable host
// surface every sandbox already depends on. The MCP servers (e.g. slack) are
// stdio and spawned on demand by the sbx gateway (`mcp <name>`), not here.

package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"pix/host/config"
)

type hostService struct {
	name  string
	addr  string
	mux   http.Handler
	check func() error // optional serve-preflight; if non-nil it MUST pass or `serve` barfs
}

// runServe starts the long-running HTTP host services. `enabled` is the list
// from `services` in config.toml (config-friendly aliases: memory, broker);
// empty means "all". The MCP servers (e.g. slack) are stdio commands run by
// the sbx gateway via `sbx mcp add`, not HTTP daemons.
// serveServiceAliases is the config name -> internal service name table: the
// WHOLE set of capabilities `serve` composes. A retired capability leaves here,
// which is what makes `serve <retired>` a usage error, not a started daemon.
func serveServiceAliases() map[string]string {
	return map[string]string{"memory": "memory", "broker": "broker"}
}

func runServe(enabled []string) {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("serve: load config: %v", err)
	}

	sup := &supervisor{}
	// fatalf routes every fatal exit through plugin cleanup (F4). A plain
	// log.Fatalf calls os.Exit and skips the signal-handler shutdown, orphaning
	// any launched plugin subprocess; sup.shutdown() is a no-op with none running,
	// so this is always safe. Use it for every fatal after `sup` exists.
	fatalf := func(format string, a ...any) {
		log.Printf("serve: "+format, a...)
		sup.shutdown()
		os.Exit(1)
	}

	// The default path has NO built-in credential broker and mints NO bearer
	// (memory needs no auth). The generic broker seam is dormant: a
	// broker materializes only when an operator configures an external
	// [plugins.broker] binary, which owns its own bearer through the retained
	// plugin path — never the process-global env (see pluginEnv, F2).
	selfPath, err := os.Executable()
	if err != nil {
		fatalf("locate self: %v", err)
	}

	// Resolve the enabled set FIRST (F1): CLI args win; else config's `services`;
	// else empty == "all". Only enabled services are constructed, launched, and
	// preflighted below — so `serve memory` never launches or preflights broker.
	effective := resolveServices(enabled, cfg.Services)
	// config-friendly aliases -> internal service name.
	alias := serveServiceAliases()
	valid := make([]string, 0, len(alias))
	for k := range alias {
		valid = append(valid, k)
	}
	sort.Strings(valid)
	want := map[string]bool{}
	for _, e := range effective {
		if strings.TrimSpace(e) == "" {
			continue
		}
		n, ok := alias[e]
		if !ok {
			fatalf("unknown service %q (valid: %s)", e, strings.Join(valid, ", "))
		}
		want[n] = true
	}
	enabledSvc := func(name string) bool { return len(want) == 0 || want[name] }

	var all []hostService

	// memory ALWAYS runs as a supervised go-plugin unit — the built-in impl as a
	// self-exec of this binary (`pix-host plugin memory`), a configured impl as
	// its own sha-pinned executable. One path, one lifecycle: the store (and its
	// advisory lock, taken by servePluginMemory) lives in a child process the
	// supervision tree can restart, health-probe and reattach to, while THIS
	// process keeps owning the :11435 listener the sandbox depends on. The
	// JSON-RPC surface is unchanged — memoryProxyMux serves the same methods and
	// params, over the same real SQLite store.
	if enabledSvc("memory") {
		// Wire the configured model names into the env the unit inherits (F6). An
		// explicit env override still wins; otherwise the config value applies.
		applyMemoryModelEnv(cfg)
		memSvc := hostService{name: "memory", addr: env("MEMORY_BIND", "127.0.0.1") + ":" + env("MEMORY_PORT", "11435")}
		spec := cfg.Plugin("memory")
		h, lerr := sup.launch("memory", "memory", spec, selfPath, spec.ExtraEnv)
		if lerr != nil {
			fatalf("launch memory unit: %v", lerr)
		}
		memSvc.mux = memoryProxyMux(h)
		all = append(all, memSvc)
	}

	// broker: the DORMANT credential-broker slot. There is NO built-in broker, so
	// with the default builtin impl this starts NOTHING (brokerService returns
	// nil). It only materializes when an operator configures an external broker
	// ([plugins.broker] with impl != builtin): then it is launched ONCE through the
	// shared supervisor (sha-verified + env-isolated, F2), the dispensed
	// CredentialBroker backs the stable /token shim, and it participates in
	// shutdown — mirroring the memory non-builtin path.
	if enabledSvc("broker") {
		brSvc, berr := brokerService(cfg, sup, selfPath)
		if berr != nil {
			fatalf("launch broker plugin: %v", berr)
		}
		if brSvc != nil {
			all = append(all, *brSvc)
		}
	}

	if len(all) == 0 {
		fatalf("no services enabled (run: pix config set services memory)")
	}

	// Preflight: every enabled service validates its host dependency UP FRONT, and
	// the whole `serve` barfs if any is broken — so you fix it now instead of
	// discovering mid-session that a capability was dark the whole time (the service
	// bound its port but couldn't actually serve). Services that degrade gracefully
	// (memory) set no check. `all` already holds only enabled services.
	var failures []string
	for _, s := range all {
		if s.check == nil {
			continue
		}
		if err := s.check(); err != nil {
			failures = append(failures, "  ✗ "+s.name+": "+err.Error())
		} else {
			log.Printf("preflight ok: %s", s.name)
		}
	}
	if len(failures) > 0 {
		fatalf("host service preflight FAILED — not starting:\n%s\nFix the above, then re-run `make serve`.", strings.Join(failures, "\n"))
	}

	// Record our pid so the launcher's `serve stop` / `serve status` can find and
	// signal us safely (no blind pkill). A stale pidfile from a previous crash is
	// overwritten — the new pid is authoritative. Best-effort: a failure only logs,
	// and we remove the file again on graceful shutdown (below) and normal return.
	writeServePidFile()
	defer removeServePidFile()
	defer removeServeLazyMarker()

	// fatalCh carries service-goroutine errors (e.g. port already in use) back to
	// the main goroutine so deferred cleanup (pidfile, lazy marker) runs before
	// exit rather than being skipped by a direct os.Exit in a side goroutine (M-4).
	fatalCh := make(chan error, len(all)+1)

	// sigCh receives SIGINT/SIGTERM for graceful shutdown. Handling it in the main
	// goroutine's select (instead of a side goroutine calling os.Exit) ensures the
	// deferred cleanup runs on normal signal handling (M-4).
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	for _, s := range all {
		s := s
		// Bound HTTP servers: ReadTimeout prevents slow-loris header floods;
		// WriteTimeout caps long-running handlers. Synthesis is the ceiling at ~60s,
		// so 90s gives comfortable headroom. (M-1)
		srv := &http.Server{
			Addr:         s.addr,
			Handler:      s.mux,
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 90 * time.Second,
		}
		log.Printf("starting %s on http://%s", s.name, s.addr)
		go func() {
			if err := srv.ListenAndServe(); err != nil {
				fatalCh <- fmt.Errorf("%s: %v", s.name, err)
			}
		}()
	}

	// Block until a signal or a service failure. The select here (vs the former
	// goroutine calling os.Exit directly) lets deferred cleanup run for both paths.
	select {
	case sig := <-sigCh:
		log.Printf("serve: received %v; shutting down", sig)
		sup.shutdown()
		// defers (removeServePidFile, removeServeLazyMarker) run on return
	case err := <-fatalCh:
		log.Printf("serve: fatal: %v", err)
		sup.shutdown()
		// Explicitly run deferred cleanup before os.Exit since defers don't run
		// when Exit is called (defers only run on return from runServe).
		removeServePidFile()
		removeServeLazyMarker()
		os.Exit(1)
	}
}

// writeServePidFile records the current pid at config.ServePidPath() (0600, dir
// 0700) so the launcher's `serve stop` / `serve status` can find and signal this
// supervisor safely. Best-effort: a failed MkdirAll/write only logs — it never
// crashes serve. A stale pidfile from a previous crash is overwritten, since the
// live pid is authoritative.
func writeServePidFile() {
	path := config.ServePidPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		log.Printf("serve: could not create pidfile dir %s: %v", filepath.Dir(path), err)
		return
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		log.Printf("serve: could not write pidfile %s: %v", path, err)
	}
}

// removeServePidFile deletes the pidfile on shutdown. Best-effort: a missing file
// is fine and any other remove error only logs.
func removeServePidFile() {
	path := config.ServePidPath()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.Printf("serve: could not remove pidfile %s: %v", path, err)
	}
}

// removeServeLazyMarker clears the launcher-written serve.lazy marker on a
// clean exit (belt + suspenders with `serve stop`'s own removal), so a lazily
// started daemon that shuts down gracefully never leaves a stale "lazy" flag.
// A hard kill leaves it behind — harmless, since lifecycle-mode detection also
// requires a live, verified-ours pidfile.
func removeServeLazyMarker() {
	if err := os.Remove(config.ServeLazyMarkerPath()); err != nil && !os.IsNotExist(err) {
		log.Printf("serve: could not remove lazy marker: %v", err)
	}
}

// brokerService builds the DORMANT credential-broker slot. It returns
// (nil, nil) when the broker impl is builtin — the default PUBLIC case, where NO
// built-in broker exists, so nothing starts. When an operator configures an
// external broker ([plugins.broker] with impl != builtin), it launches that
// binary ONCE through the shared supervisor (goplugin.NewClient + verifyPluginSHA
// + pluginEnv isolation), dispenses the CredentialBroker, and returns a
// hostService that serves the stable /token shim (brokerProxyMux). The broker is
// the ONLY plugin granted the bearer back (pluginEnv strips PIX_BROKER_AUTH
// from every other subprocess; F2), and the same bearer gates the /token shim.
func brokerService(cfg *config.Config, sup *supervisor, selfPath string) (*hostService, error) {
	spec := cfg.Plugin("broker")
	if spec.Impl == config.BuiltinImpl {
		return nil, nil // dormant seam: no built-in broker in the public tree
	}
	// The broker gets its bearer back (and only the broker) via a granted extraEnv;
	// the same value gates the /token shim. FAIL CLOSED: an enabled broker with no
	// bearer would serve /token unauthenticated (mint a real access token to any
	// process that can reach the listener — and BROKER_BIND can widen that past
	// localhost). Refuse to start rather than expose an open token endpoint.
	bearer := os.Getenv("PIX_BROKER_AUTH")
	if bearer == "" {
		return nil, fmt.Errorf("broker plugin is enabled but PIX_BROKER_AUTH is empty: refusing to serve an unauthenticated /token endpoint")
	}
	grant := []string{"PIX_BROKER_AUTH=" + bearer}
	// Append any per-plugin extra env vars from config (ExtraEnv is wired here so
	// an operator's [plugins.broker] extra_env entries are actually passed through).
	grant = append(grant, spec.ExtraEnv...)
	h, err := sup.launch("broker", "broker", spec, selfPath, grant)
	if err != nil {
		return nil, err
	}
	return &hostService{
		name:  "broker",
		addr:  env("BROKER_BIND", "127.0.0.1") + ":" + env("BROKER_PORT", "11437"),
		mux:   brokerProxyMux(h, bearer),
		check: func() error { return brokerCheck(h) },
	}, nil
}

// resolveServices picks the effective service list for `serve` (F1): the CLI
// args win if any is non-empty; otherwise config's `services`; an empty result
// means "all". CLI wins so `serve memory` overrides a broader config set.
func resolveServices(cli, cfgServices []string) []string {
	for _, s := range cli {
		if strings.TrimSpace(s) != "" {
			return cli
		}
	}
	return cfgServices
}

// applyMemoryModelEnv wires the configured memory model names into the
// in-process memory build (F6). memembed.go reads these from the env, so an
// explicit env override wins; otherwise the config value (or its default)
// applies. Set only in the memory branch so it never leaks into an unrelated
// plugin subprocess (they also get a filtered Cmd.Env — see pluginEnv).
func applyMemoryModelEnv(cfg *config.Config) {
	if os.Getenv("MEMORY_WATCHER_MODEL") == "" && cfg.MemoryWatcherModel != "" {
		os.Setenv("MEMORY_WATCHER_MODEL", cfg.MemoryWatcherModel)
	}
	if os.Getenv("MEMORY_EMBED_MODEL") == "" && cfg.MemoryEmbedModel != "" {
		os.Setenv("MEMORY_EMBED_MODEL", cfg.MemoryEmbedModel)
	}
}
