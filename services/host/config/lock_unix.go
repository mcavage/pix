//go:build unix

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// lock_unix.go is config's OWN advisory-lock primitive, deliberately NOT
// pix/host/sys's withFlock: sys.Real.StateDir already calls config.StateDir,
// so config importing sys back would be an import cycle (go build's own
// refusal, not a style preference). The three invariants that matter —
// bounded wait (never an undiagnosable hang), O_NOFOLLOW (never a
// confused-deputy write through a pre-planted symlink at the lock path),
// poll rather than a blocking Flock (so the deadline is actually
// enforceable) — are copied verbatim from sys/lock_unix.go; config is L0
// (foundation, sys is one layer up) so this is the same "duplicate a small
// primitive rather than invert the dependency" tradeoff environment.go's own
// optimalStringAlignmentDistance doc comment already documents for this
// package.
const (
	lockWait      = 30 * time.Second
	lockPoll      = 25 * time.Millisecond
	lockNoteAfter = time.Second
)

// withFileLock takes an advisory exclusive lock on lockPath for fn's
// duration, bounded at lockWait so a wedged or dead holder is a reported
// timeout, never a silent hang.
func withFileLock(lockPath string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return fmt.Errorf("create lock dir: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("open lock %s: %w", lockPath, err)
	}
	defer f.Close()
	deadline := time.Now().Add(lockWait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK {
			return fmt.Errorf("acquire lock %s: %w", lockPath, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for the lock %s — another pix process is holding it "+
				"(`lsof %s` names it); retry once it finishes", lockWait, lockPath, lockPath)
		}
		time.Sleep(lockPoll)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}
