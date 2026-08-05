//go:build unix

// reset_process_test.go is reset's REAL-process / REAL-pidfile layer. Its two
// tests are the two ways the old MEMORY_PORT health probe answered the wrong
// question, each reproduced against a genuine live `pix-host serve`-argv process
// with NOTHING listening on :11435:
//
//  1. monitor-only / memory-crashed daemon: the port is silent, the daemon is
//     very much alive. The port probe read "down", so reset stopped it and never
//     brought it back (wasUp=false).
//  2. a stop that failed or refused: the port is silent, the daemon is still
//     alive. The port probe read "down", so reset moved the data dir out from
//     under a live sqlite writer AND deleted the pidfile that was the only handle
//     `pix serve stop` had on it.
package reset

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"pix/host/config"
	"pix/host/service"
)

// startServeLikeProc launches a REAL long-lived process whose /proc cmdline is
// exactly "<dir>/pix-host\0serve\0" — what the ownership check inspects — without
// compiling a helper: a symlink named pix-host pointing at /bin/sh, invoked with a
// script file named `serve`. The sleep loop (rather than one long sleep) keeps the
// shell responsive to SIGTERM.
func startServeLikeProc(t *testing.T) int {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no sh on PATH: %v", err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "pix-host")
	if err := os.Symlink(sh, bin); err != nil {
		t.Fatalf("symlink %s -> %s: %v", bin, sh, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "serve"), []byte("while :; do sleep 0.05; done\n"), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}
	cmd := exec.Command(bin, "serve")
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s serve: %v", bin, err)
	}
	pid := cmd.Process.Pid
	// Reap CONCURRENTLY: production releases the daemon to init, so a test holding
	// an unreaped child would leave a zombie — which still answers kill(pid,0) and
	// models a process state the launcher never sees.
	reaped := make(chan struct{})
	go func() { _ = cmd.Wait(); close(reaped) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-reaped:
		case <-time.After(5 * time.Second):
			t.Errorf("child %d did not exit", pid)
		}
	})
	waitUntil(t, func() bool { return alive(pid) }, "the child to be signalable")
	return pid
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// waitUntil polls cond for up to 5s, failing with what it was waiting on.
func waitUntil(t *testing.T, cond func() bool, what string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// realStopServe points the reset executor's stop at the REAL service.Stop, with
// the managed-service question answered "no" (asking launchd would ask the
// developer's own machine). config.ServePidPath() is XDG_STATE_HOME-derived, so
// with that env set to the test's state dir this is production's stop path acting
// on the test's pidfile — a real SIGTERM to a real process.
func realStopServe(t *testing.T) {
	t.Helper()
	orig := stopServeForReset
	stopServeForReset = func(out io.Writer) (bool, error) {
		return service.StopAnyMode(func() bool { return false }, service.StopManaged, service.DefaultCtl(), out)
	}
	t.Cleanup(func() { stopServeForReset = orig })
}

// realProbeServe restores the REAL identity probe for one test (the suite's
// TestMain keeps the pidfile half real and only stubs out the managed question;
// this asserts that explicitly for the tests that depend on it).
func realProbeServe(t *testing.T) {
	t.Helper()
	orig := probeServeUp
	probeServeUp = func(pidPath string, settle time.Duration) (bool, int) {
		return service.ServeIdentityUp(nil, pidPath, settle)
	}
	t.Cleanup(func() { probeServeUp = orig })
}

// hostFor builds an injected host whose $HOME and $XDG_STATE_HOME are the test's
// temp dirs, and seeds the config + data trees so a move is observable.
func hostFor(t *testing.T, home, state string) (resetHost, Paths) {
	t.Helper()
	env := resetHost{home: home, envVars: map[string]string{"XDG_STATE_HOME": state}}
	p := ResolveResetPaths(env)
	if err := os.MkdirAll(p.MemoryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.ConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(p.ConfigDir, "config.toml"), "x")
	writeFile(t, filepath.Join(p.MemoryDir, "memory.db"), "facts")
	return env, p
}

// Review scenario 1, reproduced: a REAL live `pix-host serve` with NOTHING
// listening on the memory port — a monitor-only daemon, or one whose memory
// service crashed. It must be seen as up (identity, not health), be really
// stopped, and be restarted on the clean slate. The old MEMORY_PORT probe read
// this daemon as down and left the host with no daemon at all.
func TestExecuteReset_MemoryPortSilentDaemonIsStoppedAndRestarted(t *testing.T) {
	pid := startServeLikeProc(t)
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state) // what config.ServePidPath() resolves through
	realStopServe(t)
	realProbeServe(t)
	restarted := stubRestartServe(t)

	env, p := hostFor(t, t.TempDir(), state)
	if p.PidFile != config.ServePidPath() {
		t.Fatalf("reset resolved pidfile %q, service resolves %q — the test would prove nothing", p.PidFile, config.ServePidPath())
	}
	if err := os.MkdirAll(filepath.Dir(p.PidFile), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, p.PidFile, strconv.Itoa(pid)+"\n")

	// The port is silent: this host has nothing listening anywhere.
	if env.DialLocal(11435) {
		t.Fatal("the fixture host must have no listener — that is the whole scenario")
	}

	var buf bytes.Buffer
	if _, err := executeReset(plan(resetCfg(), p, Opts{}), DefaultResetFS(), env, &buf, fixedNow); err != nil {
		t.Fatalf("executeReset: %v\n%s", err, buf.String())
	}

	waitUntil(t, func() bool { return !alive(pid) }, "the real daemon to be stopped")
	if !*restarted {
		t.Errorf("a daemon whose memory port is silent is still a daemon: reset must restart it\n%s", buf.String())
	}
	if exists(p.DataRoot) || !exists(p.DataRoot+".bak-"+fixedTS) {
		t.Error("a proven-dead daemon must not block the data move")
	}
	if exists(p.PidFile) {
		t.Error("a successful stop should leave no pidfile behind")
	}
}

