// `serve` is the plugin supervisor. It runs the long-running HTTP host services
// (memory :11435), resolving each capability slot from config. Plugins never
// bind ports — the supervisor owns the listeners and the stable host surface
// every sandbox depends on. MCP servers are stdio and spawned on demand by the
// sbx gateway, not here.
//
// Startup is four strict phases: bindFrontDoors (every front door binds FIRST,
// so a port conflict fails with zero subprocesses ever launched), then serve
// those listeners at once behind a starting-up handler (swapHandler — a bound
// port is never open behind nothing, and this still spawns nothing, so phase
// 1's guarantee holds), then spawnMemory, then reconcilePacks LAST because a
// pack daemon's preflight can take 15s and memory must not wait on it.
//
// Shutdown mirrors that (performShutdown): drain every front door first with a
// bounded context, then
// tear down the supervised backend — and no exit path calls os.Exit until that
// full sequence has returned, so exit-1 semantics on a fatal error are
// preserved but never early.
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
	"sync/atomic"
	"syscall"
	"time"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/packinfo"
	"pix/host/sys"
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
	// h is live from the instant the port is bound, replaced in place once the
	// supervised unit is up. See swapHandler.
	h *swapHandler
}

// swapHandler lets a listener be SERVED from the instant it binds, behind
// startingMux, with the real mux swapped in once the unit is dispensed.
//
// It exists because the port used to lie: http.Serve was called only after the
// child-spawn phase returned, so for that whole phase the kernel accepted
// connections into the backlog of a listener no goroutine read from. The port
// was OPEN and every request HUNG — a TCP dial says "up", `identity` says
// "unreachable", and no caller can tell a starting daemon from a wedged one.
// Measured: TCP answered at 0.26s, HTTP at 15.22s (one pack daemon's preflight
// held the phase), against install's 10s verification budget, so `pix serve
// install` ALWAYS reported a healthy daemon as unreachable.
// A request arriving BEFORE the swap is answered by KIND, and getting that wrong
// broke uatmatrix. A probe is answered NOW (a probe that blocks is the original
// bug); real work WAITS, because that is what it effectively did before this file
// served early — the POST sat in an unaccepted backlog and then SUCCEEDED, and
// answering "not ready" instead turned a sure thing into a coin flip. Waiting is
// the default: only a recognised probe skips it.
type swapHandler struct {
	starting http.Handler                 // answers probes until real arrives
	real     atomic.Pointer[http.Handler] // the unit's own mux, once dispensed
	ready    chan struct{}                // closed on the first set
	once     sync.Once
}

func newSwapHandler(starting http.Handler) *swapHandler {
	return &swapHandler{starting: starting, ready: make(chan struct{})}
}

// set installs the unit's real mux and releases everything waiting on ready.
func (s *swapHandler) set(h http.Handler) {
	s.real.Store(&h)
	s.once.Do(func() { close(s.ready) })
}

func (s *swapHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h := s.real.Load(); h != nil {
		(*h).ServeHTTP(w, r)
		return
	}
	if isIdentityCall(r) {
		s.starting.ServeHTTP(w, r)
		return
	}
	// Bounded by the REQUEST's context and nothing else, exactly as the backlog
	// bounded it before. A grace timer would only invent a second deadline,
	// shorter than the caller's own.
	select {
	case <-s.ready:
	case <-r.Context().Done():
		return
	}
	(*s.real.Load()).ServeHTTP(w, r)
}

// frontDoorShutdownTimeout bounds how long a front-door http.Server is given to
// drain its in-flight requests once shutdown starts, before Close() forces it,
// so no front door can wedge shutdown.
const frontDoorShutdownTimeout = 5 * time.Second

// serveUsage documents the enabled-service positionals runServe parses.
// memory's bind/port stay env-only via MEMORY_BIND/MEMORY_PORT, so there are no
// flags left: --bind/--port belonged to the retired monitor ingest and are now a
// usage error, like every other retired capability. `pix serve` intercepts
// -h/--help and prints service.Usage instead; this text is for a direct
// `pix-host serve -h`.
const serveUsage = `usage: pix-host serve [service...]

  Run the long-running host services: memory (:11435). No service names
  given: run every service in ` + "`services`" + ` (config.toml), or all of
  them if that is also unset.
`

