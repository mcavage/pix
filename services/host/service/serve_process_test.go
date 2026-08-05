//go:build unix

// serve_process_test.go is the REAL-process / REAL-file layer under the four
// `serve` invariants the scripted-fake tests can only model: PID ownership
// (never signal a process we did not prove is ours), the spawn lock (two
// launchers cannot both fork a daemon), the detached spawn (own session, own
// log), and the launchd managed-stop path (stopped THROUGH its supervisor).
// Fakes prove the decision table; these prove the syscalls underneath it.
package service

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
)

// startProcNamed launches a REAL long-lived process whose argv[0] basename is
// `name` and whose argv[1] is `arg`, WITHOUT compiling a helper binary: a
// symlink named `name` pointing at /bin/sh, invoked with a script file called
// `arg` in its working directory. /proc/<pid>/cmdline then reads exactly
// "<dir>/<name>\0<arg>\0" — which is what verifyServeProc inspects. The loop
// (rather than one long sleep) keeps the shell responsive to SIGTERM even under
// a shell that defers signals while a foreground child runs.
func startProcNamed(t *testing.T, name, arg string) (dir string, pid int) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no sh on PATH: %v", err)
	}
	dir = t.TempDir()
	bin := filepath.Join(dir, name)
	if err := os.Symlink(sh, bin); err != nil {
		t.Fatalf("symlink %s -> %s: %v", bin, sh, err)
	}
	if err := os.WriteFile(filepath.Join(dir, arg), []byte("while :; do sleep 0.05; done\n"), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}
	cmd := exec.Command(bin, arg)
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s %s: %v", bin, arg, err)
	}
	pid = cmd.Process.Pid
	// Reap CONCURRENTLY. In production the daemon is released (reparented to
	// init, which reaps it), so a test that held its child unreaped would leave a
	// zombie — still answering kill(pid,0) — and model a process state the real
	// launcher never sees.
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
	waitFor(t, func() bool { return procCmdline(t, pid) != "" }, "child to appear in /proc")
	return dir, pid
}

// procCmdline returns the child's argv as a space-joined string ("" when the
// pid is gone or /proc is unavailable).
func procCmdline(t *testing.T, pid int) string {
	t.Helper()
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return ""
	}
	return strings.Join(strings.Split(strings.Trim(string(b), "\x00"), "\x00"), " ")
}

// procSession reads the session id out of /proc/<who>/stat (field 6), the
// portable-enough answer to "did Setsid actually take" without x/sys.
func procSession(t *testing.T, who string) int {
	t.Helper()
	b, err := os.ReadFile("/proc/" + who + "/stat")
	if err != nil {
		t.Skipf("no /proc/%s/stat: %v", who, err)
	}
	// comm (field 2) is parenthesized and may contain spaces: split after it.
	fields := strings.Fields(string(b)[strings.LastIndex(string(b), ")")+1:])
	if len(fields) < 4 {
		t.Fatalf("unparseable /proc/%s/stat: %q", who, b)
	}
	sid, err := strconv.Atoi(fields[3]) // state ppid pgrp session
	if err != nil {
		t.Fatalf("session id in /proc/%s/stat: %v", who, err)
	}
	return sid
}

// waitFor polls cond for up to 5s, failing the test with what it was waiting on.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// realCtl is DefaultCtl with the state-dir paths pointed at a temp dir: every
// other op (read, remove, kill, verify) is the REAL one the daemon path uses.
func realCtl(pidPath, lazyPath string) serveCtl {
	ctl := DefaultCtl()
	ctl.pidPath = func() string { return pidPath }
	ctl.removeLazy = func() { _ = os.Remove(lazyPath) }
	ctl.discover = nil // discovery would reach the whole host's process table
	return ctl
}

// A real live process named `pix-host serve` is ours; a real live process with
// any other argv[0] is positively NOT ours (known, not-ours) — the check that
// immediately precedes every SIGTERM.
func TestVerifyServeProc_RealProcesses(t *testing.T) {
	if _, err := os.Stat("/proc/self/cmdline"); err != nil {
		t.Skip("no /proc: the ps path is covered by TestVerifyServeProcPS_Darwin")
	}
	_, servePid := startProcNamed(t, "pix-host", "serve")
	if ours, known := verifyServeProc(servePid); !ours || !known {
		t.Errorf("verifyServeProc(%d) = (%v,%v) for %q, want (true,true)", servePid, ours, known, procCmdline(t, servePid))
	}
	_, otherPid := startProcNamed(t, "notpix", "serve")
	if ours, known := verifyServeProc(otherPid); ours || !known {
		t.Errorf("verifyServeProc(%d) = (%v,%v) for %q, want (false,true)", otherPid, ours, known, procCmdline(t, otherPid))
	}
}

