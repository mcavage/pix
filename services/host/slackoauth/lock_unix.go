//go:build unix

// FileLock is the production Locker: an advisory, cross-process EXCLUSIVE
// flock on a single path, so the refresh critical section (reread ->
// refresh -> write) is serialized across every process sharing the same
// underlying 1Password item, not just goroutines inside one process. Unlike
// pix-host's own NON-blocking acquireLock (which is a "refuse immediately"
// primitive for the memory store), Manager needs a caller to WAIT for a
// concurrent refresher to finish, so Lock blocks — polling a non-blocking
// flock at PollInterval until it succeeds, ctx is done, or a real error
// occurs (flock has no portable blocking call that also respects a Go
// context, so polling is the standard way to make it cancelable).

package slackoauth

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// defaultPollInterval is how often a blocked Lock retries the non-blocking
// flock while waiting for the current holder to release.
const defaultPollInterval = 25 * time.Millisecond

// FileLock is a Locker backed by a real, cross-process advisory flock.
type FileLock struct {
	// Path is the lock file (created 0600 if absent). Required.
	Path string
	// PollInterval overrides how often Lock retries while blocked. Zero
	// means the default (25ms).
	PollInterval time.Duration
}

// Lock blocks until it holds the EXCLUSIVE flock on Path, ctx is done, or a
// non-recoverable error occurs. The returned release drops the lock and
// closes the fd; it is idempotent and safe to call from a defer.
func (l *FileLock) Lock(ctx context.Context) (func(), error) {
	if l.Path == "" {
		return nil, fmt.Errorf("slackoauth: FileLock.Path is required")
	}
	if dir := filepath.Dir(l.Path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("slackoauth: create lock dir %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(l.Path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("slackoauth: open lock file %s: %w", l.Path, err)
	}

	interval := l.PollInterval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	for {
		flockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if flockErr == nil {
			break
		}
		if flockErr != syscall.EWOULDBLOCK && flockErr != syscall.EAGAIN {
			_ = f.Close()
			return nil, fmt.Errorf("slackoauth: lock %s: %w", l.Path, flockErr)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("slackoauth: lock %s: %w", l.Path, ctx.Err())
		case <-time.After(interval):
		}
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
		})
	}, nil
}
