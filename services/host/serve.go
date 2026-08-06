// `serve` is the plugin supervisor. It runs the long-running HTTP host services
// (memory :11435, monitor ingest :11437), resolving each capability slot from
// config. Plugins never bind ports — the supervisor owns the listeners and the
// stable host surface every sandbox depends on. MCP servers are stdio and
// spawned on demand by the sbx gateway, not here.
//
// monitor ingest is composed directly rather than as a supervised unit: it is one
// loopback listener over a file-backed store, and it already owns both its
// net.Listener (bound eagerly, so a port conflict is a startup error) and its
// context-based Serve/shutdown. `ctx` is threaded through so cancelling it on
// shutdown drains monitor the way SIGINT drains everything else. See
// docs/design/monitor.md.
//
// Startup is two strict phases, in order (bindFrontDoors, then
// spawnChildren): every built-in HTTP front door binds FIRST — before a
// single pack/plugin child is spawned or the pidfile is written — so a port
// conflict fails with zero subprocesses ever launched. Shutdown mirrors that
// (performShutdown): drain every front door first with a bounded context,
// then tear down the supervised backend, then wait for monitor's own drain —
// and no exit path calls os.Exit until that full sequence has returned, so
// exit-1 semantics on a fatal error are preserved but never early.
package main

import (
	"context"
	"fmt"
	"log"
	"maps"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/monitor"
	"pix/host/packinfo"
	"pix/host/workflow/pack"
)

// hostService is one mux-based listener `serve` owns: a name for the log line,
// the address it binds, the ALREADY-BOUND listener behind that address (see
// bindFrontDoors: every front door binds before anything spawns), and the
// handler behind it. There is no preflight hook by design — memory degrades
// rather than refusing to serve (a missing Ollama must not stop the store from
// answering).
type hostService struct {
	name string
	addr string
	ln   net.Listener
	mux  http.Handler
}

// frontDoorShutdownTimeout bounds how long a front-door http.Server is given to
// drain its in-flight requests once shutdown starts, before Close() forces it —
// the same ceiling monitor ingest's own Serve(ctx) already uses (see
// monitor/ingest.go), so no front door can wedge shutdown past that.
const frontDoorShutdownTimeout = 5 * time.Second

// serveUsage documents the flags runServe parses (--bind/--port are
// monitor-only; memory's bind/port stay env-only via MEMORY_BIND/MEMORY_PORT)
// plus the enabled-service positionals. `pix serve` intercepts -h/--help and
// prints service.Usage instead; this text is for a direct `pix-host serve -h`.
const serveUsage = `usage: pix-host serve [service...] [--bind ADDR] [--port N]

  Run the long-running host services: memory (:11435), monitor ingest
  (:11437). No service names given: run every service in ` + "`services`" + `
  (config.toml), or all of them if that is also unset.

  --bind ADDR   monitor ingest listen address (default 127.0.0.1,
                loopback-only). A non-loopback bind exposes the ingest
                endpoint — no auth, full agent context and tool output — to
                your local network; the process WARNS loudly when it does.
  --port N      monitor ingest port (default 11437)
`

// serveServiceAliases is the config name -> internal service name table: the
// WHOLE set of capabilities `serve` composes. A retired capability leaves here,
// which is what makes `serve <retired>` a usage error, not a started daemon.
func serveServiceAliases() map[string]string {
	return map[string]string{"memory": "memory", "monitor": "monitor"}
}

