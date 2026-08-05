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
	return AttachRefUnderLifecycle(ctx, dir, nil)
}

// AttachRefUnderLifecycle is AttachRef with a CRITICAL SECTION: it acquires
// the lifecycle lock EXCLUSIVE, runs fn while holding it, and only if fn
// succeeds goes on to acquire the refs SHARED lock — still under the
// lifecycle lock — before releasing lifecycle and returning the reference.
// AttachRef is exactly this with a nil fn, so there is ONE implementation of
// the ordering, not two that could drift.
//
// It exists because a lifecycle transition and the reference that survives it
// are one atomic step for the caller that performs it: an integration that
// CREATES a sandbox must have recorded the sandbox's immutable identity
// (and anything else a later attach or a reaper reads) BEFORE any other
// process is allowed to observe the transition as finished, and it must hold
// a live reference by then too — otherwise a reaper that acquired the
// lifecycle lock next would see a recorded sandbox with zero holders and be
// entitled to destroy it. Both halves happen inside this one EX window.
//
// fn MUST NOT call AttachRef, WithLifecycle, TryReapProof, or anything else
// that opens dir's lifecycle lock again: flock conflicts are per open file
// description, so a second open from the SAME process deadlocks against this
// one (the same reason keep.json has its own keep.lock guard). fn's error is
// returned unwrapped, no reference is taken, and the lifecycle lock is
// released.
//
// The lifecycle lock is held for as long as fn runs, so fn must be the
// TRANSITION only — never the sandbox's whole session. A caller that starts a
// long-lived child process inside fn is expected to return from fn as soon as
// the transition is recorded and to wait on that child AFTER this returns,
// with only the refs SHARED lock still held.
func AttachRefUnderLifecycle(ctx context.Context, dir string, fn func() error) (*RefLease, error) {
	lc, err := OpenLifecycleLock(dir)
	if err != nil {
		return nil, err
	}
	defer lc.Close()
	if err := lc.AcquireExclusive(ctx); err != nil {
		return nil, fmt.Errorf("lease: attach %s: acquiring lifecycle lock: %w", dir, err)
	}
	defer lc.Unlock()

	if fn != nil {
		if err := fn(); err != nil {
			return nil, err
		}
	}

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
