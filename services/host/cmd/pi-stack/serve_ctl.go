// serve_ctl.go implements the launcher-side control verbs `serve stop` and
// `serve status`, the SAFE replacement for the old `pkill -f 'pi-stack-host
// serve'`. The host writes its pid to config.ServePidPath() on startup; these
// verbs read it, verify the process is actually OUR `pi-stack-host serve`, and
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
	"strconv"
	"strings"
	"syscall"
	"time"

	"pi-stack/host/config"
)

// serveCtl bundles the injectable OS operations `serve stop`/`serve status` need.
// defaultServeCtl() wires the real syscall-backed ops; tests substitute fakes.
type serveCtl struct {
	pidPath   func() string                           // where the pidfile lives (config.ServePidPath)
	readPid   func(path string) (string, error)       // read the pidfile's raw contents
	removePid func(path string) error                 // remove the pidfile
	kill      func(pid int, sig syscall.Signal) error // send a signal (sig 0 = liveness probe)
	verify    func(pid int) (ours bool, known bool)   // is pid our `pi-stack-host serve`? known=false => can't tell
	sleep     func(d time.Duration)                   // poll delay (injected so tests don't wait)
}

// defaultServeCtl wires the real OS-backed control surface.
func defaultServeCtl() serveCtl {
	return serveCtl{
		pidPath:   config.ServePidPath,
		readPid:   func(path string) (string, error) { b, err := os.ReadFile(path); return string(b), err },
		removePid: os.Remove,
		kill:      func(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) },
		verify:    verifyServeProc,
		sleep:     time.Sleep,
	}
}

// verifyServeProc reports whether pid is OUR `pi-stack-host serve` process by
// reading /proc/<pid>/cmdline (Linux). known=false means the check is unavailable
// (e.g. darwin, no /proc) so the caller falls back to trusting the pidfile it
// wrote. ours=false with known=true means the pid is ALIVE but clearly NOT our
// process (a recycled/hijacked pid) — the caller must REFUSE to kill it.
func verifyServeProc(pid int) (ours bool, known bool) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false, false // no /proc (darwin) or unreadable — can't tell
	}
	// cmdline is a NUL-separated argv; join with spaces for a substring check.
	cmd := strings.ReplaceAll(string(b), "\x00", " ")
	if strings.Contains(cmd, "pi-stack-host") && strings.Contains(cmd, "serve") {
		return true, true
	}
	return false, true
}

// stopServe is the SAFE replacement for `pkill -f 'pi-stack-host serve'`. It
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
			fmt.Fprintln(out, "serve not running (no pidfile)")
			return false, nil
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

	// It is alive — VERIFY it is ours before we signal it.
	ours, known := ctl.verify(pid)
	switch {
	case known && !ours:
		fmt.Fprintf(out, "refusing to stop pid %d — it is not 'pi-stack-host serve' (stale/hijacked pidfile). Remove %s if you're sure.\n", pid, path)
		return false, nil
	case !known:
		fmt.Fprintf(out, "note: cannot verify pid %d via /proc (no procfs) — trusting the pidfile we wrote\n", pid)
	}

	// SIGTERM, then poll up to ~5s for exit; escalate to SIGKILL if it survives.
	if err := ctl.kill(pid, syscall.SIGTERM); err != nil {
		return false, fmt.Errorf("SIGTERM pid %d: %w", pid, err)
	}
	if serveProcGone(ctl, pid, 5*time.Second) {
		_ = ctl.removePid(path)
		fmt.Fprintf(out, "stopped 'pi-stack-host serve' (pid %d, SIGTERM)\n", pid)
		return true, nil
	}

	fmt.Fprintf(out, "pid %d did not exit on SIGTERM; escalating to SIGKILL\n", pid)
	if err := ctl.kill(pid, syscall.SIGKILL); err != nil {
		return false, fmt.Errorf("SIGKILL pid %d: %w", pid, err)
	}
	serveProcGone(ctl, pid, 5*time.Second)
	_ = ctl.removePid(path)
	fmt.Fprintf(out, "stopped 'pi-stack-host serve' (pid %d, SIGKILL)\n", pid)
	return true, nil
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
	Running   bool   `json:"running"`
	PID       int    `json:"pid,omitempty"`
	Detail    string `json:"detail,omitempty"`
	Memory    bool   `json:"memory"`
	Knowledge bool   `json:"knowledge"`
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
	if env.dial != nil {
		st.Memory = env.dial(memoryPortDefault)
		st.Knowledge = env.dial(knowledgePortDefault)
	}
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
		fmt.Fprintf(out, "serve: not running — %s\n", st.Detail)
	} else {
		fmt.Fprintln(out, "serve: not running")
	}
	fmt.Fprintf(out, "  memory    (:%d): %s\n", memoryPortDefault, upDown(st.Memory))
	fmt.Fprintf(out, "  knowledge (:%d): %s\n", knowledgePortDefault, upDown(st.Knowledge))
}

// runServeStop is the `serve stop` entry point.
func runServeStop(argv []string) {
	if wantsHelp(argv) {
		fmt.Print(serveUsage)
		return
	}
	_, err := stopServe(defaultServeCtl(), os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack serve stop: %v\n", err)
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
			fmt.Fprintf(os.Stderr, "pi-stack serve status: unknown flag %q\n\n%s", a, serveUsage)
			os.Exit(2)
		}
	}
	st := resolveServeStatus(defaultServeCtl(), defaultShellEnv())
	printServeStatus(st, os.Stdout, jsonOut)
}