// serveServiceAliases is the config name -> internal service name table: the
// WHOLE set of capabilities `serve` composes. A retired capability leaves here,
// which is what makes `serve <retired>` a usage error, not a started daemon.
func serveServiceAliases() map[string]string {
	return map[string]string{"memory": "memory"}
}

func runServe(argv []string) {
	fs := cli.NewFlagSet()
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
	// Populated in Phase 2, read by fatalf; both on THIS goroutine only, so the
	// read needs no synchronization.
	var httpServers []*http.Server
	// fatalf routes every fatal exit through the SAME sequence the signal path
	// uses: drain the serving front doors, then the backend. A plain log.Fatalf
	// skips both and orphans a launched plugin subprocess. Both calls no-op on an
	// empty list / no running unit, so this is correct in ANY phase.
	fatalf := func(format string, a ...any) {
		log.Printf("serve: "+format, a...)
		performShutdown(httpServers, sup.shutdown)
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
	all, berr := bindFrontDoors(enabledSvc)
	if berr != nil {
		fatalf("%v", berr)
	}
	if len(all) == 0 {
		fatalf("no services enabled (run: pix config set services %s)", strings.Join(valid, ","))
	}

	// fatalCh carries service-goroutine errors (e.g. port already in use) back to
	// the main goroutine, and sigCh carries SIGINT/SIGTERM: both are handled in the
	// select below rather than by a side goroutine calling os.Exit, so the deferred
	// cleanup (pidfile, lazy marker) actually runs.
	fatalCh := make(chan error, len(all)+2)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Phase 2: START SERVING every bound front door, BEFORE any child is spawned,
	// so a bound port never accepts a connection nothing will read — it answers an
	// honest ready=false identity while its unit comes up (swapHandler). Spawns
	// nothing, so Phase 1's bind-before-spawn guarantee is untouched.
	httpServers = make([]*http.Server, 0, len(all))
	for _, s := range all {
		s := s
		// Bound HTTP servers: ReadTimeout prevents slow-loris header floods,
		// WriteTimeout caps long-running handlers. Synthesis is the ~60s ceiling.
		srv := &http.Server{
			Addr:         s.addr,
			Handler:      s.h,
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

	// Phase 3: spawn the memory plugin child and swap its real mux in behind the
	// front door. Reached only once every bind succeeded, so nothing can orphan.
	spawnMemory(cfg, sup, selfPath, all, fatalf)

	// Record our pid so the launcher's `serve stop`/`serve status` can find and
	// signal us safely (no blind pkill).
	writeServePidFile()
	defer removeServePidFile()
	defer removeServeLazyMarker()

	// Phase 4: pack units, LAST and deliberately OFF the readiness path. A pack
	// daemon whose port a foreign process holds (a leaked orphan, say) burns its
	// whole 15s preflight discovering that, and used to burn it BEFORE memory
	// could answer at all. Memory's readiness must not depend on any pack's.
	reconcilePacks(cfg, sup, selfPath)

	// Block until a signal or a service failure. Both paths run the SAME fixed
	// shutdown order: drain every front door first, then the supervised
	// backend. Neither path calls os.Exit until that full sequence has actually
	// returned, so a fatal exit never skips the joined cleanup — exit 1 is
	// preserved, just never early.
	select {
	case sig := <-sigCh:
		log.Printf("serve: received %v; shutting down", sig)
		performShutdown(httpServers, sup.shutdown)
		// defers (removeServePidFile, removeServeLazyMarker) run on return
	case err := <-fatalCh:
		log.Printf("serve: fatal: %v", err)
		performShutdown(httpServers, sup.shutdown)
		// Explicitly run deferred cleanup before os.Exit since defers don't run
		// when Exit is called (defers only run on return from runServe).
		removeServePidFile()
		removeServeLazyMarker()
		os.Exit(1)
	}
}

// bindFrontDoors binds every built-in HTTP front door — memory's :11435
// listener — and nothing else: it has no `*supervisor`, no packinfo, no way to
// spawn a subprocess. That is the whole point: a port conflict fails here,
// before runServe has any chance to spawn a pack unit or the memory plugin
// child, so the failure can never leave one orphaned. Called with a NON-nil
// error the result is always nil, so a caller can never act on a half-bound
// set.
func bindFrontDoors(enabledSvc func(string) bool) ([]hostService, error) {
	var all []hostService
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
			return nil, fmt.Errorf("bind memory (%s): %w", addr, lerr)
		}
		// Answerable the moment it is bound: until spawnMemory dispenses the unit,
		// `identity` reports the right name and version with ready=false, never a hang.
		all = append(all, hostService{name: "memory", addr: addr, ln: ln,
			h: newSwapHandler(startingMux(identityMemory, servicePort("MEMORY_PORT", 11435)))})
	}

	return all, nil
}

// spawnMemory launches the memory plugin unit and swaps its real JSON-RPC mux in
// behind the already-serving front door. The ONLY place that child is spawned;
// called exactly once, and only AFTER bindFrontDoors succeeded, so a bind
// failure can never orphan it. It runs BEFORE reconcilePacks: memory is what
// every `pix run`, `pix memory …` and readiness probe blocks on, so nothing a
// pack does may sit between the bind and the first answerable request.
func spawnMemory(cfg *config.Config, sup *supervisor, selfPath string, all []hostService, fatalf func(string, ...any)) {
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
		// The starting-up handler is replaced here, and only here: from this
		// point the port answers ready=true.
		all[i].h.set(memoryProxyMux(h))
	}
}

