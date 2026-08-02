// serve_ctl.go implements the launcher-side control verbs `serve stop` and
// `serve status`, the SAFE replacement for the old `pkill -f 'pix-host
// serve'`. The host writes its pid to config.ServePidPath() on startup; these
// verbs read it, verify the process is actually OUR `pix-host serve`, and
// then signal it — so we never kill an arbitrary pid that happens to be in a
// stale/hijacked pidfile.
//
// The OS surface (pid read, remove, kill, /proc verify, sleep) is injected via
// serveCtl so the stale / alive / not-ours / term-then-kill paths are all
// unit-testable WITHOUT real processes (mirroring how reset.go injects resetFS).

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"pix/host/rpc"
	"pix/host/sys"
	"strconv"
	"strings"
	"syscall"
	"time"

	"pix/host/config"
)

// serveCtl bundles the injectable OS operations `serve stop`/`serve status` need.
// defaultServeCtl() wires the real syscall-backed ops; tests substitute fakes.
type serveCtl struct {
	pidPath    func() string                           // where the pidfile lives (config.ServePidPath)
	readPid    func(path string) (string, error)       // read the pidfile's raw contents
	removePid  func(path string) error                 // remove the pidfile
	kill       func(pid int, sig syscall.Signal) error // send a signal (sig 0 = liveness probe)
	verify     func(pid int) (ours bool, known bool)   // is pid our `pix-host serve`? known=false => can't tell
	sleep      func(d time.Duration)                   // poll delay (injected so tests don't wait)
	removeLazy func()                                  // clear the serve.lazy marker (optional; nil = skip)
	discover   func() ([]int, error)                   // find running pix-host serve pids when the pidfile is gone (optional; nil = skip)
}

// defaultServeCtl wires the real OS-backed control surface.
func defaultServeCtl() serveCtl {
	return serveCtl{
		pidPath:    config.ServePidPath,
		readPid:    func(path string) (string, error) { b, err := os.ReadFile(path); return string(b), err },
		removePid:  os.Remove,
		kill:       killProcess, // platform shim (serve_ctl_unix/windows.go)
		verify:     verifyServeProc,
		sleep:      time.Sleep,
		removeLazy: func() { _ = os.Remove(config.ServeLazyMarkerPath()) },
		discover:   discoverServeProcs, // platform shim (serve_ctl_unix/windows.go)
	}
}

// verifyServeProc reports whether pid is OUR `pix-host serve` process.
// Linux: read /proc/<pid>/cmdline (authoritative). Elsewhere (darwin/BSD, no
// /proc): fall back to `ps -o command= -p <pid>`. known=false means the check is
// unavailable (no /proc AND no usable ps) so ownership CANNOT be positively
// verified — the caller must REFUSE to signal rather than trust the pidfile.
// ours=false with known=true means the pid is ALIVE but clearly NOT our process
// (a recycled/hijacked pid) — the caller must also REFUSE to kill it.
func verifyServeProc(pid int) (ours bool, known bool) {
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		// cmdline is a NUL-separated argv (with a trailing NUL).
		argv := strings.Split(strings.Trim(string(b), "\x00"), "\x00")
		return cmdlineIsServe(argv), true
	}
	// No /proc — try ps (darwin/BSD).
	return verifyServeProcPS(pid, func(name string, args ...string) (string, error) {
		out, err := exec.Command(name, args...).CombinedOutput()
		return string(out), err
	})
}

