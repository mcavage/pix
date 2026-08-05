//go:build unix

package lease

import (
	"context"
	"fmt"
)

// AttachRef registers a new live reference to the sandbox at dir without
// racing a concurrent lifecycle transition (create/destroy/state change).
//
// It acquires the lifecycle lock EXCLUSIVE (deadline-bounded via ctx), then
// — while still holding it — acquires the refs lock SHARED (the "I am live"
// declaration), then releases the lifecycle lock. The caller is left holding
// ONLY the refs SHARED lock, for as long as it wants to keep the sandbox
// alive; call Close on the returned *RefLease to release the reference.
//
// This ordering is what makes "attach" and "a lifecycle transition" safe to
// run concurrently without either blocking indefinitely on the other:
// WithLifecycle / TryReapProof also take the lifecycle lock EXCLUSIVE before
// doing anything, so the two can never interleave mid-registration — either
// this attach's SH acquire completes and is visible to a subsequent
// zero-holder proof, or a transition holds the lifecycle lock first and this
// attach blocks (up to ctx's deadline) until that transition releases it.
//
// AttachRef never requests the refs lock EXCLUSIVE — only SHARED — so it can
// never deadlock against another process's already-held SH: a shared lock
// never blocks a new shared acquire, by kernel definition. That is what
// keeps two concurrent attaches (see TestTwoHolders_AttachRefPromptly) fast
// regardless of ordering, while still serializing correctly against a
// transition.
func AttachRef(ctx context.Context, dir string) (*RefLease, error) {
	lc, err := OpenLifecycleLock(dir)
	if err != nil {
		return nil, err
	}
	defer lc.Close()
	if err := lc.AcquireExclusive(ctx); err != nil {
		return nil, fmt.Errorf("lease: attach %s: acquiring lifecycle lock: %w", dir, err)
	}
	defer lc.Unlock()

	rl, err := OpenRefLease(dir)
	if err != nil {
		return nil, fmt.Errorf("lease: attach %s: %w", dir, err)
	}
	if err := rl.AcquireShared(ctx); err != nil {
		rl.Close()
		return nil, fmt.Errorf("lease: attach %s: acquiring refs shared lock: %w", dir, err)
	}
	return rl, nil
}

// WithLifecycle runs fn while holding dir's lifecycle lock EXCLUSIVE
// (deadline-bounded via ctx), serializing it against every other lifecycle
// transition AND against any concurrent AttachRef's brief EX window. It is
// the primitive a caller builds "destroy", "transition state", etc. on top
// of; this package has no opinion on what fn does, and fn's error (if any)
// is returned unwrapped.
//
// Callers should keep fn brief: the lock is meant to be held briefly (see
// LifecycleLock's doc), not as a long-running critical section.
func WithLifecycle(ctx context.Context, dir string, fn func() error) error {
	lc, err := OpenLifecycleLock(dir)
	if err != nil {
		return err
	}
	defer lc.Close()
	if err := lc.AcquireExclusive(ctx); err != nil {
		return fmt.Errorf("lease: lifecycle %s: %w", dir, err)
	}
	defer lc.Unlock()
	return fn()
}

// TryReapProof runs fn only when, under dir's lifecycle lock EXCLUSIVE
// (serializing against any other lifecycle transition, including a
// concurrent AttachRef's brief EX window), the refs lock's EXCLUSIVE can
// also be proven non-blocking — zero live refs holders.
//
// Both checks are non-blocking: a reaper calling this is expected to back
// off and retry later on ErrHeld, never to block here. That non-blocking
// property is what makes it safe to call from a reaper's poll loop without
// a live keep-alive holder ever being able to stall it.
//
// fn's error is returned unwrapped when it runs; ErrHeld (or a real open/
// flock error) is returned instead, without running fn, when either lock is
// currently unavailable.
func TryReapProof(dir string, fn func() error) error {
	lc, err := OpenLifecycleLock(dir)
	if err != nil {
		return err
	}
	defer lc.Close()
	if err := lc.TryExclusive(); err != nil {
		return err
	}
	defer lc.Unlock()

	rl, err := OpenRefLease(dir)
	if err != nil {
		return err
	}
	defer rl.Close()
	if err := rl.TryExclusive(); err != nil {
		return err
	}
	defer rl.Unlock()
	return fn()
}
