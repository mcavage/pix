// serve_start.go implements LAZY AUTO-START of the host services daemon
// (docs/design/serve-lifecycle.md §1): the first `pix run` / `pix
// memory …` / `pix knowledge query` spins up a detached `pix-host
// serve` if the configured service ports are down — the Ollama-CLI model
// (`ollama run` starts `ollama serve`), so nobody babysits a foreground serve.
//
// Backdoor-shape note (AGENTS.md HOST convention): the spawn trigger here is a
// USER-invoked CLI command run in the user's own login session, spawning the
// user's OWN already-installed `pix-host serve`. There is no network input
// in the trigger path — nothing listening on a socket decides to spawn. What
// AGENTS.md prohibits is a *listening* process that forks on a received
// request; this is a foreground CLI forking a sibling binary. Different shape.
//
// Every OS boundary (spawn, dial, flock, clock) is injected via serveStarter so
// the whole flow is unit-testable with NO real process spawn. The real
// detached-spawn + flock shims live in serve_start_unix.go (//go:build unix);
// serve_start_windows.go degrades gracefully on everything else.

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"pix/host/config"
)

// autoserveEnvVar is the env opt-out: any non-empty value disables lazy
// auto-start (the config sibling is `host.autoserve = false`; env wins).
const autoserveEnvVar = "PIX_NO_AUTOSERVE"

// ensureServeTimeout is the default health-wait budget: memory opens a sqlite
// store under an advisory flock and knowledge reindexes bundles at startup, so
// a cold start can take a few seconds; 15s avoids false failures while a truly
// broken start (port permanently in use) still fails fast.
const ensureServeTimeout = 15 * time.Second

// ensureServeRunTimeout is the SHORTER budget `pix run` uses: a sandbox
// launch must never feel slow because of this feature (the in-VM extensions
// degrade gracefully and retry over host.docker.internal anyway).
const ensureServeRunTimeout = 8 * time.Second

// serveSpawnLockRetry is how long ensureServe waits between non-blocking
// attempts on the spawn lock (the same cadence as the health-wait poll).
const serveSpawnLockRetry = 100 * time.Millisecond

// serveChildHandle is what spawn returns for the just-launched child: enough
// to record its pid, and — ONLY if recordPid then fails — to kill and reap it
// before the spawn lock releases (round 3, H10). release() detaches the
// launcher from the child (the unix impl's Process.Release, deferred until
// AFTER recordPid succeeds — a released process can no longer be Wait()ed, so
// releasing early was exactly what let a recordPid failure leak a zombie /
// leave an unkillable-by-Wait child on a failed kill). kill()/wait() are only
// ever called on the recordPid-failure path, before release() has run.
type serveChildHandle struct {
	pid     int
	kill    func() error // SIGKILL the child; used ONLY when recordPid fails
	wait    func() error // reap the child; used ONLY after a kill() following a recordPid failure
	release func()       // detach so the launcher can exit without the child becoming a zombie of ITS exit
}

// serveStarter bundles the injectable OS ops ensureServe needs, mirroring
// serveCtl. defaultServeStarter() wires the real ops; tests substitute fakes.
type serveStarter struct {
	hostBin func() (string, error)                                                    // findHostBinary
	dial    func(port int) bool                                                       // liveness probe (dialLocalPort)
	spawn   func(bin string, args []string, logPath string) (serveChildHandle, error) // detached spawn -> child handle
	// tryLock attempts the spawn lock WITHOUT blocking: (false, nil) means the
	// lock is busy (another launcher is mid-spawn) — the caller retries under
	// its own deadline, so a wedged lock-holder can never hang `pix run`
	// past the health budget (M2).
	tryLock func(fn func() error) (acquired bool, err error)
	// recordPid writes the freshly-spawned child pid to the serve pidfile
	// BEFORE the spawn lock is released, closing the cold-init double-spawn
	// window (H3): the daemon itself writes serve.pid only after config load +
	// store open + indexing, and a second caller acquiring the lock in that gap
	// used to see no pidfile and fork a second daemon. The daemon later
	// overwrites the file with the SAME pid. It RETURNS an error (round 2, H8):
	// a discarded MkdirAll/WriteFile failure used to let the lock release as if
	// the pid had landed, reopening the exact double-spawn window it exists to
	// close. ensureServe treats a non-nil error as a FAILED spawn — it kills the
	// just-recorded child and returns the error instead of releasing the lock as
	// success.
	recordPid func(pid int) error
	// markLazy writes the serve.lazy marker carrying the spawned PID, so mode
	// detection only classifies a daemon as lazy when the marker pid MATCHES the
	// live verified pidfile pid — a stale marker from a crash can never get a
	// later foreground daemon killed by config propagation (H4).
	markLazy func(pid int)
	ctl      serveCtl                            // REUSE: verify an already-running pid is ours
	sleep    func(d time.Duration)               // poll delay (injected)
	now      func() time.Time                    // clock (injected)
	logPath  func() string                       // config.ServeLogPath
	tailLog  func(path string, lines int) string // last N log lines for error messages
	getenv   func(string) string                 // env (opt-out + port overrides)
	stderr   io.Writer                           // user-facing progress messages
}

