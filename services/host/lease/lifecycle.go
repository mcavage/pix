//go:build unix

package lease

import (
	"context"
	"syscall"
)

const lifecycleLockFileName = "lifecycle.lock"

// LifecycleLock is a per-sandbox LIFECYCLE-TRANSITION lock: an flock-backed
// advisory lock on lifecycle.lock inside the sandbox's lease directory,
// separate from RefLease's refs.lock.
//
// U04c1 reshards what used to be one file (lease.lock, serving both jobs)
// into two, because the two jobs have different concurrency shapes:
//
//   - refs.lock (RefLease) has many concurrent SHARED holders, each just
//     declaring "I'm a live reference"; nothing about that declaration needs
//     to serialize against another live reference forming at the same time.
//   - lifecycle.lock (LifecycleLock) is EXCLUSIVE-ONLY: at most one lifecycle
//     transition (create/destroy/state change) may run at a time, and it is
//     meant to be HELD BRIEFLY — the deadline-bounded acquire here is a
//     short wait for the previous transition to finish, not a long hold.
//
// LifecycleLock never exposes a shared mode: there is no such thing as two
// processes "sharing" a lifecycle transition. See AttachRef, WithLifecycle,
// and TryReapProof (ordering.go) for the composed operations built on top of
// both locks together.
type LifecycleLock struct {
	h *flockHandle
}

// OpenLifecycleLock opens (creating if absent) the lifecycle.lock file
// inside dir. dir must already exist, and it refuses to operate through a
// symlink at either dir or the lock file itself — identical hardening to
// OpenRefLease, via the shared openFlockFile.
func OpenLifecycleLock(dir string) (*LifecycleLock, error) {
	h, err := openFlockFile(dir, lifecycleLockFileName)
	if err != nil {
		return nil, err
	}
	return &LifecycleLock{h: h}, nil
}

// Path returns the lock file path, for logging/diagnostics.
func (l *LifecycleLock) Path() string { return l.h.Path() }

// Fd exposes the raw file descriptor, for tests asserting CLOEXEC directly.
func (l *LifecycleLock) Fd() uintptr { return l.h.Fd() }

// AcquireExclusive blocks (deadline-bounded via ctx) until it holds the
// EXCLUSIVE lifecycle lock, serializing against every other lifecycle
// transition and against AttachRef's brief EX window. Callers are expected
// to hold this briefly and release it promptly (see WithLifecycle).
func (l *LifecycleLock) AcquireExclusive(ctx context.Context) error {
	return l.h.acquire(ctx, syscall.LOCK_EX)
}

// TryExclusive attempts the EXCLUSIVE lifecycle lock exactly once,
// non-blocking. It returns ErrHeld if another lifecycle transition (or
// AttachRef) currently holds it.
func (l *LifecycleLock) TryExclusive() error { return l.h.tryExclusive() }

// Unlock drops whatever lock this LifecycleLock currently holds. Safe to
// call when not currently holding one.
func (l *LifecycleLock) Unlock() error { return l.h.unlock() }

// Close unlocks (best-effort) and closes the underlying file.
func (l *LifecycleLock) Close() error { return l.h.close() }
