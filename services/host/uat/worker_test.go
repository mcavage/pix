//go:build unix

package uat

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func mustRunnerState(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "session")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// newFakeConn stands in for a live worker socket connection: EnsureWorker
// only ever calls Close on what Dial returns.
func newFakeConn() net.Conn {
	c, _ := net.Pipe()
	return c
}

func TestEnsureWorker_AdoptsLiveWorkerWithoutSpawning(t *testing.T) {
	runnerState := mustRunnerState(t)
	spawnCalled := false
	deps := EnsureWorkerDeps{
		Dial: func(path string, attempts int, delay time.Duration) (net.Conn, error) {
			return newFakeConn(), nil // something is already listening
		},
		Spawn: func(argv []string) (*exec.Cmd, error) {
			spawnCalled = true
			return nil, errors.New("must not be called")
		},
	}
	started, err := EnsureWorker(deps, "/usr/local/bin/pix-host", "/repo", runnerState, "abcd1234")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if started {
		t.Fatal("adopting a live worker must report started=false")
	}
	if spawnCalled {
		t.Fatal("a live worker must never be replaced: Spawn must not be called")
	}
}

func TestEnsureWorker_AdoptLiveNeverRequiresRepoRootOrHostBinary(t *testing.T) {
	runnerState := mustRunnerState(t)
	deps := EnsureWorkerDeps{
		Dial: func(path string, attempts int, delay time.Duration) (net.Conn, error) {
			return newFakeConn(), nil
		},
		Spawn: func(argv []string) (*exec.Cmd, error) {
			return nil, errors.New("must not be called")
		},
	}
	// A relative repoRoot/hostBinary would normally be refused, but adopting a
	// live worker must never even look at them.
	started, err := EnsureWorker(deps, "not-absolute", "also-not-absolute", runnerState, "abcd1234")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if started {
		t.Fatal("adopting a live worker must report started=false")
	}
}

func TestEnsureWorker_StartsFreshOnAbsentWorkerWithExactArgvAndInheritedEnv(t *testing.T) {
	runnerState := mustRunnerState(t)
	dialCalls := 0
	var gotArgv []string
	var gotCmd *exec.Cmd
	deps := EnsureWorkerDeps{
		Dial: func(path string, attempts int, delay time.Duration) (net.Conn, error) {
			dialCalls++
			if dialCalls == 1 {
				return nil, errors.New("nothing listening yet") // probe: absent
			}
			return newFakeConn(), nil // readiness: now up
		},
		Spawn: func(argv []string) (*exec.Cmd, error) {
			gotArgv = argv
			cmd := exec.Command("sleep", "5")
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := cmd.Start(); err != nil {
				return nil, err
			}
			gotCmd = cmd
			return cmd, nil
		},
		ReadyAttempts: 3,
		ReadyDelay:    time.Millisecond,
	}
	defer func() {
		if gotCmd != nil {
			_ = gotCmd.Process.Kill()
			_ = gotCmd.Wait()
		}
	}()

	started, err := EnsureWorker(deps, "/usr/local/bin/pix-host", "/repo", runnerState, "abcd1234")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !started {
		t.Fatal("starting a fresh worker must report started=true")
	}
	want := []string{"/usr/local/bin/pix-host", "uat-worker", "--repo", "/repo", "--state", runnerState, "--session", "abcd1234"}
	if len(gotArgv) != len(want) {
		t.Fatalf("argv = %v, want %v", gotArgv, want)
	}
	for i := range want {
		if gotArgv[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q (argv=%v)", i, gotArgv[i], want[i], gotArgv)
		}
	}
	rec, err := ReadWorkerRecord(runnerState)
	if err != nil || rec == nil {
		t.Fatalf("expected a written worker record, got %v err=%v", rec, err)
	}
	if rec.SessionID != "abcd1234" || rec.PID != gotCmd.Process.Pid {
		t.Fatalf("recorded worker %+v does not match the spawned process (pid %d)", rec, gotCmd.Process.Pid)
	}
}