// verifyServeProcPS is the darwin/BSD verify path: it asks `ps` for the target
// pid's identity and matches it. run is injected so it is unit-testable without
// a real process. A ps failure (absent, or the pid already gone) yields
// known=false: ownership cannot be positively verified, so the caller REFUSES to
// signal rather than trust the pidfile it wrote.
//
// Round 2 (H8, space-safe): the original single `ps -o command=` call then
// strings.Fields(line) SPLIT the executable path on every space in it — a
// binary at e.g. "/Users/alice/My Projects/pix-host" parsed argv[0] as
// just "/Users/alice/My", so cmdlineIsServe's basename check always failed for
// any path containing a space and verification broke (stop/status/install
// guard all rely on it). `comm=` returns ONE field — the executable path alone,
// no argv — so its basename is space-safe with no splitting at all; a separate
// `args=` call is only ever scanned for a standalone "serve" token, which is
// unaffected by spaces elsewhere in the line.
func verifyServeProcPS(pid int, run func(name string, args ...string) (string, error)) (ours bool, known bool) {
	commOut, err := run("ps", "-o", "comm=", "-p", strconv.Itoa(pid))
	if err != nil {
		return false, false // ps unavailable — can't tell
	}
	exe := strings.TrimSpace(commOut)
	if exe == "" {
		return false, false // ps returned nothing — can't tell
	}
	if filepath.Base(exe) != "pix-host" {
		return false, true // alive, clearly not ours
	}
	argsOut, err := run("ps", "-o", "args=", "-p", strconv.Itoa(pid))
	if err != nil {
		return false, false // ps unavailable for the args check — can't tell
	}
	line := strings.TrimSpace(argsOut)
	if line == "" {
		return false, false
	}
	// Scan for the standalone "serve" token ONLY in the arguments AFTER argv[0].
	// argv[0] in `args=` is the executable path, which `comm=` already gave us
	// EXACTLY (both are the full path, space-safe), so we strip it as a literal
	// prefix. This is immune to a path that both contains spaces AND repeats our
	// basename or a "serve" component
	// ("/opt/pix-host/x/pix-host serve/pix-host status") — the old
	// basename-search approaches all mis-located argv[0]'s end in that case and
	// leaked a path "serve" into the token scan (a false positive that could
	// signal the wrong process). Fallback (comm != argv[0], e.g. a symlinked
	// launch, which our own spawns never produce): the basename occurrence that
	// ENDS argv[0] (followed by whitespace or EOL); conservative, and this branch
	// is only reached when the exact-prefix strip fails.
	var rest string
	if strings.HasPrefix(line, exe) {
		rest = line[len(exe):]
	} else {
		base := filepath.Base(exe)
		for off := 0; ; {
			i := strings.Index(line[off:], base)
			if i < 0 {
				return false, true // basename not at any argv[0] boundary — treat as not-ours
			}
			end := off + i + len(base)
			if end == len(line) || line[end] == ' ' || line[end] == '\t' {
				rest = line[end:]
				break
			}
			off = end
		}
	}
	args := strings.Fields(rest)
	return len(args) > 0 && args[0] == "serve", true
}

// cmdlineIsServe requires the exact executable and first subcommand. Looking
// for `serve` anywhere later in argv could mistake `plugin broker serve` for
// the supervisor and kill an unrelated process after PID reuse.
func cmdlineIsServe(argv []string) bool {
	return len(argv) >= 2 && filepath.Base(argv[0]) == "pix-host" && argv[1] == "serve"
}

// stopServe is the SAFE replacement for `pkill -f 'pix-host serve'`. It
// returns stopped=true only when it signalled a live, verified-ours process and
// confirmed it exited. Every "not running" / stale / not-ours case returns
// stopped=false with a nil error and an explanatory line on out. A hard error
// (e.g. an unreadable pidfile that is NOT ENOENT) is returned so a caller can
// distinguish it.
func stopServe(ctl serveCtl, out io.Writer) (stopped bool, err error) {
	path := ctl.pidPath()

	raw, rerr := ctl.readPid(path)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			// No pidfile. This is the common orphan case after `pix reset`
			// (which moves the config dir — pidfile included — aside while a daemon
			// keeps running). Fall back to discovery: find any live process that we
			// can POSITIVELY verify is our `pix-host serve` and stop it. Still
			// never a blind pkill — each candidate is verified before signalling.
			return stopServeByDiscovery(ctl, out)
		}
		return false, fmt.Errorf("read pidfile %s: %w", path, rerr)
	}

	pid, perr := strconv.Atoi(strings.TrimSpace(raw))
	if perr != nil || pid <= 0 {
		_ = ctl.removePid(path)
		fmt.Fprintf(out, "serve not running (removed unparseable pidfile %s)\n", path)
		return false, nil
	}

	// Liveness probe: signal 0 checks existence without delivering a signal. A
	// failure (ESRCH) means the pid is dead — a stale pidfile from a crash.
	if ctl.kill(pid, 0) != nil {
		_ = ctl.removePid(path)
		fmt.Fprintf(out, "serve not running (stale pidfile removed, pid %d dead)\n", pid)
		return false, nil
	}

	// It is alive — VERIFY it is ours before we signal it (guards a stale/hijacked
	// pidfile). This is the check that immediately precedes the SIGTERM.
	if serveOwnershipRefused(ctl, pid, path, "it is not 'pix-host serve' (stale/hijacked pidfile)", out) {
		return false, nil
	}

	stopped, serr := signalServeToExit(ctl, pid, path, out)
	if serr != nil {
		return false, serr
	}
	if stopped {
		_ = ctl.removePid(path)
		clearServeLazyMarker(ctl)
	}
	return stopped, nil
}

