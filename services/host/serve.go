// `serve` is the plugin supervisor. It runs the long-running HTTP host services
// (memory :11435, knowledge :11436), resolving each
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
	"pix/host/plugin"
)

type hostService struct {
	name  string
	addr  string
	mux   http.Handler
	check func() error // optional serve-preflight; if non-nil it MUST pass or `serve` barfs
}

// runServe starts the long-running HTTP host services. `enabled` is the list
// from `services` in config.toml (config-friendly aliases: memory, knowledge);
// empty means "all". The MCP servers (e.g. slack) are stdio commands run by the
// sbx gateway via `sbx mcp add`, not HTTP daemons.
// serveServiceAliases is the config name -> internal service name table: the
// WHOLE set of capabilities `serve` composes. A retired capability leaves here,
// which is what makes `serve <retired>` a usage error, not a started daemon.
func serveServiceAliases() map[string]string {
	return map[string]string{"memory": "memory"}
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

	selfPath, err := os.Executable()
	if err != nil {
		fatalf("locate self: %v", err)
	}

	// Resolve the enabled set FIRST (F1): CLI args win; else config's `services`;
	// else empty == "all". Only enabled services are constructed, launched, and
	// preflighted below — so `serve memory` never launches or preflights knowledge.
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

	// knowledge: RETIRED (W1 U01a); a stale services entry is dropped on load.

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

// reindexKnowledgePlugin indexes the configured bundles into a freshly launched
// EXTERNAL knowledge plugin (F2). Without this the plugin path only installs the
// proxy shim and never calls Reindex, so the external/self-exec plugin serves an
// EMPTY index. It mirrors the built-in branch's logging + degrade behavior: no
// bundles logs an empty-index notice, a reindex failure is logged loudly (a bad
// OPTIONAL bundle must NOT crash serve), and success reports the counts. Factored
// out of runServe so it is unit-testable with an injected/stubbed store.
func reindexKnowledgePlugin(store plugin.KnowledgeStore, bundles []string) {
	if store == nil {
		log.Print("knowledge: plugin unavailable at startup — skipping reindex (will index on demand)")
		return
	}
	if len(bundles) == 0 {
		log.Print("knowledge: no bundles configured (set knowledge_bundles in config or KNOWLEDGE_BUNDLES); serving an empty index")
		return
	}
	if res, err := store.Reindex(plugin.ReindexArgs{BundlePaths: bundles}); err != nil {
		log.Printf("knowledge: reindex failed (serving whatever was already indexed): %v", err)
	} else {
		log.Printf("knowledge: indexed %d concept(s) from %d bundle(s)", res.Indexed, len(res.Bundles))
	}
}

// knowledgeBundles resolves the OKF bundle paths to index at startup: the
// configured knowledge_bundles win; otherwise the KNOWLEDGE_BUNDLES env is
// split on the OS path-list separator or a comma (a convenience for `serve
// knowledge` smoke runs without a config file). Empty means no bundles.
func knowledgeBundles(cfg *config.Config) []string {
	// Index the UNION across the base config and every profile so a `serve`
	// started under one context still serves the bundles another profile scopes
	// to (per-profile scoping happens at query time, not at index time).
	if all := cfg.AllKnowledgeBundles(); len(all) > 0 {
		return all
	}
	raw := strings.TrimSpace(os.Getenv("KNOWLEDGE_BUNDLES"))
	if raw == "" {
		return nil
	}
	split := func(r rune) bool { return r == os.PathListSeparator || r == ',' }
	var out []string
	for _, p := range strings.FieldsFunc(raw, split) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
