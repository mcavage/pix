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

// TryReapProof takes refs EX before lifecycle EX so simultaneous last-shell
// exits converge on one non-blocking, zero-reference reaper; see slim_test.go.
func TryReapProof(dir string, fn func() error) error {
	rl, err := OpenRefLease(dir)
	if err != nil {
		return err
	}
	defer rl.Close()
	if err := rl.TryExclusive(); err != nil {
		return err
	}
	defer rl.Unlock()

	lc, err := OpenLifecycleLock(dir)
	if err != nil {
		return err
	}
	defer lc.Close()
	if err := lc.TryExclusive(); err != nil {
		return err
	}
	defer lc.Unlock()
	return fn()
}

// ReferencesHeld reports whether any shell currently holds a live reference to
// this sandbox — the same question teardown asks, asked read-only.
//
// It exists because `pix ls` could not answer "why is this box still here". A
// user watching six sandboxes survive concluded that teardown-on-exit was
// broken; it was working perfectly and reporting kept-busy, because four of
// them had shells that never exited. The evidence was in a journal nobody reads
// and a lock nobody could see.
//
// The LOCK is the answer, never a PID: a pid is reused the instant its owner
// exits, and this package's threat model says nothing may treat CreatedPID as
// proof of liveness (see doc.go). Detecting shared holders requires trying the
// exclusive side, so this takes the ref lock for the microseconds it takes to
// fail or succeed. A teardown that races that window sees ErrHeld and KEEPS the
// sandbox — the fail-closed direction, so the worst case of asking is a box
// that survives one extra exit.
func ReferencesHeld(dir string) (held bool, err error) {
	rl, err := OpenRefLease(dir)
	if err != nil {
		return false, err
	}
	defer rl.Close()
	if terr := rl.TryExclusive(); terr != nil {
		if errors.Is(terr, ErrHeld) {
			return true, nil
		}
		return false, terr
	}
	defer rl.Unlock()
	return false, nil
}