func TestDefaultWorkerSpawn_InheritsEnvUnchanged(t *testing.T) {
	cmd, err := defaultWorkerSpawn([]string{"sleep", "0.2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = cmd.Wait() }()
	if cmd.Env != nil {
		t.Fatalf("cmd.Env = %v, want nil (inherit the launcher's exact environment unchanged)", cmd.Env)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("expected the worker to start in its own process group (Setpgid)")
	}
}

func TestEnsureWorker_StartupFailureKillsTheProcessAndFailsClosed(t *testing.T) {
	runnerState := mustRunnerState(t)
	var gotCmd *exec.Cmd
	deps := EnsureWorkerDeps{
		Dial: func(path string, attempts int, delay time.Duration) (net.Conn, error) {
			return nil, errors.New("never comes up") // probe AND readiness both fail
		},
		Spawn: func(argv []string) (*exec.Cmd, error) {
			cmd := exec.Command("sleep", "300")
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := cmd.Start(); err != nil {
				return nil, err
			}
			gotCmd = cmd
			return cmd, nil
		},
		ReadyAttempts: 2,
		ReadyDelay:    time.Millisecond,
	}
	started, err := EnsureWorker(deps, "/usr/local/bin/pix-host", "/repo", runnerState, "abcd1234")
	if err == nil {
		t.Fatal("a worker that never becomes ready must fail closed")
	}
	if started {
		t.Fatal("a failed start must report started=false")
	}
	if rec, _ := ReadWorkerRecord(runnerState); rec != nil {
		t.Fatalf("a failed start must not record a worker: got %+v", rec)
	}
	if gotCmd == nil {
		t.Fatal("spawn was never invoked")
	}
	// The spawned process must actually be gone, not leaked.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(gotCmd.Process.Pid, 0) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("uat-worker pid %d is still alive after a failed readiness wait", gotCmd.Process.Pid)
}

func TestStopWorker_NoRecordIsANoOp(t *testing.T) {
	runnerState := mustRunnerState(t)
	if err := StopWorker(runnerState, "abcd1234"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestStopWorker_RefusesUnverifiableIdentityAndNeverSignals(t *testing.T) {
	runnerState := mustRunnerState(t)
	if err := writeWorkerRecord(runnerState, WorkerRecord{PID: 999999, SessionID: "abcd1234", Socket: SessionSocketPath(runnerState)}); err != nil {
		t.Fatal(err)
	}
	termCalled := false
	deps := stopDeps{
		alive:  func(pid int) bool { return true },
		verify: func(pid int, sessionID string) (bool, bool) { return false, false }, // "can't tell"
		term:   func(pid int) error { termCalled = true; return nil },
	}
	err := stopWorker(deps, runnerState, "abcd1234")
	if err == nil {
		t.Fatal("an unverifiable identity must refuse, not signal")
	}
	if termCalled {
		t.Fatal("must never signal a pid whose identity could not be verified")
	}
	// The record must be left in place for a later retry.
	if rec, _ := ReadWorkerRecord(runnerState); rec == nil {
		t.Fatal("an unverifiable record must not be dropped silently")
	}
}

func TestStopWorker_RefusesOnSessionMismatchAndNeverSignals(t *testing.T) {
	runnerState := mustRunnerState(t)
	if err := writeWorkerRecord(runnerState, WorkerRecord{PID: 4242, SessionID: "other-session", Socket: SessionSocketPath(runnerState)}); err != nil {
		t.Fatal(err)
	}
	termCalled := false
	deps := stopDeps{
		alive:  func(pid int) bool { termCalled = true; return true },
		verify: func(pid int, sessionID string) (bool, bool) { return true, true },
		term:   func(pid int) error { termCalled = true; return nil },
	}
	if err := stopWorker(deps, runnerState, "abcd1234"); err == nil {
		t.Fatal("a record for a different session must never be signalled")
	}
	if termCalled {
		t.Fatal("must never even probe/signal a pid recorded under a different session id")
	}
}

func TestStopWorker_DropsStaleRecordOnPidReuseWithoutSignaling(t *testing.T) {
	runnerState := mustRunnerState(t)
	if err := writeWorkerRecord(runnerState, WorkerRecord{PID: 4242, SessionID: "abcd1234", Socket: SessionSocketPath(runnerState)}); err != nil {
		t.Fatal(err)
	}
	termCalled := false
	deps := stopDeps{
		alive:  func(pid int) bool { return true },
		verify: func(pid int, sessionID string) (bool, bool) { return false, true }, // alive, but NOT ours (pid reuse)
		term:   func(pid int) error { termCalled = true; return nil },
	}
	if err := stopWorker(deps, runnerState, "abcd1234"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if termCalled {
		t.Fatal("a pid verified as NOT ours must never be signalled")
	}
	if rec, _ := ReadWorkerRecord(runnerState); rec != nil {
		t.Fatal("a pid-reuse record must be dropped so it cannot poison a future check")
	}
}

func TestStopWorker_TerminatesAndClearsAVerifiedLiveWorker(t *testing.T) {
	runnerState := mustRunnerState(t)
	if err := writeWorkerRecord(runnerState, WorkerRecord{PID: 4242, SessionID: "abcd1234", Socket: SessionSocketPath(runnerState)}); err != nil {
		t.Fatal(err)
	}
	termCalled := false
	deps := stopDeps{
		alive:  func(pid int) bool { return true },
		verify: func(pid int, sessionID string) (bool, bool) { return true, true },
		term:   func(pid int) error { termCalled = true; return nil },
	}
	if err := stopWorker(deps, runnerState, "abcd1234"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !termCalled {
		t.Fatal("a verified live worker must be terminated")
	}
	if rec, _ := ReadWorkerRecord(runnerState); rec != nil {
		t.Fatal("a stopped worker's record must be cleared")
	}
}

func TestStopWorker_AlreadyDeadPidJustClearsTheRecord(t *testing.T) {
	runnerState := mustRunnerState(t)
	if err := writeWorkerRecord(runnerState, WorkerRecord{PID: 4242, SessionID: "abcd1234", Socket: SessionSocketPath(runnerState)}); err != nil {
		t.Fatal(err)
	}
	termCalled := false
	deps := stopDeps{
		alive:  func(pid int) bool { return false },
		verify: func(pid int, sessionID string) (bool, bool) { termCalled = true; return true, true },
		term:   func(pid int) error { termCalled = true; return nil },
	}
	if err := stopWorker(deps, runnerState, "abcd1234"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if termCalled {
		t.Fatal("a pid that is already dead must never be verified or signalled, only cleared")
	}
	if rec, _ := ReadWorkerRecord(runnerState); rec != nil {
		t.Fatal("a dead-pid record must be cleared")
	}
}

func TestVerifyWorkerProc_MatchesOnlyPixHostUatWorkerForThisSession(t *testing.T) {
	run := func(name string, args ...string) ([]byte, error) {
		return []byte("/usr/local/bin/pix-host uat-worker --repo /repo --state /state --session abcd1234\n"), nil
	}
	ours, known := verifyWorkerProc(1234, "abcd1234", run)
	if !known || !ours {
		t.Fatalf("expected a matching pix-host uat-worker cmdline to verify, got ours=%v known=%v", ours, known)
	}

	ours, known = verifyWorkerProc(1234, "different-session", run)
	if !known || ours {
		t.Fatalf("a different session id in the recorded argv must not verify, got ours=%v known=%v", ours, known)
	}

	otherRun := func(name string, args ...string) ([]byte, error) {
		return []byte("/usr/bin/some-other-process --session abcd1234\n"), nil
	}
	ours, known = verifyWorkerProc(1234, "abcd1234", otherRun)
	if !known || ours {
		t.Fatalf("a non-pix-host process must not verify, got ours=%v known=%v", ours, known)
	}

	errRun := func(name string, args ...string) ([]byte, error) {
		return nil, errors.New("ps: no such pid")
	}
	ours, known = verifyWorkerProc(1234, "abcd1234", errRun)
	if known {
		t.Fatal("an unanswerable ps must report known=false")
	}
	if ours {
		t.Fatal("known=false must never also report ours=true")
	}
}

func TestEscalateSignal_TerminatesAProcessGroup(t *testing.T) {
	cmd := exec.Command("sleep", "300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()

	if err := escalateSignal(pid, done); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if syscall.Kill(pid, 0) == nil {
		t.Fatalf("pid %d is still alive after escalateSignal", pid)
	}
}

func TestWorkerArgv_MatchesUatWorkerFlagContract(t *testing.T) {
	argv := WorkerArgv("/bin/pix-host", "/repo", "/state/sessions/abcd1234", "abcd1234")
	joined := strings.Join(argv, " ")
	for _, want := range []string{"uat-worker", "--repo /repo", "--state /state/sessions/abcd1234", "--session abcd1234"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("argv %q missing %q", joined, want)
		}
	}
}