// stopServeByDiscovery handles the missing-pidfile case: it asks ctl.discover
// for candidate pids, keeps only those it can POSITIVELY verify are our
// `pix-host serve` (alive + verified-ours), and stops each. This recovers
// an orphaned daemon (classically: `pix reset` moved the config dir, and
// the pidfile with it, out from under a still-running serve) without ever
// resorting to a blind `pkill`. No verified process => the honest "not running".
func stopServeByDiscovery(ctl serveCtl, out io.Writer) (bool, error) {
	if ctl.discover == nil {
		fmt.Fprintln(out, "serve not running (no pidfile)")
		return false, nil
	}
	pids, derr := ctl.discover()
	if derr != nil {
		fmt.Fprintln(out, "serve not running (no pidfile)")
		return false, nil
	}
	var ours []int
	for _, pid := range pids {
		if pid <= 0 || ctl.kill(pid, 0) != nil {
			continue // invalid or dead
		}
		if o, known := ctl.verify(pid); known && o {
			ours = append(ours, pid)
		}
	}
	if len(ours) == 0 {
		fmt.Fprintln(out, "serve not running (no pidfile)")
		return false, nil
	}
	anyStopped := false
	for _, pid := range ours {
		fmt.Fprintf(out, "no pidfile, but found a running 'pix-host serve' (pid %d); stopping it\n", pid)
		s, e := signalServeToExit(ctl, pid, "", out)
		if e != nil {
			return anyStopped, e
		}
		if s {
			anyStopped = true
		}
	}
	if anyStopped {
		clearServeLazyMarker(ctl)
	}
	return anyStopped, nil
}

// signalServeToExit runs the SIGTERM -> poll -> re-verify -> SIGKILL escalation
// against an already-verified-alive-and-ours pid, returning stopped=true only
// once the process is confirmed gone. path (may be "") only feeds the refusal
// hint; pidfile removal is the caller's job (discovery has none to remove).
func signalServeToExit(ctl serveCtl, pid int, path string, out io.Writer) (bool, error) {
	if err := ctl.kill(pid, syscall.SIGTERM); err != nil {
		return false, fmt.Errorf("SIGTERM pid %d: %w", pid, err)
	}
	if serveProcGone(ctl, pid, 5*time.Second) {
		fmt.Fprintf(out, "stopped 'pix-host serve' (pid %d, SIGTERM)\n", pid)
		return true, nil
	}

	// It survived SIGTERM. RE-VERIFY before escalating: during the 5s poll the
	// original process could have exited and its pid been reused by an unrelated
	// process — SIGKILLing that would be a serious bug.
	fmt.Fprintf(out, "pid %d did not exit on SIGTERM; re-verifying before SIGKILL\n", pid)
	if serveOwnershipRefused(ctl, pid, path, "identity changed since SIGTERM (possible PID reuse)", out) {
		return false, nil
	}
	fmt.Fprintf(out, "escalating to SIGKILL\n")
	if err := ctl.kill(pid, syscall.SIGKILL); err != nil {
		return false, fmt.Errorf("SIGKILL pid %d: %w", pid, err)
	}
	// Confirm the process is ACTUALLY gone before reporting success — don't claim
	// we stopped it if it somehow survived SIGKILL.
	if !serveProcGone(ctl, pid, 5*time.Second) {
		fmt.Fprintf(out, "pid %d is STILL alive after SIGKILL — could not stop it\n", pid)
		return false, fmt.Errorf("pid %d survived SIGKILL", pid)
	}
	fmt.Fprintf(out, "stopped 'pix-host serve' (pid %d, SIGKILL)\n", pid)
	return true, nil
}

