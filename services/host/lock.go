//go:build unix

// Advisory file locking (unix: linux + darwin, the only hosts pi-stack-host runs
// on). acquireLock is the correctness primitive that makes the memory daemon and
// `restore` mutually exclusive around the sqlite store: both take an EXCLUSIVE
// flock on the SAME lock file (config.MemoryLockPath()), so restore can never
// swap the db out from under a running daemon — even in the window where the
// daemon has opened the store but not yet bound its port (the TOCTOU the port
// probe alone leaves open).

package main

import (
	"fmt"
	"os"
	"sync"
	"syscall"
)

// acquireLock takes an EXCLUSIVE, NON-BLOCKING advisory flock on path (creating
// the lock file 0600 if absent). It NEVER blocks: if the lock is already held by
// another process (or another open fd) it returns an error immediately
// (EWOULDBLOCK/EAGAIN wrapped), so callers can refuse cleanly instead of
// deadlocking. The returned release func drops the lock (LOCK_UN) and closes the
// fd; it is idempotent and safe to call from a defer and a signal handler both.
func acquireLock(path string) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
		})
	}, nil
}