func runServe(argv []string) {
	fs := cli.NewFlagSet()
	monitorPort := fs.Int("port", monitor.DefaultPort)
	monitorBind := fs.Str("bind", monitor.DefaultBindAddr)
	enabled, ferr := fs.Parse(argv)
	if ferr != nil {
		fmt.Fprintf(os.Stderr, "pix-host serve: %v\n\n%s", ferr, serveUsage)
		os.Exit(2)
	}
	if fs.Help {
		fmt.Print(serveUsage)
		return
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("serve: load config: %v", err)
	}

	sup := &supervisor{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// fatalf routes every fatal exit through plugin cleanup: a plain log.Fatalf
	// skips the signal-handler shutdown and orphans a launched plugin subprocess.
	// sup.shutdown() is a no-op with none running, so use it for EVERY fatal below.
	fatalf := func(format string, a ...any) {
		log.Printf("serve: "+format, a...)
		cancel()
		sup.shutdown()
		os.Exit(1)
	}

	selfPath, err := os.Executable()
	if err != nil {
		fatalf("locate self: %v", err)
	}

	// Resolve the enabled set FIRST: CLI args win, else config's `services`, else
	// empty == "all". Only enabled services are bound and launched.
	effective := resolveServices(enabled, cfg.Services)
	// config-friendly aliases -> internal service name.
	alias := serveServiceAliases()
	valid := slices.Sorted(maps.Keys(alias))
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

	// Phase 1: bind every built-in HTTP front door EAGERLY — before a single
	// pack/plugin child is spawned or the pidfile is claimed. bindFrontDoors has
	// no access to `sup`/packinfo at all, so a bind failure here is
	// STRUCTURALLY incapable of having spawned anything: a losing second
	// `serve` racing an already-running daemon for the same port dies right
	// here, on a plain "address already in use", with zero subprocesses to
	// clean up.
	fd, berr := bindFrontDoors(enabledSvc, *monitorBind, *monitorPort)
	if berr != nil {
		fatalf("%v", berr)
	}
	if len(fd.all) == 0 && fd.monitorSrv == nil {
		fatalf("no services enabled (run: pix config set services %s)", strings.Join(valid, ","))
	}
	all, monitorSrv := fd.all, fd.monitorSrv

	// Phase 2: only once EVERY front door above is bound do we spawn pack units
	// and the memory plugin child — the one place either is ever launched.
	spawnChildren(cfg, sup, selfPath, all, fatalf)

	// Record our pid so the launcher's `serve stop`/`serve status` can find and
	// signal us safely (no blind pkill).
	writeServePidFile()
	defer removeServePidFile()
	defer removeServeLazyMarker()

	// fatalCh carries service-goroutine errors (e.g. port already in use) back to
	// the main goroutine, and sigCh carries SIGINT/SIGTERM: both are handled in the
	// select below rather than by a side goroutine calling os.Exit, so the deferred
	// cleanup (pidfile, lazy marker) actually runs.
	fatalCh := make(chan error, len(all)+2)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	httpServers := make([]*http.Server, 0, len(all))
	for _, s := range all {
		s := s
		// Bound HTTP servers: ReadTimeout prevents slow-loris header floods,
		// WriteTimeout caps long-running handlers. Synthesis is the ~60s ceiling.
		srv := &http.Server{
			Addr:         s.addr,
			Handler:      s.mux,
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 90 * time.Second,
		}
		httpServers = append(httpServers, srv)
		log.Printf("starting %s on http://%s", s.name, s.addr)
		go func() {
			if err := srv.Serve(s.ln); err != nil && err != http.ErrServerClosed {
				fatalCh <- fmt.Errorf("%s: %v", s.name, err)
			}
		}()
	}

	// monitor ingest runs its own Serve(ctx): cancelling ctx (inside
	// shutdownFrontDoors, alongside every mux-based front door) is what stops
	// it — there is no http.Server here for THIS process to Shutdown directly.
	waitMonitor := serveMonitorAndWait(monitorSrv, ctx, fatalCh)

	// Block until a signal or a service failure. Both paths run the SAME fixed
	// shutdown order: drain every front door first, then the supervised
	// backend, then wait for monitor's own graceful drain to finish. Neither
	// path calls os.Exit until that full sequence has actually returned, so a
	// fatal exit never skips the joined cleanup — exit 1 is preserved, just
	// never early.
	select {
	case sig := <-sigCh:
		log.Printf("serve: received %v; shutting down", sig)
		performShutdown(httpServers, cancel, sup.shutdown, waitMonitor)
		// defers (removeServePidFile, removeServeLazyMarker) run on return
	case err := <-fatalCh:
		log.Printf("serve: fatal: %v", err)
		performShutdown(httpServers, cancel, sup.shutdown, waitMonitor)
		// Explicitly run deferred cleanup before os.Exit since defers don't run
		// when Exit is called (defers only run on return from runServe).
		removeServePidFile()
		removeServeLazyMarker()
		os.Exit(1)
	}
}

// serveFrontDoors is bindFrontDoors' result: every built-in HTTP front door,
// already bound, plus the monitor ingest server (which owns its own listener).
type serveFrontDoors struct {
	all        []hostService
	monitorSrv *monitor.IngestServer
}

// bindFrontDoors binds every built-in HTTP front door — memory's :11435
// listener, monitor ingest's own loopback listener — and nothing else: it has
// no `*supervisor`, no packinfo, no way to spawn a subprocess. That is the
// whole point: a port conflict fails here, before runServe has any chance to
// spawn a pack unit or the memory plugin child, so the failure can never
// leave one orphaned. Called with a NON-nil error, the returned
// serveFrontDoors is always the zero value — every listener bound during THIS
// call is closed first, so a later failure (e.g. monitor, after memory
// already bound) never leaks the earlier winner's socket.
func bindFrontDoors(enabledSvc func(string) bool, monitorBind string, monitorPort int) (serveFrontDoors, error) {
	var fd serveFrontDoors
	// memory ALWAYS runs as a supervised go-plugin unit — the built-in impl as a
	// self-exec of this binary (`pix-host plugin memory`), a configured impl as its
	// own sha-pinned executable. One path, one lifecycle: the store (and its
	// advisory lock, taken by servePluginMemory) lives in a child the supervision
	// tree can restart, health-probe and reattach to, while THIS process keeps
	// owning the :11435 listener the sandbox depends on — bound here, well before
	// that child is ever spawned.
	if enabledSvc("memory") {
		addr := env("MEMORY_BIND", "127.0.0.1") + ":" + env("MEMORY_PORT", "11435")
		ln, lerr := net.Listen("tcp", addr)
		if lerr != nil {
			return serveFrontDoors{}, fmt.Errorf("bind memory (%s): %w", addr, lerr)
		}
		fd.all = append(fd.all, hostService{name: "memory", addr: addr, ln: ln})
	}

	// monitor ingest: a loopback HTTP listener over a bounded, file-backed store,
	// receiving NDJSON events + blob bodies from the in-sandbox tap. It spawns no
	// child of its own, but binding it here — before pack/memory-child spawn —
	// keeps every front door's bind attempt in the same, spawn-free phase.
	if enabledSvc("monitor") {
		root, rerr := config.MonitorStoreRoot()
		if rerr != nil {
			closeFrontDoorListeners(fd.all)
			return serveFrontDoors{}, fmt.Errorf("resolve monitor store root: %w", rerr)
		}
		if !isLoopbackAddr(monitorBind) {
			log.Printf("WARNING: monitor ingest is bound to %s, exposed on your local network with NO AUTHENTICATION — anyone on the network can send this sandbox's full agent context and tool output into the store. Use a firewall, or bind loopback (drop --bind) unless you specifically need this.", monitorBind)
		}
		srv, merr := buildMonitorIngest(monitorBind, monitorPort, root)
		if merr != nil {
			closeFrontDoorListeners(fd.all)
			return serveFrontDoors{}, fmt.Errorf("launch monitor ingest: %w", merr)
		}
		fd.monitorSrv = srv
		log.Printf("starting monitor on http://%s (store %s)", srv.Addr(), root)
	}

	return fd, nil
}

// closeFrontDoorListeners closes every listener already bound in `all` — used
// when a LATER front door in the same bindFrontDoors call fails, so the
// earlier winner's socket is never leaked into the fatalf/os.Exit that follows.
func closeFrontDoorListeners(all []hostService) {
	for _, s := range all {
		if s.ln != nil {
			_ = s.ln.Close()
		}
	}
}

// spawnChildren launches every pack/plugin child process — each active pack's
// Tier-1-accepted [[services]] units, then the memory plugin unit — and is the
// ONLY place either is spawned. runServe calls it exactly once, and only AFTER
// bindFrontDoors has already succeeded, so a bind failure can never leave one
// of these orphaned. It attaches the memory unit's proxy mux onto the
// already-bound hostService in `all`, in place.
func spawnChildren(cfg *config.Config, sup *supervisor, selfPath string, all []hostService, fatalf func(string, ...any)) {
	// Every active pack's Tier-1-accepted [[services]] view, collected across ALL
	// active packs and reconciled against the tree in exactly ONE call: no
	// `plugins.*` shortcut, and one bad pack's load/export failure only logs,
	// never blocking serve or a sibling pack. reconcilePackUnits treats its views
	// argument as the FULL desired state, so calling it once per pack (the prior
	// shape of this loop) made each pack's call remove every earlier pack's
	// units; mergePackServices flattens every pack's views into the one
	// argument this single call takes, and fails a colliding unit name closed
	// (naming both packs) instead of one pack's view silently overwriting
	// another's.
	var packSets []packServiceSet
	for _, root := range packinfo.ActivePackRoots(cfg, "") {
		p, perr := packinfo.LoadPack(root)
		if perr != nil {
			log.Printf("serve: pack %s: %v", root, perr)
			continue
		}
		views, verr := pack.AcceptedGoPluginServicesForSelf(p, cfg.GogAccount, selfPath)
		if verr != nil {
			log.Printf("serve: pack %s services: %v", root, verr)
			continue
		}
		if len(views) == 0 {
			continue
		}
		packSets = append(packSets, packServiceSet{packName: p.Manifest.Name, views: views})
	}
	merged, mergeErr := mergePackServices(packSets)
	if mergeErr != nil {
		log.Printf("serve: pack services: %v", mergeErr)
	}
	units, rerr := sup.reconcilePackUnits(selfPath, merged)
	if rerr != nil {
		log.Printf("serve: pack services: %v", rerr)
	}
	if len(units) > 0 {
		log.Printf("serve: supervising %d pack service(s): %s", len(units), strings.Join(slices.Sorted(maps.Keys(units)), ", "))
	}

	for i := range all {
		if all[i].name != "memory" {
			continue
		}
		// Wire the configured model names into the env the unit inherits; an explicit
		// env override still wins.
		applyMemoryModelEnv(cfg)
		spec := cfg.Plugin("memory")
		h, lerr := sup.launch("memory", "memory", spec, selfPath, spec.ExtraEnv)
		if lerr != nil {
			fatalf("launch memory unit: %v", lerr)
			return
		}
		all[i].mux = memoryProxyMux(h)
	}
}

// performShutdown is the ONE shutdown sequence every exit path — signal,
// fatal service error — runs, in a fixed order: drain every front door FIRST
// (stop accepting new connections immediately, then wait out any in-flight
// request bounded by frontDoorShutdownTimeout — see shutdownFrontDoors), THEN
// tear down the supervised backend (pack units + the memory plugin), THEN
// wait for monitor ingest's own graceful drain to actually finish. Getting
// this backwards — tearing the backend down while a front door still accepts
// — lets a brand-new request land on a backend that is already mid-teardown.
func performShutdown(httpServers []*http.Server, cancelMonitor context.CancelFunc, shutdownBackend func(), waitMonitor func()) {
	shutdownFrontDoors(httpServers, cancelMonitor)
	shutdownBackend()
	waitMonitor()
}

// shutdownFrontDoors stops accepting new connections and drains every
// mux-based front-door http.Server, bounded by frontDoorShutdownTimeout so a
// stuck handler cannot hang shutdown forever, and cancels ctx so monitor
// ingest's own Serve(ctx) begins its matching bounded drain at the same time.
// It returns once every http.Server's Shutdown call has returned; the caller
// waits on monitor separately (waitMonitor), so a slow monitor drain never
// blocks the backend teardown that follows.
func shutdownFrontDoors(servers []*http.Server, cancelMonitor context.CancelFunc) {
	cancelMonitor()
	var wg sync.WaitGroup
	for _, srv := range servers {
		srv := srv
		wg.Add(1)
		go func() {
			defer wg.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), frontDoorShutdownTimeout)
			defer cancel()
			if err := srv.Shutdown(shutdownCtx); err != nil {
				_ = srv.Close()
			}
		}()
	}
	wg.Wait()
}

