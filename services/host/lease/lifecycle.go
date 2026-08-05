//go:build unix

package lease

import (
	"context"
	"syscall"
)

const lifecycleLockFileName = "lifecycle.lock"

type LifecycleLock struct {
	h *flockHandle
}

func OpenLifecycleLock(dir string) (*LifecycleLock, error) {
	h, err := openFlockFile(dir, lifecycleLockFileName)
	if err != nil {
		return nil, err
	}
	return &LifecycleLock{h: h}, nil
}

func (l *LifecycleLock) Path() string { return l.h.Path() }

// AcquireExclusive blocks (deadline-bounded via ctx) until it holds the
// EXCLUSIVE lifecycle lock, serializing against every other lifecycle
// transition and against AttachRef's brief EX window. Callers are expected
// to hold this briefly and release it promptly (see WithLifecycle). Like
// RefLease.AcquireExclusive, a replace/unlink race detected immediately
// after the flock (see flockHandle.validateLive) transparently reopens and
// retries within ctx's deadline rather than returning a lock proven stale.
func (l *LifecycleLock) AcquireExclusive(ctx context.Context) error {
	return l.h.acquireValidated(ctx, syscall.LOCK_EX)
}

// TryExclusive attempts the EXCLUSIVE lifecycle lock exactly once,
// non-blocking. It returns ErrHeld if another lifecycle transition (or
// AttachRef) currently holds it. A replace/unlink race detected immediately
// after the flock is retried (bounded, non-blocking — see
// flockHandle.tryExclusiveValidated) rather than handed back as a stale
// lock.
func (l *LifecycleLock) TryExclusive() error { return l.h.tryExclusiveValidated() }

func (l *LifecycleLock) Unlock() error { return l.h.unlock() }

func (l *LifecycleLock) Close() error { return l.h.close() }