// Stop against a REAL verified-ours process: SIGTERM lands, the process is gone,
// and both the pidfile and the lazy marker are cleared.
func TestStopServe_RealProcessTerminatesAndClearsState(t *testing.T) {
	_, pid := startProcNamed(t, "pix-host", "serve")
	state := t.TempDir()
	pidPath := filepath.Join(state, "serve.pid")
	lazyPath := filepath.Join(state, "serve.lazy")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lazyPath, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	stopped, err := Stop(realCtl(pidPath, lazyPath), &buf)
	if err != nil || !stopped {
		t.Fatalf("Stop = (%v,%v), want (true,nil); output:\n%s", stopped, err, buf.String())
	}
	if !strings.Contains(buf.String(), "SIGTERM") {
		t.Errorf("want a SIGTERM report, got %q", buf.String())
	}
	waitFor(t, func() bool { return syscall.Kill(pid, 0) != nil }, "the real process to exit")
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Errorf("pidfile survived a successful stop: %v", err)
	}
	if _, err := os.Stat(lazyPath); !os.IsNotExist(err) {
		t.Errorf("lazy marker survived a successful stop: %v", err)
	}
}

// PID ownership, the whole point: a pidfile pointing at a REAL process that is
// not ours is refused — no signal, and the pidfile is left for the user.
func TestStopServe_RealForeignPidRefused(t *testing.T) {
	_, pid := startProcNamed(t, "notpix", "serve")
	state := t.TempDir()
	pidPath := filepath.Join(state, "serve.pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	stopped, err := Stop(realCtl(pidPath, filepath.Join(state, "serve.lazy")), &buf)
	if err != nil || stopped {
		t.Fatalf("Stop = (%v,%v), want (false,nil)", stopped, err)
	}
	if !strings.Contains(buf.String(), "refusing to stop") {
		t.Errorf("want a refusal, got %q", buf.String())
	}
	if syscall.Kill(pid, 0) != nil {
		t.Error("the foreign process was signalled")
	}
	if _, err := os.Stat(pidPath); err != nil {
		t.Errorf("a refusal must keep the pidfile for the user: %v", err)
	}
	// The same ownership answer gates the start path's "someone is already
	// starting" branch: an unverifiable/foreign pid is NOT a running serve.
	if _, ok := readLiveServePid(realCtl(pidPath, "")); ok {
		t.Error("readLiveServePid accepted a foreign pid")
	}
}

// readLiveServePid accepts a REAL live `pix-host serve` (the idempotency check
// that keeps a second launcher from double-spawning).
func TestReadLiveServePid_RealProcess(t *testing.T) {
	_, pid := startProcNamed(t, "pix-host", "serve")
	state := t.TempDir()
	pidPath := filepath.Join(state, "serve.pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, ok := readLiveServePid(realCtl(pidPath, ""))
	if !ok || got != pid {
		t.Fatalf("readLiveServePid = (%d,%v), want (%d,true)", got, ok, pid)
	}
}

// The spawn lock is a REAL flock: while another open file description holds it,
// tryServeSpawnLock reports busy WITHOUT running the critical section, and it
// acquires again once the holder releases. This is what makes the "two `pix
// run`s cold-start once" test more than a mock agreement.
func TestTryServeSpawnLock_RealFlockExcludesAndReleases(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	lockPath := config.ServeSpawnLockPath()
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	holder, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("seed the lock: %v", err)
	}

	ran := false
	acquired, err := tryServeSpawnLock(func() error { ran = true; return nil })
	if acquired || err != nil {
		t.Fatalf("tryServeSpawnLock while held = (%v,%v), want (false,nil)", acquired, err)
	}
	if ran {
		t.Fatal("the critical section ran while the lock was held elsewhere")
	}

	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	acquired, err = tryServeSpawnLock(func() error { ran = true; return nil })
	if !acquired || err != nil || !ran {
		t.Fatalf("after release: acquired=%v err=%v ran=%v", acquired, err, ran)
	}
}

// The real detached spawn: the child lands in its OWN session (so a terminal
// close cannot SIGHUP it), its stdout/stderr land in the 0600 serve log, and the
// launcher-side pidfile write records the pid the caller got back.
func TestSpawnDetachedServe_RealChildSessionLogAndPidfile(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no sh on PATH: %v", err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "pix-host")
	if err := os.Symlink(sh, bin); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "serve"), []byte("echo up\nwhile :; do sleep 0.05; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir) // the child inherits cwd, where its `serve` script lives
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	logPath := filepath.Join(state, "pix", "serve.log")

	handle, err := spawnDetachedServe(bin, []string{"serve"}, logPath)
	if err != nil {
		t.Fatalf("spawnDetachedServe: %v", err)
	}
	defer func() { _ = syscall.Kill(handle.pid, syscall.SIGKILL) }()

	if err := recordSpawnedServePid(handle.pid); err != nil {
		t.Fatalf("recordSpawnedServePid: %v", err)
	}
	handle.release()

	sid, own := procSession(t, strconv.Itoa(handle.pid)), procSession(t, "self")
	if sid == 0 || sid == own {
		t.Errorf("child session %d vs launcher session %d: the spawn was not detached", sid, own)
	}
	waitFor(t, func() bool {
		b, err := os.ReadFile(logPath)
		return err == nil && strings.Contains(string(b), "up")
	}, "the child's output to reach the serve log")
	if fi, err := os.Stat(logPath); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf("serve log mode = %v, want 0600", fi.Mode().Perm())
	}
	raw, err := os.ReadFile(config.ServePidPath())
	if err != nil {
		t.Fatalf("read the recorded pidfile: %v", err)
	}
	if strings.TrimSpace(string(raw)) != strconv.Itoa(handle.pid) {
		t.Errorf("pidfile = %q, want %d", raw, handle.pid)
	}
	// And that recorded pid is exactly what the ownership probe accepts.
	if got, ok := readLiveServePid(realCtl(config.ServePidPath(), "")); !ok || got != handle.pid {
		t.Errorf("readLiveServePid = (%d,%v), want (%d,true)", got, ok, handle.pid)
	}
}

