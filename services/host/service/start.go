// start.go implements LAZY AUTO-START of the host services daemon
// (docs/design/serve-lifecycle.md §1): the first `pix run` / `pix memory …`
// spins up a detached `pix-host serve` and waits for its ports, so nobody has to
// run the daemon by hand. One flock serializes the spawn DECISION; the pidfile
// makes it idempotent.

package service

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"pix/host/launcher"
	"pix/host/rpc"
	"pix/host/sys"
	"strconv"
	"strings"
	"time"

	"pix/host/config"
)

// autoserveEnvVar is the env opt-out (config sibling: `host.autoserve = false`).
const autoserveEnvVar = "PIX_NO_AUTOSERVE"

// EnsureTimeout is the default health-wait budget: a cold start opens sqlite
// under an advisory flock, so 15s avoids false failures without hiding a break.
const EnsureTimeout = 15 * time.Second

// EnsureRunTimeout is the SHORTER budget `pix run` uses: a sandbox launch must
// never feel slow because of this (the in-VM extensions retry anyway).
const EnsureRunTimeout = 8 * time.Second

// serveSpawnLockRetry paces Ensure's non-blocking spawn-lock attempts.
const serveSpawnLockRetry = 100 * time.Millisecond

// serveChildHandle is what spawn returns for the just-launched child: enough to
// record its pid, and — ONLY if recordPid then fails — to kill and reap it before
// the spawn lock releases (round 3, H10).
type serveChildHandle struct {
	pid     int
	kill    func() error // SIGKILL the child; used ONLY when recordPid fails
	wait    func() error // reap the child; used ONLY after a kill() following a recordPid failure
	release func()       // detach so the launcher can exit without the child becoming a zombie of ITS exit
}

// serveStarter bundles the injectable OS ops Ensure needs, mirroring serveCtl.
type serveStarter struct {
	hostBin func() (string, error)                                                    // launcher.FindHostBinary
	dial    func(port int) bool                                                       // liveness probe (sys.Real{}.DialLocal)
	spawn   func(bin string, args []string, logPath string) (serveChildHandle, error) // detached spawn -> child handle
	// tryLock takes the spawn lock WITHOUT blocking; (false, nil) = busy, and the
	// caller retries under its deadline so a wedged holder never hangs `pix run`.
	tryLock func(fn func() error) (acquired bool, err error)
	// recordPid writes the child pid to the pidfile BEFORE the spawn lock
	// releases, closing the cold-init double-spawn window (H3).
	recordPid func(pid int) error
	// markLazy writes the serve.lazy marker carrying the spawned PID: mode
	// detection calls a daemon lazy only when the marker MATCHES the live pid (H4).
	markLazy func(pid int)
	ctl      serveCtl                            // REUSE: verify an already-running pid is ours
	sleep    func(d time.Duration)               // poll delay (injected)
	now      func() time.Time                    // clock (injected)
	logPath  func() string                       // config.ServeLogPath
	tailLog  func(path string, lines int) string // last N log lines for error messages
	getenv   func(string) string                 // env (opt-out + port overrides)
	stderr   io.Writer                           // user-facing progress messages
}

// DefaultStarter wires the real OS-backed ops. progress is where the "starting
// pix services…" chatter goes, and it is a PARAMETER: this package cannot know
// whether its caller is answering a --json question. CLI callers pass stderr.
func DefaultStarter(progress io.Writer) serveStarter {
	return serveStarter{
		hostBin:   launcher.FindHostBinary,
		dial:      sys.Real{}.DialLocal,
		spawn:     spawnDetachedServe,
		tryLock:   tryServeSpawnLock,
		recordPid: recordSpawnedServePid,
		markLazy: func(pid int) {
			// Best-effort; a missing marker only downgrades config propagation
			// from "restart the lazy daemon" to "print a note".
			_ = os.WriteFile(config.ServeLazyMarkerPath(), []byte(strconv.Itoa(pid)+"\n"), 0o600)
		},
		ctl:     DefaultCtl(),
		sleep:   time.Sleep,
		now:     time.Now,
		logPath: config.ServeLogPath,
		tailLog: tailFileLines,
		getenv:  os.Getenv,
		stderr:  progress,
	}
}

// recordSpawnedServePid is the launcher-side pidfile write. It RETURNS an error
// (round 2, H8): a swallowed failure would release the spawn lock with no record
// of the child, so the next launcher would spawn a second daemon.
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

// EnsureOpts narrows the wait to the ports a caller actually needs.
type EnsureOpts struct {
	Services []string      // subset to require up; empty = the config `services` set
	Timeout  time.Duration // health-wait budget; 0 = EnsureTimeout
	Quiet    bool          // suppress the "ready" line when nothing was started
}

// errAutoserveDisabled is returned when serve is down and auto-start is opted out.
var errAutoserveDisabled = fmt.Errorf(
	"serve not running and auto-start is disabled (%s / host.autoserve=false) — run `pix serve`", autoserveEnvVar)

