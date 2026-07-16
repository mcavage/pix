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
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"pi-stack/host/config"
)

// errLockHeld is returned by acquireLock ONLY when the flock is already held by
// another process/fd (EWOULDBLOCK) — i.e. genuine contention. Any other failure
// (missing dir, permissions) is a different error and must NOT be reported as
// "another server is using the database".
var errLockHeld = errors.New("lock already held by another process")

// acquireLock takes an EXCLUSIVE, NON-BLOCKING advisory flock on path (creating
// the lock file 0600 if absent). It NEVER blocks: if the lock is already held by
// another process (or another open fd) it returns an error immediately
// (EWOULDBLOCK/EAGAIN wrapped), so callers can refuse cleanly instead of
// deadlocking. The returned release func drops the lock (LOCK_UN) and closes the
// fd; it is idempotent and safe to call from a defer and a signal handler both.
func acquireLock(path string) (release func(), err error) {
	// Ensure the lock file's parent dir exists (0700). The lock lives next to the
	// memory db, and on a FRESH setup that dir isn't created until the store opens
	// — which happens AFTER the lock is taken. Without this, first-ever `serve`
	// fails with ENOENT (mislabeled as contention).
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create lock dir for %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("%w (%s)", errLockHeld, path)
		}
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

// lockMemoryStoreOrFatal is the shared prologue every LIVE-SERVING memory entry
// point runs BEFORE opening the store: it takes the exclusive, non-blocking
// advisory flock on config.MemoryLockPath() — the correctness primitive that
// makes the built-in `serve`, the bare `memory` daemon, the memory plugin
// self-exec, and `restore` all mutually exclusive around the sqlite db, closing
// the port-probe TOCTOU (the store opens before any port binds). It returns the
// release func to hold for the process lifetime and drop on shutdown. On failure
// (another memory server or a restore already holds it) it does NOT open the
// store — it fails fast through fatal, which MUST NOT return. Pass a
// cleanup-aware fatal (serve's fatalf, which runs supervisor shutdown before
// os.Exit); a nil fatal defaults to log.Fatalf. The acquire is indirected via
// acquireMemLockFn so tests can drive the refuse/free paths hermetically.
var acquireMemLockFn = acquireLock

func lockMemoryStoreOrFatal(fatal func(format string, a ...any)) func() {
	if fatal == nil {
		fatal = log.Fatalf
	}
	path := config.MemoryLockPath()
	release, err := acquireMemLockFn(path)
	if err != nil {
		if errors.Is(err, errLockHeld) {
			fatal("memory: another memory server or a restore is already using the database (%s) — only one may hold it. Stop it first: pi-stack serve stop", path)
		} else {
			fatal("memory: could not acquire store lock at %s: %v", path, err)
		}
		return func() {} // unreachable once fatal exits; a no-op keeps callers defer-safe
	}
	return release
}