// defaultServeStarter wires the real OS-backed ops.
func defaultServeStarter() serveStarter {
	return serveStarter{
		hostBin:   findHostBinary,
		dial:      dialLocalPort,
		spawn:     spawnDetachedServe,
		tryLock:   tryServeSpawnLock,
		recordPid: recordSpawnedServePid,
		markLazy: func(pid int) {
			// Best-effort; a missing marker only downgrades config propagation
			// from "restart the lazy daemon" to "print a note".
			_ = os.WriteFile(config.ServeLazyMarkerPath(), []byte(strconv.Itoa(pid)+"\n"), 0o600)
		},
		ctl:     defaultServeCtl(),
		sleep:   time.Sleep,
		now:     time.Now,
		logPath: config.ServeLogPath,
		tailLog: tailFileLines,
		getenv:  os.Getenv,
		stderr:  os.Stderr,
	}
}

// recordSpawnedServePid is the launcher-side pidfile write (see
// serveStarter.recordPid). It RETURNS an error (round 2, H8): a swallowed
// MkdirAll/WriteFile failure used to let ensureServe's spawn lock release as
// if the pid had been recorded, reopening the H3 double-spawn window with no
// backstop for knowledge-only configs (the memory store's own flock does not
// exist when memory is disabled).
func recordSpawnedServePid(pid int) error {
	path := config.ServePidPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create pidfile dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		return fmt.Errorf("write pidfile %s: %w", path, err)
	}
	return nil
}

// ensureServeOpts lets a caller narrow the wait to the ports it actually needs
// (run needs the whole config set; `memory recall` only needs memory).
type ensureServeOpts struct {
	Services []string      // subset to require up; empty = the config `services` set
	Timeout  time.Duration // health-wait budget; 0 = ensureServeTimeout
	Quiet    bool          // suppress the "ready" line when nothing was started
}

// errAutoserveDisabled is returned when serve is down and auto-start is opted
// out (env or config); callers degrade exactly as before this feature existed.
var errAutoserveDisabled = fmt.Errorf(
	"serve not running and auto-start is disabled (%s / host.autoserve=false) — run `pix serve`", autoserveEnvVar)

