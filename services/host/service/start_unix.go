//go:build unix

// serve_start_unix.go holds the real detached-spawn + flock shims for lazy
// auto-start (M1: they use Setsid and syscall.Flock, which only exist on unix;
// serve_start_windows.go carries the graceful-degrade shims so GOOS=windows

package service

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"pix/host/config"
)

// spawnDetachedServe is the real detached-spawn shim (kept a thin one-liner-ish
// wrapper like DefaultCtl's syscalls; everything around it is tested via
// fakes). Setsid gives the child its own session so a terminal close / SIGHUP
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

// ReadFileNoSymlink reads path via O_NOFOLLOW so the open itself atomically
// refuses a symlink — there is no separate Lstat-then-open TOCTOU window for
// an attacker to swap the target in (round 2, H8: tailFileLines' read side).
func ReadFileNoSymlink(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// tryServeSpawnLock attempts (NON-blocking) the exclusive flock that serializes
// the spawn decision on config.ServeSpawnLockPath(): two concurrent `pix
// run`s cannot both fork a daemon. (false, nil) = the lock is busy; Ensure
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
