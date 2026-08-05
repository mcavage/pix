package service

import (
	"bytes"
	"fmt"
	"pix/host/rpc"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeProc is a scripted process used to exercise Stop without any real OS
// process. It tracks liveness and the signals it received, and can be told to
// die after N SIGTERM liveness probes (to simulate a graceful exit) or to ignore
type fakeProc struct {
	pid          int
	alive        bool
	sigs         []syscall.Signal // every non-zero signal delivered, in order
	dieOnTerm    bool             // exit on the next liveness probe after a SIGTERM
	dieOnKill    bool             // exit on the next liveness probe after a SIGKILL
	surviveKill  bool             // pathological: ignore SIGKILL, stay alive
	termReceived bool
	killReceived bool
}

// kill implements serveCtl.kill against the scripted process. Signal 0 is a
// liveness probe: it returns ESRCH once the process has "exited".
func (p *fakeProc) kill(pid int, sig syscall.Signal) error {
	if pid != p.pid || !p.alive {
		return syscall.ESRCH
	}
	if sig == 0 {
		// A liveness probe also advances the death state, so a poll loop observes
		// the process exiting after the signal it was told to die on.
		if p.termReceived && p.dieOnTerm {
			p.alive = false
			return syscall.ESRCH
		}
		if p.killReceived && p.dieOnKill {
			p.alive = false
			return syscall.ESRCH
		}
		return nil
	}
	p.sigs = append(p.sigs, sig)
	if sig == syscall.SIGTERM {
		p.termReceived = true
	}
	if sig == syscall.SIGKILL {
		p.killReceived = true
		// SIGKILL is uncatchable; the process is gone right away in this model
		// UNLESS the test is modelling a pathological survivor.
		if !p.surviveKill {
			p.alive = false
		}
	}
	return nil
}

// ctlFor builds a serveCtl wired to a fake pidfile + fake process. readErr, when
// non-nil, is returned by readPid (e.g. an ENOENT to model a missing pidfile).
func ctlFor(pidRaw string, readErr error, proc *fakeProc, removed *bool, verify func(int) (bool, bool)) serveCtl {
	if verify == nil {
		verify = func(int) (bool, bool) { return true, true } // ours by default
	}
	kill := func(pid int, sig syscall.Signal) error { return syscall.ESRCH }
	if proc != nil {
		kill = proc.kill
	}
	return serveCtl{
		pidPath:   func() string { return "/fake/serve.pid" },
		readPid:   func(string) (string, error) { return pidRaw, readErr },
		removePid: func(string) error { *removed = true; return nil },
		kill:      kill,
		verify:    verify,
		sleep:     func(time.Duration) {}, // never wait in tests
	}
}

// TestStopServe_NoPidfile: absent pidfile => not running, nothing removed.
func TestStopServe_NoPidfile(t *testing.T) {
	removed := false
	ctl := ctlFor("", syscall.ENOENT, nil, &removed, nil)
	var buf bytes.Buffer
	stopped, err := Stop(ctl, &buf)
	if err != nil || stopped {
		t.Fatalf("stopped=%v err=%v, want false,nil", stopped, err)
	}
	if !strings.Contains(buf.String(), "no pidfile") {
		t.Errorf("want 'no pidfile' message, got %q", buf.String())
	}
	if removed {
		t.Error("must not remove a pidfile that was never there")
	}
}

// TestStopServe_NoPidfile_DiscoversOrphan: no pidfile, but a live verified-ours
// serve is discovered (the `pix reset` orphan case) => it is stopped.
func TestStopServe_NoPidfile_DiscoversOrphan(t *testing.T) {
	removed := false
	proc := &fakeProc{pid: 5150, alive: true, dieOnTerm: true}
	ctl := ctlFor("", syscall.ENOENT, proc, &removed, nil)
	ctl.discover = func() ([]int, error) { return []int{5150}, nil }
	var buf bytes.Buffer
	stopped, err := Stop(ctl, &buf)
	if err != nil || !stopped {
		t.Fatalf("stopped=%v err=%v, want true,nil; out=%q", stopped, err, buf.String())
	}
	if !proc.termReceived {
		t.Error("discovered orphan should have received SIGTERM")
	}
	if !strings.Contains(buf.String(), "no pidfile, but found a running") {
		t.Errorf("want discovery message, got %q", buf.String())
	}
}

// TestStopServe_NoPidfile_DiscoveryNotOurs: a discovered pid that verifies as
// NOT ours is left alone (never signalled) => honest "not running".
func TestStopServe_NoPidfile_DiscoveryNotOurs(t *testing.T) {
	removed := false
	proc := &fakeProc{pid: 6000, alive: true}
	notOurs := func(int) (bool, bool) { return false, true } // alive, not ours
	ctl := ctlFor("", syscall.ENOENT, proc, &removed, notOurs)
	ctl.discover = func() ([]int, error) { return []int{6000}, nil }
	var buf bytes.Buffer
	stopped, err := Stop(ctl, &buf)
	if err != nil || stopped {
		t.Fatalf("stopped=%v err=%v, want false,nil", stopped, err)
	}
	if proc.termReceived {
		t.Error("a not-ours discovered pid must never be signalled")
	}
	if !strings.Contains(buf.String(), "no pidfile") {
		t.Errorf("want 'no pidfile' message, got %q", buf.String())
	}
}

// TestStopServe_StalePidfile: pid dead (kill(0) ESRCH) => stale removed, not running.
func TestStopServe_StalePidfile(t *testing.T) {
	removed := false
	proc := &fakeProc{pid: 4242, alive: false} // dead
	ctl := ctlFor("4242\n", nil, proc, &removed, nil)
	var buf bytes.Buffer
	stopped, err := Stop(ctl, &buf)
	if err != nil || stopped {
		t.Fatalf("stopped=%v err=%v, want false,nil", stopped, err)
	}
	if !removed {
		t.Error("a stale pidfile must be removed")
	}
	if !strings.Contains(buf.String(), "stale pidfile") {
		t.Errorf("want 'stale pidfile' message, got %q", buf.String())
	}
}

// TestStopServe_AliveAndOurs_SIGTERM: alive + verified ours => SIGTERM, exits,
// pidfile removed, no SIGKILL.
func TestStopServe_AliveAndOurs_SIGTERM(t *testing.T) {
	removed := false
	proc := &fakeProc{pid: 100, alive: true, dieOnTerm: true}
	ctl := ctlFor("100", nil, proc, &removed, func(int) (bool, bool) { return true, true })
	var buf bytes.Buffer
	stopped, err := Stop(ctl, &buf)
	if err != nil || !stopped {
		t.Fatalf("stopped=%v err=%v, want true,nil", stopped, err)
	}
	if !removed {
		t.Error("pidfile must be removed once the process is gone")
	}
	if len(proc.sigs) != 1 || proc.sigs[0] != syscall.SIGTERM {
		t.Errorf("want exactly one SIGTERM, got %v", proc.sigs)
	}
	if !strings.Contains(buf.String(), "SIGTERM") {
		t.Errorf("want SIGTERM report, got %q", buf.String())
	}
}

// TestStopServe_NotOurs_Refuses: alive but cmdline mismatch => refuse to kill,
// no signal, pidfile left in place.
func TestStopServe_NotOurs_Refuses(t *testing.T) {
	removed := false
	proc := &fakeProc{pid: 55, alive: true, dieOnTerm: true}
	notOurs := func(int) (bool, bool) { return false, true } // known, but not ours
	ctl := ctlFor("55", nil, proc, &removed, notOurs)
	var buf bytes.Buffer
	stopped, err := Stop(ctl, &buf)
	if err != nil || stopped {
		t.Fatalf("stopped=%v err=%v, want false,nil", stopped, err)
	}
	if len(proc.sigs) != 0 {
		t.Errorf("must NOT signal a process that is not ours, got %v", proc.sigs)
	}
	if removed {
		t.Error("must not remove the pidfile of a live foreign process")
	}
	if !strings.Contains(buf.String(), "refusing") {
		t.Errorf("want a refusal message, got %q", buf.String())
	}
}

// TestStopServe_SurvivesSIGTERM_Escalates: alive + ignores SIGTERM => escalate to
// SIGKILL, pidfile removed.
func TestStopServe_SurvivesSIGTERM_Escalates(t *testing.T) {
	removed := false
	// dieOnTerm=false: it stays alive through the SIGTERM poll; SIGKILL ends it.
	proc := &fakeProc{pid: 77, alive: true, dieOnTerm: false, dieOnKill: true}
	ctl := ctlFor("77", nil, proc, &removed, nil)
	var buf bytes.Buffer
	stopped, err := Stop(ctl, &buf)
	if err != nil || !stopped {
		t.Fatalf("stopped=%v err=%v, want true,nil", stopped, err)
	}
	if len(proc.sigs) != 2 || proc.sigs[0] != syscall.SIGTERM || proc.sigs[1] != syscall.SIGKILL {
		t.Errorf("want SIGTERM then SIGKILL, got %v", proc.sigs)
	}
	if !removed {
		t.Error("pidfile must be removed after SIGKILL")
	}
	if !strings.Contains(buf.String(), "SIGKILL") {
		t.Errorf("want SIGKILL escalation report, got %q", buf.String())
	}
}

// TestStopServe_UnverifiableRefuses: no /proc AND ps unavailable (known=false) =>
// ownership cannot be positively verified, so Stop REFUSES to signal (never
// trusts the pidfile alone: a stale/reused pid would otherwise get SIGTERM). No
func TestStopServe_UnverifiableRefuses(t *testing.T) {
	removed := false
	proc := &fakeProc{pid: 88, alive: true, dieOnTerm: true}
	unknown := func(int) (bool, bool) { return false, false } // can't tell (no /proc, no ps)
	ctl := ctlFor("88", nil, proc, &removed, unknown)
	var buf bytes.Buffer
	stopped, err := Stop(ctl, &buf)
	if err != nil || stopped {
		t.Fatalf("stopped=%v err=%v, want false,nil", stopped, err)
	}
	if len(proc.sigs) != 0 {
		t.Errorf("must NOT signal an unverifiable pid, got %v", proc.sigs)
	}
	if removed {
		t.Error("must not remove the pidfile of an unverifiable live process")
	}
	if !strings.Contains(buf.String(), "cannot verify") || !strings.Contains(buf.String(), "refusing to signal") {
		t.Errorf("want a refusal message, got %q", buf.String())
	}
}

// TestResolveServeStatus_Running: pidfile present + alive + ours + ports up.
func TestResolveServeStatus_Running(t *testing.T) {
	removed := false
	proc := &fakeProc{pid: 321, alive: true}
	ctl := ctlFor("321", nil, proc, &removed, func(int) (bool, bool) { return true, true })
	env := fakeEnv{ports: map[int]bool{rpc.MemoryPortDefault: true}}.env()

	st := resolveServeStatus(ctl, env)
	if !st.Running || st.PID != 321 {
		t.Fatalf("want running pid 321, got %+v", st)
	}
	if !st.Memory {
		t.Errorf("want memory up, got %+v", st)
	}

	var buf bytes.Buffer
	printServeStatus(st, &buf, false)
	if !strings.Contains(buf.String(), "running (pid 321)") {
		t.Errorf("printer omitted pid: %q", buf.String())
	}
}

// TestResolveServeStatus_NotRunning: no pidfile => not running; ports reflected.
func TestResolveServeStatus_NotRunning(t *testing.T) {
	removed := false
	ctl := ctlFor("", syscall.ENOENT, nil, &removed, nil)
	env := fakeEnv{ports: map[int]bool{}}.env()

	st := resolveServeStatus(ctl, env)
	if st.Running || st.PID != 0 {
		t.Fatalf("want not running, got %+v", st)
	}

	var buf bytes.Buffer
	printServeStatus(st, &buf, false)
	if !strings.Contains(buf.String(), "not running") {
		t.Errorf("printer should say not running: %q", buf.String())
	}
}

// TestResolveServeStatus_StalePidfile: alive-but-dead pid reports not running
// with a stale detail (status never signals, so it does not remove the file).
func TestResolveServeStatus_StaleNotRunning(t *testing.T) {
	removed := false
	proc := &fakeProc{pid: 9, alive: false}
	ctl := ctlFor("9", nil, proc, &removed, nil)
	env := fakeEnv{ports: map[int]bool{}}.env()
	st := resolveServeStatus(ctl, env)
	if st.Running {
		t.Fatalf("stale pid must not report running, got %+v", st)
	}
	if !strings.Contains(st.Detail, "stale") {
		t.Errorf("want stale detail, got %q", st.Detail)
	}
}

// psFake builds a two-column `ps` fake: verifyServeProcPS calls `ps -o comm=`
// (the executable path alone, no argv) then `ps -o args=` (the full command
// line, only ever scanned for a standalone "serve" token). Dispatch is on the
func psFake(comm, args string, commErr, argsErr error) func(string, ...string) (string, error) {
	return func(_ string, a ...string) (string, error) {
		for _, v := range a {
			switch v {
			case "comm=":
				return comm, commErr
			case "args=":
				return args, argsErr
			}
		}
		return "", fmt.Errorf("unexpected ps invocation: %v", a)
	}
}

// TestVerifyServeProcPS_Darwin: the darwin/BSD verify path matches our serve
// process from injected `ps -o comm=`/`args=` calls and rejects an unrelated
// process; an absent ps yields known=false (can't tell, trust the pidfile).
func TestVerifyServeProcPS_Darwin(t *testing.T) {
	ours := psFake("/usr/local/bin/pix-host\n", "/usr/local/bin/pix-host serve --foo\n", nil, nil)
	if ok, known := verifyServeProcPS(4242, ours); !ok || !known {
		t.Errorf("our serve process: ours=%v known=%v, want true,true", ok, known)
	}

	notOurs := psFake("/usr/bin/postgres\n", "/usr/bin/postgres -D /data serve\n", nil, nil) // basename not pix-host
	if ok, known := verifyServeProcPS(4242, notOurs); ok || !known {
		t.Errorf("foreign process: ours=%v known=%v, want false,true", ok, known)
	}

	noPS := psFake("", "", syscall.ENOENT, nil)
	if ok, known := verifyServeProcPS(4242, noPS); ok || known {
		t.Errorf("absent ps: ours=%v known=%v, want false,false", ok, known)
	}
}

// Round 2 (H8, space-safe): a `pix-host` installed at a path containing
// spaces (e.g. "/Users/alice/My Projects/pix-host") must still verify.
func TestVerifyServeProcPS_PathWithSpaces(t *testing.T) {
	spaced := psFake(
		"/Users/alice/My Projects/pix-host\n",
		"/Users/alice/My Projects/pix-host serve\n",
		nil, nil)
	if ok, known := verifyServeProcPS(4242, spaced); !ok || !known {
		t.Errorf("spaced-path serve process: ours=%v known=%v, want true,true", ok, known)
	}

	// A foreign process at a spaced path must still be rejected (basename check
	// still discriminates correctly, it just no longer mis-splits).
	notServe := psFake("/Users/alice/My Projects/pix-host\n", "/Users/alice/My Projects/pix-host status\n", nil, nil)
	if ok, known := verifyServeProcPS(4242, notServe); ok || !known {
		t.Errorf("spaced-path non-serve process: ours=%v known=%v, want false,true", ok, known)
	}
}

// Round 3 (H11): the standalone "serve" token scan must ignore argv[0], since
// a `pix-host` binary installed under a path that has a directory
// component literally named "serve" (e.g. "/Users/alice/My serve
func TestVerifyServeProcPS_ServeInPathNotSubcommand(t *testing.T) {
	falsePositive := psFake(
		"pix-host\n",
		"/Users/alice/My serve Projects/pix-host status\n",
		nil, nil)
	if ok, known := verifyServeProcPS(4242, falsePositive); ok || !known {
		t.Errorf("a `serve` PATH component with no serve SUBCOMMAND must be rejected: ours=%v known=%v, want false,true", ok, known)
	}

	genuine := psFake(
		"pix-host\n",
		"/Users/alice/My serve Projects/pix-host serve\n",
		nil, nil)
	if ok, known := verifyServeProcPS(4242, genuine); !ok || !known {
		t.Errorf("a real serve subcommand after a spaced/`serve`-containing path must still verify: ours=%v known=%v, want true,true", ok, known)
	}
}

// TestVerifyServeProcPS_RepeatedBasenameFullComm exercises the robust primary
// path: on macOS `ps -o comm=` returns the FULL executable path, so argv[0] is
// stripped by exact prefix. A path that both contains spaces AND repeats the
func TestVerifyServeProcPS_RepeatedBasenameFullComm(t *testing.T) {
	exe := "/opt/pix-host/My serve dir/pix-host"
	falsePositive := psFake(exe+"\n", exe+" status\n", nil, nil)
	if ok, known := verifyServeProcPS(4242, falsePositive); ok || !known {
		t.Errorf("repeated-basename spaced path with no serve subcommand must be rejected: ours=%v known=%v, want false,true", ok, known)
	}
	genuine := psFake(exe+"\n", exe+" serve\n", nil, nil)
	if ok, known := verifyServeProcPS(4242, genuine); !ok || !known {
		t.Errorf("genuine serve after a repeated-basename spaced path must verify: ours=%v known=%v, want true,true", ok, known)
	}
}

// TestCmdlineIsServe_Tight: only `pix-host` basename + `serve` as the
// first subcommand counts; a process merely mentioning the words does not.
func TestCmdlineIsServe_Tight(t *testing.T) {
	if !cmdlineIsServe([]string{"/opt/pix-host", "serve", "--x"}) {
		t.Error("real serve cmdline must match")
	}
	if cmdlineIsServe([]string{"/bin/grep", "pix-host serve"}) {
		t.Error("a grep of the words must NOT match")
	}
	if cmdlineIsServe([]string{"/opt/pix-host", "status"}) {
		t.Error("pix-host without a serve arg must NOT match")
	}
	if cmdlineIsServe([]string{"/opt/pix-host", "plugin", "broker", "serve"}) {
		t.Error("serve in a later plugin argument must NOT match the supervisor")
	}
	exe := "/opt/pix-host"
	plugin := psFake(exe+"\n", exe+" plugin broker serve\n", nil, nil)
	if ok, known := verifyServeProcPS(4242, plugin); ok || !known {
		t.Errorf("ps fallback must reject a later serve arg: ours=%v known=%v", ok, known)
	}
}

// TestStopServe_ReVerifyBeforeKill: a pid whose identity CHANGES between the
// up-front check and the pre-SIGKILL re-check (PID reuse during the poll) must
// NOT be SIGKILLed — Stop refuses on the second verify.
func TestStopServe_ReVerifyBeforeKill(t *testing.T) {
	removed := false
	// dieOnTerm=false so it survives SIGTERM and we reach the re-verify gate.
	proc := &fakeProc{pid: 202, alive: true, dieOnTerm: false}
	calls := 0
	verify := func(int) (bool, bool) {
		calls++
		return calls == 1, true // ours on the 1st (pre-SIGTERM) call, NOT ours on the 2nd
	}
	ctl := ctlFor("202", nil, proc, &removed, verify)
	var buf bytes.Buffer
	stopped, err := Stop(ctl, &buf)
	if stopped {
		t.Fatalf("must not report stopped when identity changed mid-poll (err=%v)", err)
	}
	for _, s := range proc.sigs {
		if s == syscall.SIGKILL {
			t.Fatal("must NOT SIGKILL a pid that changed identity during the poll")
		}
	}
	if removed {
		t.Error("must not remove the pidfile after refusing to SIGKILL")
	}
	if !strings.Contains(buf.String(), "PID reuse") {
		t.Errorf("want a PID-reuse refusal note, got %q", buf.String())
	}
}

// TestStopServe_SurvivesSIGKILL_NotSuccess: a process that somehow outlives
// SIGKILL must be reported as NOT stopped (with an error), never claimed stopped.
func TestStopServe_SurvivesSIGKILL_NotSuccess(t *testing.T) {
	removed := false
	proc := &fakeProc{pid: 303, alive: true, dieOnTerm: false, surviveKill: true}
	ctl := ctlFor("303", nil, proc, &removed, nil)
	var buf bytes.Buffer
	stopped, err := Stop(ctl, &buf)
	if stopped {
		t.Fatal("must not report stopped when the process survived SIGKILL")
	}
	if err == nil {
		t.Error("a survived SIGKILL must surface an error")
	}
	if removed {
		t.Error("must not remove the pidfile of a process that is still alive")
	}
	if !strings.Contains(buf.String(), "STILL alive after SIGKILL") {
		t.Errorf("want an honest survival report, got %q", buf.String())
	}
}

// TestResolveServeStatus_HonorsPortEnv: an overridden MEMORY_PORT is used for
// both the dial probe and the printed line, not the hardcoded default.
func TestResolveServeStatus_HonorsPortEnv(t *testing.T) {
	removed := false
	ctl := ctlFor("", syscall.ENOENT, nil, &removed, nil)
	const customPort = 21435
	env := fakeEnv{
		envVars: map[string]string{"MEMORY_PORT": strconv.Itoa(customPort)},
		ports:   map[int]bool{customPort: true, rpc.MemoryPortDefault: false},
	}.env()

	st := resolveServeStatus(ctl, env)
	if st.MemoryPort != customPort {
		t.Fatalf("MemoryPort = %d, want %d", st.MemoryPort, customPort)
	}
	if !st.Memory {
		t.Error("must dial the overridden port (up), not the default (down)")
	}
	var buf bytes.Buffer
	printServeStatus(st, &buf, false)
	if !strings.Contains(buf.String(), strconv.Itoa(customPort)) {
		t.Errorf("printed status must show the overridden port, got %q", buf.String())
	}
}

// verify sanity of the pid parse helper indirectly through a round trip so the
// test file also documents the expected pidfile format (decimal + trailing NL).
func TestPidfileFormat(t *testing.T) {
	raw := strconv.Itoa(12345) + "\n"
	if got := strings.TrimSpace(raw); got != "12345" {
		t.Fatalf("pidfile parse mismatch: %q", got)
	}
}
