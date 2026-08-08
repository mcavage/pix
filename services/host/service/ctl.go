// ctl.go implements `serve stop` and `serve status`, the SAFE replacement for
// `pkill -f 'pix-host serve'`: the daemon writes its pid to
// config.ServePidPath(), and nothing here signals a pid it has not positively
// verified is our own `pix-host serve`.

package service

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"pix/host/cli"
	"pix/host/rpc"
	"pix/host/sys"
	"pix/host/unitreport"
	"strconv"
	"strings"
	"syscall"
	"time"

	"pix/host/config"
)

// serveCtl bundles the injectable OS ops `serve stop`/`serve status` need.
// DefaultCtl() wires the real syscall-backed ops; tests substitute fakes.
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

// DefaultCtl wires the real OS-backed control surface.
func DefaultCtl() serveCtl {
	return serveCtl{
		pidPath:    config.ServePidPath,
		readPid:    func(path string) (string, error) { b, err := os.ReadFile(path); return string(b), err },
		removePid:  os.Remove,
		kill:       killProcess, // platform shim (ctl_unix.go)
		verify:     verifyServeProc,
		sleep:      time.Sleep,
		removeLazy: func() { _ = os.Remove(config.ServeLazyMarkerPath()) },
		discover:   discoverServeProcs, // platform shim (ctl_unix.go)
	}
}

// pidProbe is what ONE look at the pidfile answers. Stop, status and the start
// path's idempotency check read the SAME answer, so they cannot drift on what
// "running" means — only on what they DO about it.
type pidProbe struct {
	pid     int   // 0 when the pidfile was missing or unparseable
	missing bool  // no pidfile at all (the common orphaned-config-dir case)
	alive   bool  // kill(pid, 0) succeeded
	ours    bool  // verified our own `pix-host serve`
	known   bool  // the identity question was answerable at all
	err     error // a non-ENOENT read failure: report it, never call it "not running"
}

// isOurs is the only state anything may signal. running is the STATUS polarity:
// an unverifiable-but-live pid READS as running (honest for a reader) while
// anything that would signal it refuses on that same state.
func (p pidProbe) isOurs() bool  { return p.alive && p.known && p.ours }
func (p pidProbe) running() bool { return p.alive && (p.ours || !p.known) }

// statusDetail explains a not-running answer ("" when there is nothing to say).
func (p pidProbe) statusDetail() string {
	switch {
	case p.err != nil:
		return "could not read pidfile: " + p.err.Error()
	case p.missing:
		return ""
	case p.pid == 0:
		return "unparseable pidfile"
	case !p.alive:
		return "stale pidfile (process dead)"
	case !p.isOurs():
		return "pidfile points at a non-serve process (stale/hijacked)"
	}
	return ""
}

// probeServePid reads + classifies the pidfile WITHOUT signalling anything.
func probeServePid(ctl serveCtl) pidProbe {
	raw, err := ctl.readPid(ctl.pidPath())
	if err != nil {
		if os.IsNotExist(err) {
			return pidProbe{missing: true}
		}
		return pidProbe{err: err}
	}
	pid, perr := strconv.Atoi(strings.TrimSpace(raw))
	if perr != nil || pid <= 0 {
		return pidProbe{} // present but unparseable
	}
	// Signal 0 checks existence without delivering a signal; a failure (ESRCH)
	// means the pid is dead — a stale pidfile from a crash.
	if ctl.kill(pid, 0) != nil {
		return pidProbe{pid: pid}
	}
	ours, known := ctl.verify(pid)
	return pidProbe{pid: pid, alive: true, ours: ours, known: known}
}