// reconcilePacks brings every active pack's Tier-1-accepted [[services]] units
// — daemons first, then go-plugin units — to their desired state. The ONLY place
// a pack child is spawned; never returns an error (one pack's breakage must not
// stop serve or its siblings); runs LAST (see Phase 4 in runServe).
func reconcilePacks(cfg *config.Config, sup *supervisor, selfPath string) {
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
		views, verr := pack.AcceptedGoPluginServices(p)
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
	// Daemons before go-plugin units: a pack's daemon is what the SANDBOX reaches
	// (snow-proxy on its loopback port), so a session starting while it comes up
	// gets connection-refused on its first query. Failures are logged, never
	// fatal — one broken daemon must not stop serve, a sibling, or (now) memory.
	if derr := sup.reconcileDaemons(selfPath, merged); derr != nil {
		log.Printf("serve: pack daemons: %v", derr)
	}
	units, rerr := sup.reconcilePackUnits(selfPath, merged)
	if rerr != nil {
		log.Printf("serve: pack services: %v", rerr)
	}
	if len(units) > 0 {
		log.Printf("serve: supervising %d pack service(s): %s", len(units), strings.Join(slices.Sorted(maps.Keys(units)), ", "))
	}
}

// performShutdown is the ONE shutdown sequence every exit path — signal,
// fatal service error — runs, in a fixed order: drain every front door FIRST
// (stop accepting new connections immediately, then wait out any in-flight
// request bounded by frontDoorShutdownTimeout — see shutdownFrontDoors), THEN
// tear down the supervised backend (pack units + the memory plugin). Getting
// this backwards — tearing the backend down while a front door still accepts
// — lets a brand-new request land on a backend that is already mid-teardown.
func performShutdown(httpServers []*http.Server, shutdownBackend func()) {
	shutdownFrontDoors(httpServers)
	shutdownBackend()
}