// clearServeLazyMarker best-effort removes the serve.lazy marker after a
// successful stop, so lifecycle-mode detection never sees a stale "lazy" flag
// for a daemon we just stopped (guarded: fakes in older tests omit the field).
func clearServeLazyMarker(ctl serveCtl) {
	if ctl.removeLazy != nil {
		ctl.removeLazy()
	}
}

// serveOwnershipRefused re-checks (via ctl.verify) that pid is still our serve
// process right before a signal, printing + returning true when we must REFUSE.
// `when` is a short phase label folded into the message. We REFUSE in BOTH
// unsafe cases: the pid is alive but verified NOT ours (known && !ours), AND when
// ownership cannot be positively verified at all (!known: no /proc and ps is
// missing/errored). Signalling an UNVERIFIABLE pid would SIGTERM/SIGKILL a
// stale/reused pid from the pidfile — so we never trust the pidfile alone.
func serveOwnershipRefused(ctl serveCtl, pid int, path, when string, out io.Writer) bool {
	ours, known := ctl.verify(pid)
	rmHint, certainHint := "", ""
	if path != "" {
		rmHint = fmt.Sprintf(" Remove %s if you're sure.", path)
		certainHint = fmt.Sprintf(" (remove %s if you're certain)", path)
	}
	switch {
	case known && !ours:
		fmt.Fprintf(out, "refusing to stop pid %d; %s.%s\n", pid, when, rmHint)
		return true
	case !known:
		fmt.Fprintf(out, "cannot verify pid %d is 'pix-host serve' (%s); refusing to signal%s\n", pid, when, certainHint)
		return true
	}
	return false
}

// serveProcGone polls kill(pid,0) until the process is gone or the timeout's worth
// of polls elapse. The poll count is derived from timeout so an injected no-op
// sleep keeps tests fast without a wall-clock wait.
func serveProcGone(ctl serveCtl, pid int, timeout time.Duration) bool {
	const interval = 100 * time.Millisecond
	attempts := int(timeout/interval) + 1
	for i := 0; i < attempts; i++ {
		if ctl.kill(pid, 0) != nil {
			return true // gone
		}
		ctl.sleep(interval)
	}
	return ctl.kill(pid, 0) != nil
}

// serveState is the resolved `serve status` snapshot: is the supervisor running
// (pidfile present + alive + ours), and which service ports answer.
type serveState struct {
	Running       bool   `json:"running"`
	PID           int    `json:"pid,omitempty"`
	Detail        string `json:"detail,omitempty"`
	Memory        bool   `json:"memory"`
	Knowledge     bool   `json:"knowledge"`
	MemoryPort    int    `json:"memory_port"`
	KnowledgePort int    `json:"knowledge_port"`
}