// ServeIdentityUp answers "is a `pix-host serve` daemon really running?" from
// what the daemon IS — a loaded managed unit, or a pidfile naming a live process
// we cannot prove is a stranger's — and NEVER from a service port. A port is the
// wrong question in BOTH directions: it stays silent for a daemon whose memory
// service is disabled or has crashed (a caller then reads "down" and tears a live
// daemon down for good), and it answers for any stranger that bound :11435 (a
// caller then "restarts" a daemon that never ran). pidPath is the pidfile to
// classify — "" when the caller has no state dir, where only the managed answer
// applies. The polarity is pidProbe.running(), the REFUSING direction: a live pid
// whose identity is unverifiable reads as UP. settle > 0 waits, bounded, for a
// still-live pid to exit — a launchd bootout or a SIGTERM returns when the stop
// is DELIVERED, not when the process is reaped, and a caller that read that
// instant as "still up" would refuse a stop that in fact worked.
func ServeIdentityUp(managedActive func() bool, pidPath string, settle time.Duration) (up bool, pid int) {
	if managedActive != nil && managedActive() {
		return true, 0
	}
	if pidPath == "" {
		return false, 0
	}
	ctl := DefaultCtl()
	ctl.pidPath = func() string { return pidPath }
	pr := probeServePid(ctl)
	if !pr.running() {
		return false, pr.pid
	}
	if settle > 0 && serveProcGone(ctl, pr.pid, settle) {
		return false, pr.pid
	}
	return true, pr.pid
}

// verifyServeProc reports whether pid is OUR `pix-host serve` process.
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
// pid's identity. Anything ps cannot answer yields known=false ("can't tell"),
// which every caller treats as "do not signal".
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

// cmdlineIsServe requires the exact executable AND first subcommand: matching
// `serve` anywhere later in argv could mistake `plugin broker serve` for the
// supervisor and kill an unrelated process after PID reuse.
func cmdlineIsServe(argv []string) bool {
	return len(argv) >= 2 && filepath.Base(argv[0]) == "pix-host" && argv[1] == "serve"
}

// Stop returns stopped=true only when it signalled a live, verified-ours process
// AND confirmed it exited. Every not-running/stale/not-ours case is (false, nil):
// there was nothing of ours to stop, which is not an error.
func Stop(ctl serveCtl, out io.Writer) (stopped bool, err error) {
	path := ctl.pidPath()
	pr := probeServePid(ctl)
	switch {
	case pr.err != nil:
		return false, fmt.Errorf("read pidfile %s: %w", path, pr.err)
	case pr.missing:
		// No pidfile. This is the common orphan case when the config dir —
		// pidfile included — is moved aside by hand while a daemon keeps
		// running. Fall back to discovery.
		return stopServeByDiscovery(ctl, out)
	case pr.pid == 0:
		_ = ctl.removePid(path)
		fmt.Fprintf(out, "serve not running (removed unparseable pidfile %s)\n", path)
		return false, nil
	case !pr.alive:
		_ = ctl.removePid(path)
		fmt.Fprintf(out, "serve not running (stale pidfile removed, pid %d dead)\n", pr.pid)
		return false, nil
	case !pr.isOurs():
		// Alive, but not provably ours: refuse. This is the check that
		// immediately precedes the SIGTERM.
		printServeRefusal(pr.known, pr.pid, path, "it is not 'pix-host serve' (stale/hijacked pidfile)", out)
		return false, nil
	}

	stopped, serr := signalServeToExit(ctl, pr.pid, path, out)
	if serr != nil {
		return false, serr
	}
	if stopped {
		_ = ctl.removePid(path)
		clearServeLazyMarker(ctl)
	}
	return stopped, nil
}