// ensureServe makes the required services reachable, auto-starting a detached
// `pix-host serve` if needed. Returns nil when the required ports answer
// (already up OR started-and-became-ready), or an error describing why it could
// not (spawn failed / timed out / opted out and down). The WHOLE flow —
// including spawn-lock acquisition — is bounded by one deadline, and it prints
// its own user-facing progress/failure lines to st.stderr so callers can treat
// the error as best-effort.
func ensureServe(st serveStarter, cfg *config.Config, opts ensureServeOpts) error {
	ports := requiredServePorts(st, cfg, opts.Services)
	if len(ports) == 0 {
		return nil // nothing required (e.g. knowledge asked for but not enabled)
	}
	allUp := func() bool {
		for _, p := range ports {
			if !st.dial(p.port) {
				return false
			}
		}
		return true
	}
	if allUp() {
		return nil // fast path: silent when already up
	}

	// Opt-out gate: probe said down, and we may not spawn.
	if st.getenv(autoserveEnvVar) != "" || !cfg.AutoserveEnabled() {
		fmt.Fprintf(st.stderr, "%v\n", errAutoserveDisabled)
		return errAutoserveDisabled
	}

	// ONE deadline bounds both the lock acquisition below and the health-wait
	// after it (M2): a wedged lock-holder degrades into the same legible timeout
	// as a daemon that never becomes healthy.
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = ensureServeTimeout
	}
	deadline := st.now().Add(timeout)

	spawned := false
	waiting := false
	criticalSection := func() error {
		// Double-checked locking: a racing process may have started serve between
		// the fast-path probe and the lock.
		if allUp() {
			return nil
		}
		// Idempotency via pidfile: a live, verified-ours `pix-host serve`
		// means a start is already in progress (process up, port not bound yet) —
		// wait for it instead of spawning a second one. Reuses serve_ctl's
		// ownership check verbatim. The launcher's recordPid write below is what
		// makes this catch a cold-init racer (H3).
		if pid, ok := readLiveServePid(st.ctl); ok {
			_ = pid
			waiting = true
			return nil
		}
		bin, err := st.hostBin()
		if err != nil {
			return fmt.Errorf("could not start pix services: %v. run `pix serve` to see the error", err)
		}
		fmt.Fprintf(st.stderr, "starting pix services (%s)…\n", describeServePorts(ports))
		handle, err := st.spawn(bin, []string{"serve"}, st.logPath())
		if err != nil {
			return fmt.Errorf("could not start pix services: %v. run `pix serve` to see the error", err)
		}
		// BOTH writes happen before the lock releases: a racer that acquires the
		// lock next sees the pidfile (readLiveServePid above) and waits (H3), and
		// the lazy marker carries the pid for stale-marker-proof detection (H4).
		// recordPid failing must NOT release the lock as success (round 2, H8): a
		// racer would then see no pidfile AND a still-alive unrecorded child, and
		// spawn a second daemon. Round 3 (H10): the spawn seam holds off
		// Process.Release() until recordPid succeeds, so on failure we can still
		// Kill() AND Wait()/reap the child here — a released process can't be
		// reaped, which used to leave a zombie (or, worse, an unverified survivor
		// if the signal itself failed). A failed Kill() is NOT swallowed: it is
		// folded into the returned error so the launch fails loudly instead of the
		// lock releasing as if cleanup had succeeded.
		if err := st.recordPid(handle.pid); err != nil {
			if killErr := handle.kill(); killErr != nil {
				return fmt.Errorf("spawned pix services (pid %d) but could not record its pidfile (%v), AND could not kill it to avoid a double-spawn race (%v) — a second daemon must not be started until pid %d is confirmed dead", handle.pid, err, killErr, handle.pid)
			}
			_ = handle.wait() // reap: not yet released, so this is a real wait on our own child
			return fmt.Errorf("spawned pix services (pid %d) but could not record its pidfile, so it was killed and reaped to avoid a double-spawn race: %v", handle.pid, err)
		}
		handle.release()
		st.markLazy(handle.pid)
		spawned = true
		return nil
	}
	for {
		acquired, err := st.tryLock(criticalSection)
		if err != nil {
			fmt.Fprintf(st.stderr, "%v\n", err)
			return err
		}
		if acquired {
			break
		}
		if !st.now().Before(deadline) {
			msg := fmt.Sprintf("pix services did not become ready in %s (another process holds the start lock %s)",
				timeout, config.ServeSpawnLockPath())
			fmt.Fprintln(st.stderr, msg)
			return fmt.Errorf("%s", msg)
		}
		st.sleep(serveSpawnLockRetry)
	}

	// Health-wait: poll the required ports until all-up or the budget elapses.
	for {
		if allUp() {
			if (spawned || waiting) && !opts.Quiet {
				fmt.Fprintln(st.stderr, "pix services ready")
			}
			return nil
		}
		if !st.now().Before(deadline) {
			logPath := st.logPath()
			msg := fmt.Sprintf("pix services did not become ready in %s", timeout)
			if tail := st.tailLog(logPath, 10); tail != "" {
				msg += ". last log lines:\n" + tail
			}
			msg += "\nsee logs: " + logPath
			fmt.Fprintln(st.stderr, msg)
			return fmt.Errorf("%s", msg)
		}
		st.sleep(200 * time.Millisecond)
	}
}

// servePortSpec names one required service port for probing + messages.
type servePortSpec struct {
	name string
	port int
}

// requiredServePorts resolves which service ports ensureServe must see up:
// the caller's requested subset (or the whole config set), intersected with the
// ENABLED services — a disabled service is never required — mapped to ports via
// the same MEMORY_PORT/KNOWLEDGE_PORT-aware resolution serve_ctl uses.
func requiredServePorts(st serveStarter, cfg *config.Config, requested []string) []servePortSpec {
	enabled := map[string]bool{}
	for _, s := range cfg.Services {
		enabled[s] = true
	}
	if len(requested) == 0 {
		requested = cfg.Services
	}
	env := shellEnv{getenv: st.getenv}
	var out []servePortSpec
	seen := map[string]bool{}
	for _, s := range requested {
		if !enabled[s] || seen[s] {
			continue
		}
		seen[s] = true
		switch s {
		case "memory":
			out = append(out, servePortSpec{"memory", servePort(env, "MEMORY_PORT", memoryPortDefault)})
		case "knowledge":
			out = append(out, servePortSpec{"knowledge", servePort(env, "KNOWLEDGE_PORT", knowledgePortDefault)})
		}
	}
	return out // requested order preserved ("memory:11435, knowledge:11436")
}

// describeServePorts renders "memory:11435, knowledge:11436" for messages.
func describeServePorts(ports []servePortSpec) string {
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		parts = append(parts, fmt.Sprintf("%s:%d", p.name, p.port))
	}
	return strings.Join(parts, ", ")
}