// buildMonitorIngest constructs the monitor store + ingest server, split out
// from runServe so a test can build one against a t.TempDir() root without the
// signal handling, pidfile and plugin supervision around it.
func buildMonitorIngest(bind string, port int, root string) (*monitor.IngestServer, error) {
	store, err := monitor.NewStore(monitor.StoreConfig{Root: root})
	if err != nil {
		return nil, fmt.Errorf("monitor store: %w", err)
	}
	srv, err := monitor.NewIngestServer(monitor.IngestConfig{Port: port, BindAddr: bind, Store: store})
	if err != nil {
		return nil, fmt.Errorf("monitor ingest: %w", err)
	}
	return srv, nil
}

// serveMonitorAndWait runs monitorSrv.Serve(ctx) in the background and
// returns a wait func that blocks until that call has ACTUALLY returned —
// monitor's own graceful shutdown (bounded to 5s, see monitor/ingest.go)
// having already completed, not merely started. runServe calls wait() before
// it returns OR os.Exit(1)s, on every shutdown path, so a fatal failure
// elsewhere in `serve` can never cut monitor off mid-drain the way an
// unwaited background goroutine would. A nil monitorSrv (monitor not
// enabled) returns a no-op wait.
func serveMonitorAndWait(monitorSrv *monitor.IngestServer, ctx context.Context, fatalCh chan<- error) (wait func()) {
	if monitorSrv == nil {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := monitorSrv.Serve(ctx); err != nil {
			fatalCh <- fmt.Errorf("monitor: %v", err)
		}
	}()
	return func() { <-done }
}