// servePort resolves a service port honoring the MEMORY_PORT / KNOWLEDGE_PORT env
// overrides `serve` itself reads, preferring the injected env.Getenv (so tests
// stay hermetic) and falling back to the process environment.
// servePort takes sys.Getenver, not the whole world: it reads one variable.
// The signature is now the documentation.
func servePort(env sys.Getenver, name string, def int) int {
	if v := strings.TrimSpace(env.Getenv(name)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// resolveServeStatus reads the pidfile + probes the process and the service ports
// WITHOUT signalling anything. Split from the printer so it is unit-testable.
func resolveServeStatus(ctl serveCtl, env shellEnv) serveState {
	var st serveState
	path := ctl.pidPath()
	if raw, err := ctl.readPid(path); err == nil {
		if pid, perr := strconv.Atoi(strings.TrimSpace(raw)); perr == nil && pid > 0 {
			if ctl.kill(pid, 0) == nil {
				ours, known := ctl.verify(pid)
				if !known || ours {
					st.Running = true
					st.PID = pid
				} else {
					st.Detail = "pidfile points at a non-serve process (stale/hijacked)"
				}
			} else {
				st.Detail = "stale pidfile (process dead)"
			}
		} else {
			st.Detail = "unparseable pidfile"
		}
	} else if !os.IsNotExist(err) {
		st.Detail = "could not read pidfile: " + err.Error()
	}
	st.MemoryPort = servePort(env, "MEMORY_PORT", rpc.MemoryPortDefault)
	st.KnowledgePort = servePort(env, "KNOWLEDGE_PORT", rpc.KnowledgePortDefault)
	st.Memory = env.DialLocal(st.MemoryPort)
	st.Knowledge = env.DialLocal(st.KnowledgePort)
	return st
}

// printServeStatus renders a serveState for humans (or JSON when jsonOut).
func printServeStatus(st serveState, out io.Writer, jsonOut bool) {
	if jsonOut {
		b, _ := json.MarshalIndent(st, "", "  ")
		fmt.Fprintln(out, string(b))
		return
	}
	if st.Running {
		fmt.Fprintf(out, "serve: running (pid %d)\n", st.PID)
	} else if st.Detail != "" {
		fmt.Fprintf(out, "serve: not running: %s\n", st.Detail)
	} else {
		fmt.Fprintln(out, "serve: not running")
	}
	memPort, kbPort := st.MemoryPort, st.KnowledgePort
	if memPort == 0 {
		memPort = rpc.MemoryPortDefault
	}
	if kbPort == 0 {
		kbPort = rpc.KnowledgePortDefault
	}
	fmt.Fprintf(out, "  memory    (:%d): %s\n", memPort, upDown(st.Memory))
	fmt.Fprintf(out, "  knowledge (:%d): %s\n", kbPort, upDown(st.Knowledge))
}

// stopServeAnyMode stops the serve daemon in whatever lifecycle mode it is in.
// A MANAGED service (launchd KeepAlive / systemd Restart=) MUST be stopped via
// its supervisor — a bare SIGTERM to the pid is respawned within a second — so
// managed is handled FIRST via stopManagedService. Lazy/foreground/down all fall
// through to the pidfile-based (and discovery-fallback) stopServe. Injectable
// deps mirror the rest of this file so it stays unit-testable.
func stopServeAnyMode(managedActive func() bool, stopManaged func(io.Writer) error, ctl serveCtl, out io.Writer) (bool, error) {
	if managedActive() {
		if err := stopManaged(out); err != nil {
			return false, err
		}
		return true, nil
	}
	return stopServe(ctl, out)
}

// runServeStop is the `serve stop` entry point.
func runServeStop(argv []string) {
	if wantsHelp(argv) {
		fmt.Print(serveUsage)
		return
	}
	if len(argv) > 0 {
		fmt.Fprintf(os.Stderr, "pix serve stop: unexpected argument %q\n\n%s", argv[0], serveUsage)
		os.Exit(2)
	}
	_, err := stopServeAnyMode(managedServiceActive, stopManagedService, defaultServeCtl(), os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix serve stop: %v\n", err)
		os.Exit(1)
	}
}

// runServeStatus is the `serve status` entry point.
func runServeStatus(argv []string) {
	jsonOut := false
	for _, a := range argv {
		switch a {
		case "-h", "--help":
			fmt.Print(serveUsage)
			return
		case "--json":
			jsonOut = true
		default:
			fmt.Fprintf(os.Stderr, "pix serve status: unknown flag %q\n\n%s", a, serveUsage)
			os.Exit(2)
		}
	}
	env := defaultShellEnv()
	st := resolveServeStatus(defaultServeCtl(), env)
	printServeStatus(st, os.Stdout, jsonOut)
}