// shutdownFrontDoors stops accepting new connections and drains every
// mux-based front-door http.Server, bounded by frontDoorShutdownTimeout so a
// stuck handler cannot hang shutdown forever. It returns once every
// http.Server's Shutdown call has returned.
func shutdownFrontDoors(servers []*http.Server) {
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

// writeServePidFile records the current pid at config.ServePidPath() (0600, dir
// 0700). Best-effort: a failed MkdirAll/write only logs, never crashes serve. A
// stale pidfile from a previous crash is overwritten — the live pid is authority.
// The write is taken under the SAME sys.Lock as removeOwnedPidFile's
// compare-and-delete (config.PidFileLockPath), so a just-exiting old owner's
// cleanup and this respawn's write can never interleave: see removeOwnedPidFile.
func writeServePidFile() {
	writeServePidFileAt(config.ServePidPath(), os.Getpid())
}

// writeServePidFileAt is writeServePidFile's real body, parameterized so a
// test can point it at a temp path and a synthetic pid (standing in for a
// different, respawned process) without touching config.ServePidPath().
func writeServePidFileAt(path string, pid int) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		log.Printf("serve: could not create pidfile dir %s: %v", filepath.Dir(path), err)
		return
	}
	lockPath := config.PidFileLockPath(path)
	if err := sys.Lock(lockPath, func() error {
		return os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o600)
	}); err != nil {
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
// currently-running daemon) just wrote for itself.
//
// The read-compare-delete runs INSIDE one sys.Lock hold on
// config.PidFileLockPath(path) — a STABLE sibling path, never the pidfile
// inode itself (which gets removed and recreated across a respawn, so
// locking it directly would be the exact TOCTOU this function used to have:
// a compare that reads true, then a respawned daemon overwrites the file,
// then a stale Remove deletes the NEW owner's file out from under it).
// writeServePidFile/writeServePidFileAt and the launcher's
// recordSpawnedServePid/markLazy take the SAME lock around their writes, so
// the two sides can never interleave: whichever gets the lock first runs its
// whole compare-delete or whole write to completion before the other even
// starts. A missing file is fine; any other read/remove/lock error only logs
// (best-effort — cleanup must never crash serve on its way out).
func removeOwnedPidFile(path string) {
	removeOwnedPidFileWithHook(path, nil)
}

// removeOwnedPidFileWithHook is removeOwnedPidFile's real body. afterOwnershipConfirmed,
// when non-nil, runs AFTER the ownership check succeeds but BEFORE os.Remove —
// while the lock is STILL held. Production always calls removeOwnedPidFile,
// which passes nil; the only other caller is
// TestRemoveOwnedPidFileSerializesAgainstConcurrentWrite, which uses the hook
// to deterministically pause an "old owner" mid-critical-section and prove a
// concurrent respawn's write genuinely blocks on the same lock rather than
// racing it.
func removeOwnedPidFileWithHook(path string, afterOwnershipConfirmed func()) {
	lockPath := config.PidFileLockPath(path)
	if err := sys.Lock(lockPath, func() error {
		raw, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				log.Printf("serve: could not read %s before removing: %v", path, err)
			}
			return nil
		}
		pid, perr := strconv.Atoi(strings.TrimSpace(string(raw)))
		if perr != nil || pid != os.Getpid() {
			// Not ours (unparseable, or another process's pid): leave it exactly
			// alone. Whoever it belongs to is responsible for its own cleanup.
			return nil
		}
		if afterOwnershipConfirmed != nil {
			afterOwnershipConfirmed()
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Printf("serve: could not remove %s: %v", path, err)
		}
		return nil
	}); err != nil {
		log.Printf("serve: could not acquire lock %s: %v", lockPath, err)
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

// applyMemoryModelEnv wires the configured memory model names AND the capture
// admission mode into the env the memory unit inherits (memembed.go reads the
// models, memory_capture_mode.go reads MEMORY_CAPTURE_MODE), so an explicit
// env override wins and the config value applies otherwise. Set only in the
// memory branch so none of this leaks into an unrelated plugin subprocess.
func applyMemoryModelEnv(cfg *config.Config) {
	if os.Getenv("MEMORY_WATCHER_MODEL") == "" && cfg.MemoryWatcherModel != "" {
		os.Setenv("MEMORY_WATCHER_MODEL", cfg.MemoryWatcherModel)
	}
	if os.Getenv("MEMORY_EMBED_MODEL") == "" && cfg.MemoryEmbedModel != "" {
		os.Setenv("MEMORY_EMBED_MODEL", cfg.MemoryEmbedModel)
	}
	if os.Getenv("MEMORY_CAPTURE_MODE") == "" && cfg.MemoryCapture != "" {
		os.Setenv("MEMORY_CAPTURE_MODE", cfg.MemoryCapture)
	}
}