// readLiveServePid reads the pidfile through ctl and reports (pid, true) only
// for a live, VERIFIED-ours `pix-host serve` — the same refuse-unless-sure
// posture as `serve stop` (an unverifiable pid is treated as not-running so we
// rely on the daemon's own store flock as the correctness backstop).
func readLiveServePid(ctl serveCtl) (int, bool) {
	raw, err := ctl.readPid(ctl.pidPath())
	if err != nil {
		return 0, false
	}
	pid := 0
	if _, err := fmt.Sscanf(strings.TrimSpace(raw), "%d", &pid); err != nil || pid <= 0 {
		return 0, false
	}
	if ctl.kill(pid, 0) != nil {
		return 0, false
	}
	ours, known := ctl.verify(pid)
	if !known || !ours {
		return 0, false
	}
	return pid, true
}

// readServeLazyMarkerPid parses the pid out of the serve.lazy marker. ok=false
// for a missing, unparseable, or legacy-format ("lazy\n") marker — mode
// detection then treats the daemon as FOREGROUND, the conservative direction
// (config propagation advises instead of killing).
func readServeLazyMarkerPid() (pid int, ok bool) {
	b, err := os.ReadFile(config.ServeLazyMarkerPath())
	if err != nil {
		return 0, false
	}
	pid, perr := strconv.Atoi(strings.TrimSpace(string(b)))
	if perr != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// tailFileLines returns the last n lines of a file ("" when unreadable/empty),
// used to fold the daemon's own words into a health-wait timeout error. It
// REFUSES to read through a symlink (H1). Round 2 (H8) closes a TOCTOU: the
// original Lstat-then-ReadFile left a window where swapping the log for a
// symlink AFTER the Lstat made it read+echo an arbitrary file. readFileNoSymlink
// (serve_start_unix.go / serve_start_windows.go) opens with O_NOFOLLOW so the
// open itself atomically refuses a symlink — there is no separate check to race.
func tailFileLines(path string, n int) string {
	b, err := readFileNoSymlink(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// ensureServeUp is the best-effort call-site helper `run`, `memory`, and
// `knowledge` use: load the base config, run ensureServe with the real ops, and
// swallow the error (ensureServe already told the user why on stderr) so the
// primary action proceeds and degrades exactly as it did before lazy start.
func ensureServeUp(services []string, timeout time.Duration) {
	cfg, err := config.Load()
	if err != nil {
		return // a broken config fails loudly in the primary action instead
	}
	if from, stale := staleServeVersion(cfg, defaultShellEnv(), services, rpcIdentityProbe); stale {
		restartStaleServe(defaultServeReloader(), from, version, os.Stderr)
	}
	_ = ensureServe(defaultServeStarter(), cfg, ensureServeOpts{Services: services, Timeout: timeout})
}

// staleServeVersion recognizes only a positively identified Pix service at a
// different version. Foreign/unresponsive port holders are never restarted.
func staleServeVersion(cfg *config.Config, env shellEnv, requested []string, probe identityProber) (string, bool) {
	if cfg == nil || probe == nil {
		return "", false
	}
	st := serveStarter{getenv: env.getenv}
	for _, p := range requiredServePorts(st, cfg, requested) {
		if env.dial != nil && !env.dial(p.port) {
			continue
		}
		id, err := probe(p.port)
		want := identityMemoryName
		if p.name == "knowledge" {
			want = identityKnowledgeName
		}
		if err == nil && id.Name == want && id.Version != "" && id.Version != version {
			return id.Version, true
		}
	}
	return "", false
}

// restartStaleServe preserves lifecycle ownership: managed services restart
// through their supervisor, lazy services stop safely and relaunch, and a
// foreground process is left for its terminal owner.
func restartStaleServe(rl serveReloader, from, to string, out io.Writer) {
	switch rl.mode() {
	case serveManaged:
		if err := rl.kickManaged(); err != nil {
			fmt.Fprintf(out, "warning: could not update pix services from %s to %s: %v\n", from, to, err)
			return
		}
		fmt.Fprintf(out, "updated pix services %s → %s.\n", from, to)
	case serveLazy:
		stopped, err := rl.stopServe(io.Discard)
		if err != nil || !stopped {
			fmt.Fprintf(out, "warning: could not safely stop pix services %s — run: pix serve stop && pix serve\n", from)
			return
		}
		if err := rl.ensure(); err != nil {
			fmt.Fprintf(out, "warning: pix services stopped but %s did not start: %v\n", to, err)
			return
		}
		fmt.Fprintf(out, "updated pix services %s → %s.\n", from, to)
	case serveForeground:
		fmt.Fprintf(out, "pix services %s are running in another terminal; restart them to use %s.\n", from, to)
	}
}