// stopServeByDiscovery handles the missing-pidfile case: of ctl.discover's
// candidates it keeps only pids POSITIVELY verified as our live serve, and stops
// each. This is what recovers a daemon orphaned by a moved-aside config dir.
func stopServeByDiscovery(ctl serveCtl, out io.Writer) (bool, error) {
	var ours []int
	if ctl.discover != nil {
		pids, derr := ctl.discover()
		if derr == nil {
			for _, pid := range pids {
				if pid <= 0 || ctl.kill(pid, 0) != nil {
					continue // invalid or dead
				}
				if o, known := ctl.verify(pid); known && o {
					ours = append(ours, pid)
				}
			}
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
// against an already-verified pid, reporting stopped only once it is gone.
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
	if ours, known := ctl.verify(pid); !ours || !known {
		printServeRefusal(known, pid, path, "identity changed since SIGTERM (possible PID reuse)", out)
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
// successful stop, so mode detection never sees a stale "lazy" flag for a daemon
// we just stopped.
func clearServeLazyMarker(ctl serveCtl) {
	if ctl.removeLazy != nil {
		ctl.removeLazy()
	}
}

// printServeRefusal explains why we will NOT signal pid: either it is provably
// someone else's (known) or we could not tell (!known), which refuses just as
// hard. `when` names the moment the check ran; path (may be "") adds the "remove
// it if you're sure" escape hatch.
func printServeRefusal(known bool, pid int, path, when string, out io.Writer) {
	sure, certain := "", ""
	if path != "" {
		sure = fmt.Sprintf(" Remove %s if you're sure.", path)
		certain = fmt.Sprintf(" (remove %s if you're certain)", path)
	}
	if known {
		fmt.Fprintf(out, "refusing to stop pid %d; %s.%s\n", pid, when, sure)
		return
	}
	fmt.Fprintf(out, "cannot verify pid %d is 'pix-host serve' (%s); refusing to signal%s\n", pid, when, certain)
}

// serveProcGone polls kill(pid,0) until the process is gone or the timeout's
// worth of polls elapse (derived from timeout, so an injected no-op sleep keeps
// tests fast).
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
	Running    bool   `json:"running"`
	PID        int    `json:"pid,omitempty"`
	Detail     string `json:"detail,omitempty"`
	Memory     bool   `json:"memory"`
	MemoryPort int    `json:"memory_port"`
	// Units is the supervision tree as `serve` last published it: identity,
	// state, restarts, generation, reattach, scrubbed last error and last probe
	// latency, per unit. It is ALWAYS present in JSON — as [] when there is
	// nothing to report — and NEVER omitted just because it is empty: a reader
	// must be able to tell "zero supervised units" from "the field is missing
	// because this build predates it". UnitsDetail explains an empty or stale
	// Units; it is never "everything is fine" by omission either.
	Units       []unitreport.Unit `json:"units"`
	UnitsDetail string            `json:"units_detail"`
}

// unitsStaleAfter is how long a published snapshot stays believable. serve
// republishes every 5s, so three missed intervals means the daemon is wedged or
// died without cleaning up — either way the units are reported as stale, not as
// their last known good state.
const unitsStaleAfter = 20 * time.Second

// resolveServeUnits loads the published supervision snapshot and decides whether
// it describes THIS running daemon. Split out as a pure function so a test can
// hand it a file and a clock instead of a live tree.
func resolveServeUnits(path string, running bool, pid int, now time.Time) ([]unitreport.Unit, string) {
	// emptyUnits is returned (never nil) whenever there is no tree to show: a nil
	// slice marshals to JSON `null`, and this field must always be an array —
	// "zero units" and "field absent" are different claims.
	emptyUnits := []unitreport.Unit{}
	rep, found, err := unitreport.ReadReport(path)
	switch {
	case err != nil:
		return emptyUnits, fmt.Sprintf("unreadable supervision snapshot (%v)", err)
	case !found && !running:
		return emptyUnits, "" // not running: no tree to report, and nothing surprising about it
	case !found:
		return emptyUnits, "serve is running but published no supervision snapshot"
	case !running:
		return emptyUnits, "stale supervision snapshot from a serve that is no longer running"
	case pid != 0 && rep.PID != 0 && rep.PID != pid:
		return emptyUnits, fmt.Sprintf("supervision snapshot belongs to pid %d, not the running pid %d", rep.PID, pid)
	case rep.SchemaVersion != unitreport.SchemaVersion:
		return emptyUnits, fmt.Sprintf("supervision snapshot schema %d, this build reads %d", rep.SchemaVersion, unitreport.SchemaVersion)
	}
	units := rep.Units
	if units == nil {
		units = emptyUnits
	}
	if age := now.Sub(time.Unix(rep.GeneratedUnix, 0)); age > unitsStaleAfter {
		// A stale snapshot's units are refused the same as a wrong-pid or
		// schema-mismatched one: they describe a tree that may no longer
		// exist, so they must never render as current rows alongside the
		// "units: unknown" line — that combination reads as "fine except for
		// this one thing" when it is really "we cannot vouch for any of it".
		return emptyUnits, fmt.Sprintf("supervision snapshot is %ds stale", int(age.Seconds()))
	}
	return units, ""
}

// portProbe is what the status/upgrade paths actually touch: one environment
// variable and one local port.
type portProbe interface {
	sys.Getenver
	sys.Net
}

// Port resolves a service port honoring the env override `serve` itself reads,
// through the injected Getenv so tests stay hermetic.
func Port(env sys.Getenver, name string, def int) int {
	if v := strings.TrimSpace(env.Getenv(name)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// resolveServeStatus classifies the pidfile + probes the service ports WITHOUT
// signalling anything. Split from the printer so it is unit-testable.
func resolveServeStatus(ctl serveCtl, env portProbe) serveState {
	var st serveState
	pr := probeServePid(ctl)
	st.Running, st.Detail = pr.running(), pr.statusDetail()
	if st.Running {
		st.PID = pr.pid
	}
	st.MemoryPort = Port(env, "MEMORY_PORT", rpc.MemoryPortDefault)
	st.Memory = env.DialLocal(st.MemoryPort)
	st.Units, st.UnitsDetail = resolveServeUnits(config.ServeUnitsPath(), st.Running, st.PID, time.Now())
	return st
}

// printServeStatus renders a serveState for humans (or JSON when jsonOut).
func printServeStatus(st serveState, out io.Writer, jsonOut bool) {
	if jsonOut {
		// Units must publish as [] , never JSON `null`, even for a serveState a
		// caller built directly (not through resolveServeStatus) with the field
		// left at its nil zero value.
		if st.Units == nil {
			st.Units = []unitreport.Unit{}
		}
		b, _ := json.MarshalIndent(st, "", "  ")
		fmt.Fprintln(out, string(b))
		return
	}
	switch {
	case st.Running:
		fmt.Fprintf(out, "serve: running (pid %d)\n", st.PID)
	case st.Detail != "":
		fmt.Fprintf(out, "serve: not running: %s\n", st.Detail)
	default:
		fmt.Fprintln(out, "serve: not running")
	}
	memPort := st.MemoryPort
	if memPort == 0 {
		memPort = rpc.MemoryPortDefault
	}
	fmt.Fprintf(out, "  memory (:%d): %s\n", memPort, cli.UpDown(st.Memory))
	printServeUnits(st, out)
}

// printServeUnits renders the supervision tree for humans: one line per unit,
// with the numbers that answer "is it flapping" (restarts/generation) and "is it
// slowing down" (last probe) beside the state. A unit we could not see says so.
func printServeUnits(st serveState, out io.Writer) {
	if st.UnitsDetail != "" {
		fmt.Fprintf(out, "  units: unknown (%s)\n", st.UnitsDetail)
	}
	if len(st.Units) == 0 {
		if st.UnitsDetail == "" && st.Running {
			fmt.Fprintln(out, "  units: none supervised")
		}
		return
	}
	for _, u := range st.Units {
		extra := ""
		if u.Reattached {
			extra = " reattached"
		}
		fmt.Fprintf(out, "  unit %s (%s): %s pid=%d gen=%d restarts=%d probe=%dus%s\n",
			u.Name, u.Kind, u.State, u.PID, u.Generation, u.Restarts, u.LastProbeUS, extra)
		if u.LastError != "" {
			fmt.Fprintf(out, "    last error: %s\n", u.LastError)
		}
	}
}

// StopAnyMode stops the daemon in whatever lifecycle mode it is in. A MANAGED
// service goes through its supervisor: a bare SIGTERM is undone by launchd's
// KeepAlive — the classic "I stopped it but it came right back" bug (invariant #3).
func StopAnyMode(managedActive func() bool, stopManaged func(io.Writer) error, ctl serveCtl, out io.Writer) (bool, error) {
	if managedActive() {
		if err := stopManaged(out); err != nil {
			return false, err
		}
		return true, nil
	}
	return Stop(ctl, out)
}

// StopService is the `serve stop` entry point.
func StopService(out io.Writer) error {
	_, err := StopAnyMode(ManagedActive, StopManaged, DefaultCtl(), out)
	return err
}

// ReportStatus prints whether serve is running and whether its ports are up.
func ReportStatus(out io.Writer, jsonOut bool) error {
	printServeStatus(resolveServeStatus(DefaultCtl(), sys.Real{}), out, jsonOut)
	return nil
}