// AGENTS.md invariant #3: a MANAGED daemon is stopped through its supervisor.
// launchdStop boots the agent out of its domain (so KeepAlive stops respawning
// it) and leaves the plist ON DISK, written here by the real installer FS.
func TestLaunchdStop_BootsOutAndKeepsThePlist(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	run := &recRunner{}
	if err := launchdInstall(run.run, realInstallFS(), 501, home, "/usr/local/bin/pix-host", nil, &bytes.Buffer{}); err != nil {
		t.Fatalf("launchdInstall: %v", err)
	}
	plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.pix.serve.plist")
	if _, err := os.Stat(plistPath); err != nil {
		t.Fatalf("installer did not write the plist: %v", err)
	}

	var out bytes.Buffer
	run.calls = nil
	if err := launchdStop(run.run, 501, &out); err != nil {
		t.Fatalf("launchdStop: %v", err)
	}
	if len(run.calls) != 1 || run.calls[0] != "launchctl bootout gui/501/com.pix.serve" {
		t.Errorf("launchdStop calls = %v, want exactly a bootout", run.calls)
	}
	if _, err := os.Stat(plistPath); err != nil {
		t.Errorf("managed stop removed the plist (that is uninstall, not stop): %v", err)
	}
	if !strings.Contains(out.String(), "stays installed") {
		t.Errorf("want the stays-installed explanation, got %q", out.String())
	}
}

// StopAnyMode routes a managed daemon to the supervisor and NEVER to the pid
// path: the ctl here fails the test if any signal is attempted.
func TestStopAnyMode_ManagedNeverSignalsAPid(t *testing.T) {
	ctl := DefaultCtl()
	ctl.kill = func(int, syscall.Signal) error { t.Fatal("managed stop must not signal a pid"); return nil }
	ctl.readPid = func(string) (string, error) { t.Fatal("managed stop must not read the pidfile"); return "", nil }
	managedCalled := false
	stopped, err := StopAnyMode(
		func() bool { return true },
		func(io.Writer) error { managedCalled = true; return nil },
		ctl, &bytes.Buffer{})
	if err != nil || !stopped || !managedCalled {
		t.Fatalf("StopAnyMode(managed) = (%v,%v), managedCalled=%v", stopped, err, managedCalled)
	}
}