// Ensure makes the required services reachable, auto-starting a detached
// `pix-host serve` if needed. Returns nil when the required ports answer
// (already up OR started-and-became-ready), else an error describing why not.
func Ensure(st serveStarter, cfg *config.Config, opts EnsureOpts) error {
	ports := requiredServePorts(st, cfg, opts.Services)
	if len(ports) == 0 {
		return nil // nothing required (e.g. memory asked for but not enabled)
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
		timeout = EnsureTimeout
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
		// Idempotency via pidfile: a live, verified-ours `pix-host serve` means a
		// start is already in progress (process up, port not bound yet) — wait for
		// it instead of spawning a second one.
		if _, ok := readLiveServePid(st.ctl); ok {
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
		// The pidfile write happens BEFORE the lock releases: the next racer to
		// acquire it sees the pid and waits (H3).
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

// requiredServePorts resolves which service ports Ensure must see up: the
// requested subset intersected with the config's enabled set, in requested order.
func requiredServePorts(st serveStarter, cfg *config.Config, requested []string) []servePortSpec {
	enabled := map[string]bool{}
	for _, s := range cfg.Services {
		enabled[s] = true
	}
	if len(requested) == 0 {
		requested = cfg.Services
	}
	env := sys.GetenvFunc(st.getenv)
	var out []servePortSpec
	seen := map[string]bool{}
	for _, s := range requested {
		if !enabled[s] || seen[s] {
			continue
		}
		seen[s] = true
		if s == "memory" {
			out = append(out, servePortSpec{"memory", Port(env, "MEMORY_PORT", rpc.MemoryPortDefault)})
		}
	}
	return out // requested order preserved ("memory:11435")
}

// describeServePorts renders "memory:11435" for messages.
func describeServePorts(ports []servePortSpec) string {
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		parts = append(parts, fmt.Sprintf("%s:%d", p.name, p.port))
	}
	return strings.Join(parts, ", ")
}

// readLiveServePid reports (pid, true) only for a live, VERIFIED-ours `pix-host
// serve`: an unverifiable pid counts as NOT running, so the start path would
// rather risk a serialized second spawn than adopt a stranger as our daemon.
func readLiveServePid(ctl serveCtl) (int, bool) {
	if pr := probeServePid(ctl); pr.isOurs() {
		return pr.pid, true
	}
	return 0, false
}

// readServeLazyMarkerPid parses the pid out of the serve.lazy marker. ok=false
// for a missing, unparseable, or legacy-format ("lazy\n") marker — mode
// detection then treats the daemon as FOREGROUND, the conservative direction
// (advise the user instead of signalling something we don't own).
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

// tailFileLines returns the last n lines of a file ("" when unreadable), folding
// the daemon's own words into a health-wait timeout error. The read refuses a
// symlink via O_NOFOLLOW, with no lstat-then-open window (H1/H8).
func tailFileLines(path string, n int) string {
	b, err := ReadFileNoSymlink(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// EnsureUp is the best-effort call-site helper `run` and `memory` use: load the
// base config, run Ensure with the real ops, and swallow the error (Ensure told
// the user why on progress) so the primary action proceeds. progress is the
// caller's stderr, never its stdout.
func EnsureUp(progress io.Writer, services []string, timeout time.Duration) {
	cfg, err := config.Load()
	if err != nil {
		return // a broken config fails loudly in the primary action instead
	}
	if from, stale := staleServeVersion(cfg, sys.Real{}, services, rpc.IdentityProbe); stale {
		restartStaleServe(DefaultReloader(progress), from, launcher.Version, progress)
	}
	_ = Ensure(DefaultStarter(progress), cfg, EnsureOpts{Services: services, Timeout: timeout})
}

// staleServeVersion recognizes ONLY a positively identified Pix service at a
// different launcher.Version; a foreign or mute port holder is never restarted.
func staleServeVersion(cfg *config.Config, env portProbe, requested []string, probe rpc.IdentityProber) (string, bool) {
	if cfg == nil || probe == nil {
		return "", false
	}
	st := serveStarter{getenv: env.Getenv}
	for _, p := range requiredServePorts(st, cfg, requested) {
		if !env.DialLocal(p.port) {
			continue
		}
		id, err := probe(p.port)
		want := rpc.MemoryName
		if err == nil && id.Name == want && id.Version != "" && id.Version != launcher.Version {
			return id.Version, true
		}
	}
	return "", false
}

// restartStaleServe preserves lifecycle ownership: managed restarts through its
// supervisor, lazy stops safely and relaunches, foreground is left to its
// terminal owner. serveDown shares the lazy path deliberately — an unrecorded
// orphan (no pidfile, no marker) was still PROVEN to be Pix by the identity
// probe, so it is recycled the same safe way.
func restartStaleServe(rl serveReloader, from, to string, out io.Writer) {
	switch mode := rl.mode(); mode {
	case serveManaged:
		if err := rl.kickManaged(); err != nil {
			fmt.Fprintf(out, "warning: could not update pix services from %s to %s: %v\n", from, to, err)
			return
		}
		fmt.Fprintf(out, "updated pix services %s → %s.\n", from, to)
	case serveForeground:
		fmt.Fprintf(out, "pix services %s are running in another terminal; restart them to use %s.\n", from, to)
	case serveLazy, serveDown:
		switch r := relazyServe(rl); {
		case r.stopErr != nil || r.notStopped:
			fmt.Fprintf(out, "warning: could not safely stop pix services %s — run: pix serve stop && pix serve\n", from)
		case r.startErr != nil:
			fmt.Fprintf(out, "warning: pix services stopped but %s did not start: %v\n", to, r.startErr)
		default:
			fmt.Fprintf(out, "updated pix services %s → %s.\n", from, to)
		}
	}
}
