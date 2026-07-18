//go:build unix

// serve_start_unix.go holds the real detached-spawn + flock shims for lazy
// auto-start (M1: they use Setsid and syscall.Flock, which only exist on unix;
// serve_start_windows.go carries the graceful-degrade shims so GOOS=windows
// still compiles). Everything around these thin wrappers is unit-tested via
// the injected serveStarter seams in serve_start.go.

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"pi-stack/host/config"
)

// spawnDetachedServe is the real detached-spawn shim (kept a thin one-liner-ish
// wrapper like defaultServeCtl's syscalls; everything around it is tested via
// fakes). Setsid gives the child its own session so a terminal close / SIGHUP
// to the launcher never reaches it. Round 3 (H10): Process.Release() is NO
// LONGER called here — it is handed to the caller as handle.release(), to be
// invoked ONLY after ensureServe's recordPid succeeds. Releasing unconditionally
// (the old behavior) meant a recordPid failure had no way to Wait()/reap the
// child after killing it (a released *os.Process can't be waited on), leaking a
// zombie and giving no confirmation the kill even landed. The launcher records
// the child pid in the pidfile immediately (H3); the daemon overwrites it with
// the same pid later.
func spawnDetachedServe(bin string, args []string, logPath string) (serveChildHandle, error) {
	logf, err := openServeLogFile(logPath)
	if err != nil {
		return serveChildHandle{}, err
	}
	defer logf.Close() // the child holds its own dup of the fd after Start
	cmd := exec.Command(bin, args...)
	cmd.Env = os.Environ()
	cmd.Stdin = nil
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // new session + process group
	if err := cmd.Start(); err != nil {
		return serveChildHandle{}, err
	}
	proc := cmd.Process
	return serveChildHandle{
		pid:     proc.Pid,
		kill:    proc.Kill,
		wait:    func() error { _, err := proc.Wait(); return err },
		release: func() { _ = proc.Release() }, // do NOT wait after this; we are launching, not supervising
	}, nil
}

// openServeLogFile opens (creating 0600) the lazy daemon's log for append,
// REFUSING to follow a symlink (H1): serve.log lives in a user-writable state
// dir, and a planted `serve.log -> /some/sensitive/file` symlink would make the
// daemon append its output into the target (corruption) and a failed-start
// tailFileLines echo the target's last lines to the terminal (secret leak). A
// pre-existing symlink is removed and replaced by a regular file; O_NOFOLLOW
// closes the Lstat→open TOCTOU window.
func openServeLogFile(logPath string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(logPath); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(logPath); err != nil {
			return nil, fmt.Errorf("refusing to log through symlink %s (and could not remove it: %v)", logPath, err)
		}
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EMLINK) {
			return nil, fmt.Errorf("refusing to log through symlink %s: %v", logPath, err)
		}
		return nil, err
	}
	return f, nil
}

// readFileNoSymlink reads path via O_NOFOLLOW so the open itself atomically
// refuses a symlink — there is no separate Lstat-then-open TOCTOU window for
// an attacker to swap the target in (round 2, H8: tailFileLines' read side).
func readFileNoSymlink(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// tryServeSpawnLock attempts (NON-blocking) the exclusive flock that serializes
// the spawn decision on config.ServeSpawnLockPath(): two concurrent `pi-stack
// run`s cannot both fork a daemon. (false, nil) = the lock is busy; ensureServe
// retries under its own deadline (M2), so a wedged holder can never hang the
// caller. The daemon's own memory-store flock remains the correctness backstop
// if this lock is ever lost (exotic NFS home).
func tryServeSpawnLock(fn func() error) (bool, error) {
	return tryFlock(config.ServeSpawnLockPath(), fn)
}

// tryFlock is the non-blocking sibling of withFlock: take
// syscall.Flock(LOCK_EX|LOCK_NB); if the lock is held elsewhere return
// (false, nil) so the caller can retry on its own schedule.
func tryFlock(lockPath string, fn func() error) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return false, fmt.Errorf("create lock dir: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, fmt.Errorf("open lock %s: %w", lockPath, err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return false, nil // busy — caller retries under its deadline
		}
		return false, fmt.Errorf("acquire lock %s: %w", lockPath, err)
	}
	// LIFO defers: unlock BEFORE close (closing the fd would also drop the lock,
	// but the explicit LOCK_UN keeps the release intent obvious).
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return true, fn()
}

// withFlock is the shared BLOCKING flock helper (factored out of withTaskLock
// so the flock dance is not duplicated): ensure the dir, open/create the lock
// file, take syscall.Flock(LOCK_EX), run fn, then ALWAYS release (LOCK_UN) +
// close via defer. Callers must not os.Exit inside fn. Blocking is correct for
// task-lifecycle ops (sub-second critical sections); the spawn path uses
// tryFlock above so it stays deadline-bounded.
func withFlock(lockPath string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return fmt.Errorf("create lock dir: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open lock %s: %w", lockPath, err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("acquire lock %s: %w", lockPath, err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}
