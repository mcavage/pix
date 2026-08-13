//go:build unix

package sys

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// flockWait bounds how long a caller will wait for the lock. A VARIABLE so a
// test can shrink it; nothing else may write it.
//
// Every section this lock protects is a short one — write the trust store,
// stage a wrapper, replace a pidfile — so a wait this long means the holder is
// wedged or gone, not busy.
var flockWait = 30 * time.Second

// flockPoll is how often an unavailable lock is retried. Short enough that
// ordinary contention is invisible, long enough not to spin a core.
const flockPoll = 25 * time.Millisecond

// withFlock takes an advisory exclusive lock on lockPath for the duration of fn:
// every caller serializes a short section it must actually perform.
//
// BOUNDED, deliberately. This was a plain blocking LOCK_EX, and a blocking wait
// with no deadline in a cross-process lock is a hang with no diagnosis: `pix
// pack use` stopped dead after registering, printing nothing, because its
// post-commit wrapper refresh was waiting on a lock some other process held —
// and a post-commit side effect is precisely the code that is allowed to fail
// with a note rather than take the terminal with it. The caller can report a
// timeout; it cannot report a wait that never ends.
//
// The error names the lock file, because the useful next step is finding out
// who holds it (`lsof <path>`).
func withFlock(lockPath string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return fmt.Errorf("create lock dir: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open lock %s: %w", lockPath, err)
	}
	defer f.Close()
	if err := acquireFlock(f, flockWait); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

// acquireFlock polls a NON-blocking lock until it is taken or the budget runs
// out. Polling rather than blocking is what makes the deadline enforceable at
// all: syscall.Flock has no timeout, and the alternative — a blocking Flock on
// a goroutine plus a select — leaks that goroutine for as long as the holder
// lives, still holding the fd.
func acquireFlock(f *os.File, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if err != syscall.EWOULDBLOCK {
			return fmt.Errorf("acquire lock %s: %w", f.Name(), err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for the lock %s — another pix process is holding it "+
				"(`lsof %s` names it); retry once it finishes", budget, f.Name(), f.Name())
		}
		time.Sleep(flockPoll)
	}
}
