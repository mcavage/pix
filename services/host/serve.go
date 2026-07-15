// `serve` is the plugin supervisor. It runs the long-running HTTP host services
// (memory :11435, knowledge :11436, plus any overlay services), resolving each
// capability slot from config: a "builtin" impl runs IN-PROCESS exactly as
// before (memoryMux()); a non-builtin impl is launched ONCE at
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
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"pi-stack/host/config"
	"pi-stack/host/plugin"
)

type hostService struct {
	name  string
	addr  string
	mux   http.Handler
	check func() error // optional serve-preflight; if non-nil it MUST pass or `serve` barfs
}

// runServe starts the long-running HTTP host services. `enabled` is the list
// from `SERVICES` in config/local.mk (config-friendly aliases: memory, knowledge,
// plus any overlay registers); empty means "all". The MCP servers (e.g. slack) are
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

	// The default path has NO built-in credential broker and mints NO bearer
	// (memory/knowledge need no auth). The generic broker seam is dormant: a
	// broker materializes only when an overlay registers one (via
	// extraServiceFactories), and that overlay owns its own bearer through the
	// retained plugin path — never the process-global env (see pluginEnv, F2).
	selfPath, err := os.Executable()
	if err != nil {
		fatalf("locate self: %v", err)
	}

	// Resolve the enabled set FIRST (F1): CLI args win; else config's `services`;
	// else empty == "all". Only enabled services are constructed, launched, and
	// preflighted below — so `serve memory` never launches or preflights knowledge.
	effective := resolveServices(enabled, cfg.Services)
	// config-friendly aliases -> internal service name. Built-ins, plus any
	// overlay-registered aliases (extraServiceAliases) and their identity name —
	// so the public tree never hardcodes an overlay name.
	alias := map[string]string{
		"memory":    "memory",
		"knowledge": "knowledge",
		"broker":    "broker",
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

	// memory: the built-in store runs IN-PROCESS (fast path — recall is per-turn);
	// only a non-builtin impl is spawned as a plugin. memory degrades gracefully
	// (recall -> keyword, capture off) and logs its own status, so no fatal
	// preflight in either path.
	if enabledSvc("memory") {
		// Wire the configured model names into the in-process build (F6). An
		// explicit env override still wins; otherwise the config value (or its
		// default) applies.
		applyMemoryModelEnv(cfg)
		memSvc := hostService{name: "memory", addr: env("MEMORY_BIND", "127.0.0.1") + ":" + env("MEMORY_PORT", "11435")}
		if spec := cfg.Plugin("memory"); spec.Impl == config.BuiltinImpl {
			// Build the store with error handling and route a failure through fatalf
			// (F3): a bare log.Fatalf here would skip sup.shutdown() and orphan an
			// already-launched plugin (e.g. an overlay broker or service subprocess).
			store, hasEmb, berr := buildMemStore()
			if berr != nil {
				fatalf("%v", berr)
			}
			memSvc.mux = newMemoryMux(store, hasEmb)
		} else {
			h, lerr := sup.launch("memory", "memory", spec, selfPath, nil)
			if lerr != nil {
				fatalf("launch memory plugin: %v", lerr)
			}
			memSvc.mux = memoryProxyMux(h)
		}
		all = append(all, memSvc)
	}

	// knowledge: the OKF retrieval index. Opt-in (NOT in DefaultServices) — it only
	// runs when named in SERVICES/config, so a fresh install pays nothing for it.
	// Like memory, the built-in store runs IN-PROCESS (fast path); only a
	// non-builtin impl is spawned as a plugin. The configured bundles are indexed
	// at startup and it degrades loudly (empty index / keyword-only) rather than
	// failing, so no fatal preflight in either path.
	if enabledSvc("knowledge") {
		// The knowledge embedder reuses the memory embed model knob (MEMORY_EMBED_MODEL);
		// wire it in for the case where knowledge runs without memory enabled.
		applyMemoryModelEnv(cfg)
		knSvc := hostService{name: "knowledge", addr: env("KNOWLEDGE_BIND", "127.0.0.1") + ":" + env("KNOWLEDGE_PORT", "11436")}
		if spec := cfg.Plugin("knowledge"); spec.Impl == config.BuiltinImpl {
			// Route a build failure through fatalf (F3) so cleanup runs and any
			// already-launched plugin subprocess is not orphaned.
			store, _, kerr := buildKnowledgeStore()
			if kerr != nil {
				fatalf("%v", kerr)
			}
			bundles := knowledgeBundles(cfg)
			if len(bundles) == 0 {
				log.Print("knowledge: no bundles configured (set knowledge_bundles in config or KNOWLEDGE_BUNDLES) — serving an empty index")
			} else if n, indexed, rerr := store.reindex(bundles); rerr != nil {
				log.Printf("knowledge: reindex failed (serving whatever was already indexed): %v", rerr)
			} else {
				log.Printf("knowledge: indexed %d concept(s) from %d bundle(s)", n, len(indexed))
			}
			knSvc.mux = knowledgeMux(store)
		} else {
			h, lerr := sup.launch("knowledge", "knowledge", spec, selfPath, nil)
			if lerr != nil {
				fatalf("launch knowledge plugin: %v", lerr)
			}
			// F2: the built-in path indexes the configured bundles at startup; the
			// PLUGIN path must do the same or an external/self-exec knowledge plugin
			// serves an EMPTY index. Reindex the freshly dispensed store with the same
			// bundles + degrade behavior as the built-in branch.
			ks, _ := h.get().(plugin.KnowledgeStore)
			reindexKnowledgePlugin(ks, knowledgeBundles(cfg))
			knSvc.mux = knowledgeProxyMux(h)
		}
		all = append(all, knSvc)
	}

	// broker: the OVERLAY-ONLY credential-broker slot. DORMANT in the public tree —
	// there is NO built-in broker, so with the default builtin impl this starts
	// NOTHING (brokerService returns nil). It only materializes when an overlay
	// configures an external broker ([plugins.broker] with impl != builtin): then it
	// is launched ONCE through the shared supervisor (sha-verified + env-isolated,
	// F2), the dispensed CredentialBroker backs the stable /token shim, and it
	// participates in shutdown — mirroring the memory/knowledge non-builtin path.
	if enabledSvc("broker") {
		brSvc, berr := brokerService(cfg, sup, selfPath)
		if berr != nil {
			fatalf("launch broker plugin: %v", berr)
		}
		if brSvc != nil {
			all = append(all, *brSvc)
		}
	}

	// Overlay services (e.g. a warehouse proxy) self-register via init() when
	// present. Only the enabled ones are kept (the public tree has none).
	for _, f := range extraServiceFactories {
		if svc := f(); enabledSvc(svc.name) {
			all = append(all, svc)
		}
	}

	if len(all) == 0 {
		fatalf("no services enabled (set SERVICES in config/local.mk, e.g. SERVICES = memory)")
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

	// Graceful shutdown: kill every managed plugin subprocess on SIGINT/SIGTERM so
	// nothing is orphaned (no-op when everything is built-in/in-process), and drop
	// the pidfile so `serve status` doesn't report a dead pid.
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		<-ch
		log.Print("serve: shutting down; cleaning up plugin subprocesses")
		sup.shutdown()
		removeServePidFile()
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

// brokerService builds the OVERLAY-ONLY credential-broker slot. It returns
// (nil, nil) when the broker impl is builtin — the default PUBLIC case, where NO
// built-in broker exists, so nothing starts. When an overlay configures an
// external broker ([plugins.broker] with impl != builtin), it launches that
// binary ONCE through the shared supervisor (goplugin.NewClient + verifyPluginSHA
// + pluginEnv isolation), dispenses the CredentialBroker, and returns a
// hostService that serves the stable /token shim (brokerProxyMux). The broker is
// the ONLY plugin granted the bearer back (pluginEnv strips PI_STACK_BROKER_AUTH
// from every other subprocess; F2), and the same bearer gates the /token shim.
func brokerService(cfg *config.Config, sup *supervisor, selfPath string) (*hostService, error) {
	spec := cfg.Plugin("broker")
	if spec.Impl == config.BuiltinImpl {
		return nil, nil // dormant seam: no built-in broker in the public tree
	}
	// The broker gets its bearer back (and only the broker) via a granted extraEnv;
	// the same value gates the /token shim. An empty bearer disables the check.
	bearer := os.Getenv("PI_STACK_BROKER_AUTH")
	var grant []string
	if bearer != "" {
		grant = []string{"PI_STACK_BROKER_AUTH=" + bearer}
	}
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
		log.Print("knowledge: no bundles configured (set knowledge_bundles in config or KNOWLEDGE_BUNDLES) — serving an empty index")
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
