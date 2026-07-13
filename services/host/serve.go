// `serve` is the plugin supervisor. It runs the long-running HTTP host services
// (gws-token :11441, memory :11435, plus any overlay services), resolving each
// capability slot from config: a "builtin" impl runs IN-PROCESS exactly as
// before (memoryMux() / gwsTokenMux()); a non-builtin impl is launched ONCE at
// startup as a go-plugin subprocess and the HTTP shim proxies to it. Plugins
// never bind ports — the supervisor owns the listeners and the stable host
// surface every sandbox already depends on. The MCP servers (e.g. slack) are
// stdio and spawned on demand by the sbx gateway (`mcp <name>`), not here.

package main

import (
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"pi-stack/host/config"
)

type hostService struct {
	name  string
	addr  string
	mux   http.Handler
	check func() error // optional serve-preflight; if non-nil it MUST pass or `serve` barfs
}

// runServe starts the long-running HTTP host services. `enabled` is the list
// from `SERVICES` in config/local.mk (config-friendly aliases: memory, gws, plus
// any overlay registers); empty means "all". The MCP servers (e.g. slack) are
// stdio commands run by the sbx gateway via `sbx mcp add`, not HTTP daemons.
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

	// Broker bearer (zero friction): mint (or reuse) the shared token and REQUIRE
	// it on the gws-token shim. It is passed EXPLICITLY to the gws mux/handler and
	// (for an external broker) to that plugin's OWN env only — never into the
	// process-global env. Otherwise every memory/mcp plugin subprocess would
	// inherit the bearer and could mint Google tokens at :11441 (F2).
	tok, err := config.EnsureToken()
	if err != nil {
		fatalf("broker token: %v", err)
	}

	selfPath, err := os.Executable()
	if err != nil {
		fatalf("locate self: %v", err)
	}

	// Resolve the enabled set FIRST (F1): CLI args win; else config's `services`;
	// else empty == "all". Only enabled services are constructed, launched, and
	// preflighted below — so `serve memory` never launches or preflights gws.
	effective := resolveServices(enabled, cfg.Services)
	// config-friendly aliases -> internal service name. Built-ins, plus any
	// overlay-registered aliases (extraServiceAliases) and their identity name —
	// so the public tree never hardcodes an overlay name.
	alias := map[string]string{
		"gws": "gws-token", "gws-token": "gws-token",
		"memory": "memory",
	}
	for k, v := range extraServiceAliases {
		alias[k] = v
		alias[v] = v
	}
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

	// gws-token: builtin runs in-process; a non-builtin impl is launched as a
	// CredentialBroker plugin and the /token shim proxies to it. It barfs if the
	// host gws isn't authenticated (else it starts but serves "no host token" and
	// Gmail/Calendar are silently dark in the VM).
	if enabledSvc("gws-token") {
		gwsSvc := hostService{name: "gws-token", addr: env("GWS_TOKEN_BIND", "127.0.0.1") + ":" + env("GWS_TOKEN_PORT", "11441"), check: gwsTokenCheck}
		if spec := cfg.Plugin("gws"); spec.Impl == config.BuiltinImpl {
			gwsSvc.mux = gwsTokenMux(tok)
		} else {
			// The bearer is granted to the broker plugin's OWN env only (F2).
			h, lerr := sup.launch("gws-token", "broker", spec, selfPath, []string{"GWS_TOKEN_AUTH=" + tok})
			if lerr != nil {
				fatalf("launch gws broker plugin: %v", lerr)
			}
			gwsSvc.mux = gwsBrokerProxyMux(h, tok)
			gwsSvc.check = func() error { return brokerCheck(h) }
		}
		all = append(all, gwsSvc)
	}

	// memory: the built-in store runs IN-PROCESS (fast path — recall is per-turn);
	// only a non-builtin impl is spawned as a plugin. memory degrades gracefully
	// (recall -> keyword, capture off) and logs its own status, so no fatal
	// preflight in either path.
	if enabledSvc("memory") {
		// Wire the configured model names into the in-process build (F6). An
		// explicit env override still wins; otherwise the config value (or its
		// default) applies. Set AFTER any gws launch above so it can never leak
		// into the broker subprocess's inherited env.
		applyMemoryModelEnv(cfg)
		memSvc := hostService{name: "memory", addr: env("MEMORY_BIND", "127.0.0.1") + ":" + env("MEMORY_PORT", "11435")}
		if spec := cfg.Plugin("memory"); spec.Impl == config.BuiltinImpl {
			memSvc.mux = memoryMux()
		} else {
			h, lerr := sup.launch("memory", "memory", spec, selfPath, nil)
			if lerr != nil {
				fatalf("launch memory plugin: %v", lerr)
			}
			memSvc.mux = memoryProxyMux(h)
		}
		all = append(all, memSvc)
	}

	// Overlay services (e.g. a warehouse proxy) self-register via init() when
	// present. Only the enabled ones are kept (the public tree has none).
	for _, f := range extraServiceFactories {
		if svc := f(); enabledSvc(svc.name) {
			all = append(all, svc)
		}
	}

	if len(all) == 0 {
		fatalf("no services enabled (set SERVICES in config/local.mk, e.g. SERVICES = memory gws)")
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

	// Graceful shutdown: kill every managed plugin subprocess on SIGINT/SIGTERM so
	// nothing is orphaned (no-op when everything is built-in/in-process).
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		<-ch
		log.Print("serve: shutting down; cleaning up plugin subprocesses")
		sup.shutdown()
		os.Exit(0)
	}()

	for _, s := range all {
		s := s
		log.Printf("starting %s on http://%s", s.name, s.addr)
		go func() {
			if err := http.ListenAndServe(s.addr, s.mux); err != nil {
				fatalf("%s: %v", s.name, err)
			}
		}()
	}
	select {} // block forever; the goroutines serve
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
