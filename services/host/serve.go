// `serve` runs the long-running HTTP host services (gws-token, memory, plus any
// overlay services that self-register when present), each on its own port, in one
// process. The MCP servers (slack) are stdio and spawned on demand by the sbx
// gateway, not here.

package main

import (
	"log"
	"net/http"
	"sort"
	"strings"
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
	all := []hostService{
		// gws-token barfs if the host gws isn't authenticated (else it starts but
		// serves "no host token" and Gmail/Calendar are silently dark in the VM).
		{name: "gws-token", addr: env("GWS_TOKEN_BIND", "127.0.0.1") + ":" + env("GWS_TOKEN_PORT", "11441"), mux: gwsTokenMux(), check: gwsTokenCheck},
		// memory degrades gracefully (recall -> keyword, capture off) and logs its
		// own status, so it has no fatal preflight.
		{name: "memory", addr: env("MEMORY_BIND", "127.0.0.1") + ":" + env("MEMORY_PORT", "11435"), mux: memoryMux()},
	}
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
