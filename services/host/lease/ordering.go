//go:build unix

package lease

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// RefAcquireTimeout bounds the refs SHARED acquire AttachRefUnderLifecycle
// makes AFTER fn returns. It is a package-level var, not a const: fn runs a
// create/attach transition that can legitimately run for the full sandbox
// create-poll window (launch.SbxCreatePollTimeout, currently fifteen
// minutes) while STILL holding the lifecycle lock, so by the time fn
// returns, ctx — sized only for the lifecycle-lock acquire above, seconds —
// is very likely already past its own deadline. flockDeadline's first
// attempt is a non-blocking try, so an UNCONTENDED refs acquire still
// succeeds instantly even on a dead ctx, which is exactly what hides this:
// the very first brief refs contention it hits — an exclusive holder that
// clears within microseconds — would otherwise fail immediately on the
// already-elapsed deadline instead of retrying, leaving fn's already-started
// child live with NO reference held. Production code never mutates this;
// tests (in this package and in workflow/launch) shrink it to keep a
// sustained-contention regression fast without waiting out the real budget.
var RefAcquireTimeout = 30 * time.Second

// AttachRefUnderLifecycle runs fn under the lifecycle EXCLUSIVE lock (the
// create/attach transition), then — before releasing that lock — takes the
// refs SHARED lock so no other process can ever observe the transition as
// finished with zero holders. The refs acquire uses its OWN fresh,
// RefAcquireTimeout-bounded context (see that var's doc), detached from
// ctx's deadline and cancellation: ctx bounds only the lifecycle acquire
// above fn, never the refs acquire after it.
func AttachRefUnderLifecycle(ctx context.Context, dir string, fn func() error) (*RefLease, error) {
	lc, err := OpenLifecycleLock(dir)
	if err != nil {
		return nil, err
	}
	defer lc.Close()
	if err := lc.AcquireExclusive(ctx); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("lease: attach %s: timed out waiting for the lifecycle lock — another create or attach is in progress on this sandbox: %w", dir, err)
		}
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
	refCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), RefAcquireTimeout)
	defer cancel()
	if err := rl.AcquireShared(refCtx); err != nil {
		rl.Close()
		return nil, fmt.Errorf("lease: attach %s: acquiring refs shared lock after the transition ran: %w", dir, err)
	}
	return rl, nil
}

// TryReapProof runs fn only when, under dir's lifecycle lock EXCLUSIVE, the refs
// lock's EXCLUSIVE can ALSO be proven non-blocking — zero live reference holders.
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