// Review scenario 2, reproduced: the stop REFUSES (an unverifiable/hijacked
// pidfile is the production shape; here the stop simply reports it stopped
// nothing) while a REAL `pix-host serve` process is still alive and the memory
// port is silent. Everything destructive must be blocked: no data move, no
// runtime-file deletion, the pidfile preserved, no second daemon started — and
// the error must say so.
func TestExecuteReset_LiveDaemonAfterRefusedStopBlocksMoveAndKeepsPidfile(t *testing.T) {
	pid := startServeLikeProc(t)
	state := t.TempDir()
	realProbeServe(t)
	restarted := stubRestartServe(t)

	// A stop that refused: it signalled nothing and reports no error, exactly what
	// `Stop` does for a pid it cannot prove is ours.
	orig := stopServeForReset
	stopServeForReset = func(out io.Writer) (bool, error) { return false, nil }
	t.Cleanup(func() { stopServeForReset = orig })

	env, p := hostFor(t, t.TempDir(), state)
	if err := os.MkdirAll(filepath.Dir(p.PidFile), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, p.PidFile, strconv.Itoa(pid)+"\n")
	lock := filepath.Join(filepath.Dir(p.PidFile), "serve.spawn.lock")
	writeFile(t, lock, "")

	var buf bytes.Buffer
	_, err := executeReset(plan(resetCfg(), p, Opts{}), DefaultResetFS(), env, &buf, fixedNow)
	if err == nil {
		t.Fatalf("a live daemon must make reset fail loudly, got success:\n%s", buf.String())
	}
	if !strings.Contains(err.Error(), strconv.Itoa(pid)) {
		t.Errorf("the error should name the live pid %d, got %v", pid, err)
	}
	if !alive(pid) {
		t.Fatal("reset signalled the daemon itself — stopping is service.Stop's job")
	}
	if !exists(p.DataRoot) || exists(p.DataRoot+".bak-"+fixedTS) {
		t.Error("the data dir must NOT move while a live sqlite writer holds it")
	}
	if !exists(p.PidFile) {
		t.Error("the pidfile must survive: it is the only handle `pix serve stop` has on the live daemon")
	}
	if raw, rErr := os.ReadFile(p.PidFile); rErr != nil || strings.TrimSpace(string(raw)) != strconv.Itoa(pid) {
		t.Errorf("pidfile = %q (err %v), want the live pid %d", raw, rErr, pid)
	}
	if !exists(lock) {
		t.Error("the spawn lock must survive too — it is live state, not stale state")
	}
	if *restarted {
		t.Error("must not start a second daemon against a still-running one")
	}
	// The config dir (safe: no live writer) is still backed up.
	if exists(p.ConfigDir) || !exists(p.ConfigDir+".bak-"+fixedTS) {
		t.Error("the safe config-dir backup should still have happened")
	}
	// And the pidfile still classifies as OUR live daemon, which is what a
	// follow-up `pix serve stop` depends on.
	if up, gotPid := service.ServeIdentityUp(nil, p.PidFile, 0); !up || gotPid != pid {
		t.Errorf("ServeIdentityUp after the blocked reset = (%v,%d), want (true,%d)", up, gotPid, pid)
	}
}
