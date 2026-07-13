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

	// Broker bearer (zero friction): mint (or reuse) the shared token and REQUIRE
	// it on the gws-token shim. Set GWS_TOKEN_AUTH before constructing the shim
	// (gwsTokenMux() reads it at build time); the launcher injects the same token
	// into the sandbox, so the user never sees it. Applies to both the built-in
	// and the plugin-backed broker path.
	tok, err := config.EnsureToken()
	if err != nil {
		log.Fatalf("serve: broker token: %v", err)
	}
	os.Setenv("GWS_TOKEN_AUTH", tok)

	selfPath, err := os.Executable()
	if err != nil {
		log.Fatalf("serve: locate self: %v", err)
	}

	sup := &supervisor{}

	// gws-token: builtin runs in-process; a non-builtin impl is launched as a
	// CredentialBroker plugin and the /token shim proxies to it. It barfs if the
	// host gws isn't authenticated (else it starts but serves "no host token" and
	// Gmail/Calendar are silently dark in the VM).
	gwsSvc := hostService{name: "gws-token", addr: env("GWS_TOKEN_BIND", "127.0.0.1") + ":" + env("GWS_TOKEN_PORT", "11441"), check: gwsTokenCheck}
	if spec := cfg.Plugin("gws"); spec.Impl == config.BuiltinImpl {
		gwsSvc.mux = gwsTokenMux()
	} else {
		h, lerr := sup.launch("gws-token", "broker", spec, selfPath)
		if lerr != nil {
			log.Fatalf("serve: launch gws broker plugin: %v", lerr)
		}
		gwsSvc.mux = gwsBrokerProxyMux(h, tok)
		gwsSvc.check = func() error { return brokerCheck(h) }
	}

	// memory: the built-in store runs IN-PROCESS (fast path — recall is per-turn);
	// only a non-builtin impl is spawned as a plugin. memory degrades gracefully
	// (recall -> keyword, capture off) and logs its own status, so no fatal
	// preflight in either path.
	memSvc := hostService{name: "memory", addr: env("MEMORY_BIND", "127.0.0.1") + ":" + env("MEMORY_PORT", "11435")}
	if spec := cfg.Plugin("memory"); spec.Impl == config.BuiltinImpl {
		memSvc.mux = memoryMux()
	} else {
		h, lerr := sup.launch("memory", "memory", spec, selfPath)
		if lerr != nil {
			log.Fatalf("serve: launch memory plugin: %v", lerr)
		}
		memSvc.mux = memoryProxyMux(h)
	}

	all := []hostService{gwsSvc, memSvc}
	// Overlay services (e.g. a warehouse proxy) self-register via init() when present.
	for _, f := range extraServiceFactories {
		all = append(all, f())
	}
	// config-friendly aliases -> internal service name. Built-ins, plus each
	// service's own name as an identity alias, plus any overlay-registered aliases
	// (extraServiceAliases) — so the public tree never hardcodes an overlay name.
	alias := map[string]string{
		"gws": "gws-token", "gws-token": "gws-token",
		"memory": "memory",
	}
	for _, s := range all {
		alias[s.name] = s.name
	}
	for k, v := range extraServiceAliases {
		alias[k] = v
	}
	valid := make([]string, 0, len(alias))
	for k := range alias {
		valid = append(valid, k)
	}
	sort.Strings(valid)
	want := map[string]bool{}
	for _, e := range enabled {
		if e == "" {
			continue
		}
		n, ok := alias[e]
		if !ok {
			log.Fatalf("serve: unknown service %q (valid: %s)", e, strings.Join(valid, ", "))
		}
		want[n] = true
	}
	// Preflight: every enabled service validates its host dependency UP FRONT, and
	// the whole `serve` barfs if any is broken — so you fix it now instead of
	// discovering mid-session that a capability was dark the whole time (the service
	// bound its port but couldn't actually serve). Services that degrade gracefully
	// (memory) set no check.
	var failures []string
	for _, s := range all {
		if (len(want) > 0 && !want[s.name]) || s.check == nil {
			continue
		}
		if err := s.check(); err != nil {
			failures = append(failures, "  ✗ "+s.name+": "+err.Error())
		} else {
			log.Printf("preflight ok: %s", s.name)
		}
	}
	if len(failures) > 0 {
		log.Fatalf("serve: host service preflight FAILED — not starting:\n%s\nFix the above, then re-run `make serve`.", strings.Join(failures, "\n"))
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

	started := 0
	for _, s := range all {
		if len(want) > 0 && !want[s.name] {
			continue
		}
		s := s
		log.Printf("starting %s on http://%s", s.name, s.addr)
		go func() {
			if err := http.ListenAndServe(s.addr, s.mux); err != nil {
				log.Fatalf("%s: %v", s.name, err)
			}
		}()
		started++
	}
	if started == 0 {
		log.Fatal("serve: no services enabled (set SERVICES in config/local.mk, e.g. SERVICES = memory gws)")
	}
	select {} // block forever; the goroutines serve
}