// isLoopbackAddr reports whether host (no port) is the loopback interface: the
// warn-on-LAN-bind classification for monitor ingest (docs/design/monitor.md).
func isLoopbackAddr(host string) bool {
	switch host {
	case "", "localhost":
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// writeServePidFile records the current pid at config.ServePidPath() (0600, dir
// 0700). Best-effort: a failed MkdirAll/write only logs, never crashes serve. A
// stale pidfile from a previous crash is overwritten — the live pid is authority.
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

// removeServePidFile deletes the pidfile on shutdown — but ONLY if it still
// names OUR pid. See removeOwnedPidFile.
func removeServePidFile() {
	removeOwnedPidFile(config.ServePidPath())
}

// removeServeLazyMarker clears the launcher-written serve.lazy marker on a
// clean exit (belt + suspenders with `serve stop`'s own removal), so a lazily
// started daemon that shuts down gracefully never leaves a stale "lazy" flag.
// A hard kill leaves it behind — harmless, since lifecycle-mode detection also
// requires a live, verified-ours pidfile. Same ownership guard as the pidfile:
// the marker is written carrying the SAME pid (service/start.go's markLazy),
// so the check is identical.
func removeServeLazyMarker() {
	removeOwnedPidFile(config.ServeLazyMarkerPath())
}

// removeOwnedPidFile deletes path ONLY when it currently names OUR pid: two
// `serve` processes can race to write the SAME pidfile/lazy-marker path (a
// loser starts, binds nothing — see the eager listener binds above — but can
// still reach this cleanup on its way out through fatalf), and the loser's
// own deferred cleanup must never delete a file the WINNER (or any other,
// currently-running daemon) just wrote for itself. Reading before removing is
// a best-effort compare-and-delete, not atomic against a concurrent write —
// but the only way that race can resolve is in the FILE'S favor: we only ever
// delete a file that, at the moment we looked, said WE are its owner; we
// never delete one that says otherwise. A missing file is fine; any other
// read/remove error only logs.
func removeOwnedPidFile(path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("serve: could not read %s before removing: %v", path, err)
		}
		return
	}
	pid, perr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if perr != nil || pid != os.Getpid() {
		// Not ours (unparseable, or another process's pid): leave it exactly
		// alone. Whoever it belongs to is responsible for its own cleanup.
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.Printf("serve: could not remove %s: %v", path, err)
	}
}

// resolveServices picks the effective service list for `serve`: the CLI args win
// if any is non-empty (so `serve memory` overrides a broader config set), else
// config's `services`; an empty result means "all".
func resolveServices(cli, cfgServices []string) []string {
	for _, s := range cli {
		if strings.TrimSpace(s) != "" {
			return cli
		}
	}
	return cfgServices
}

// applyMemoryModelEnv wires the configured memory model names into the env the
// memory unit inherits (memembed.go reads them there), so an explicit env
// override wins and the config value applies otherwise. Set only in the memory
// branch so it never leaks into an unrelated plugin subprocess.
func applyMemoryModelEnv(cfg *config.Config) {
	if os.Getenv("MEMORY_WATCHER_MODEL") == "" && cfg.MemoryWatcherModel != "" {
		os.Setenv("MEMORY_WATCHER_MODEL", cfg.MemoryWatcherModel)
	}
	if os.Getenv("MEMORY_EMBED_MODEL") == "" && cfg.MemoryEmbedModel != "" {
		os.Setenv("MEMORY_EMBED_MODEL", cfg.MemoryEmbedModel)
	}
}
