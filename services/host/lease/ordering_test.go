//go:build unix

package lease

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestAttachRef_LeavesOnlyRefsSharedHeld proves AttachRef's end state: the
// caller holds refs.lock SHARED (so a zero-holder TryExclusive on refs is
// refused) but lifecycle.lock has already been released (so a lifecycle
// transition can still be acquired promptly — attach does not squat on it).
func TestAttachRef_LeavesOnlyRefsSharedHeld(t *testing.T) {
	dir := mustDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	rl, err := AttachRefUnderLifecycle(ctx, dir, nil)
	if err != nil {
		t.Fatalf("AttachRef: %v", err)
	}
	defer rl.Close()

	prober, err := OpenRefLease(dir)
	if err != nil {
		t.Fatalf("OpenRefLease prober: %v", err)
	}
	defer prober.Close()
	if err := prober.TryExclusive(); !errors.Is(err, ErrHeld) {
		t.Errorf("refs TryExclusive after AttachRef = %v, want ErrHeld (attach holds SH)", err)
	}

	lc, err := OpenLifecycleLock(dir)
	if err != nil {
		t.Fatalf("OpenLifecycleLock: %v", err)
	}
	defer lc.Close()
	if err := lc.TryExclusive(); err != nil {
		t.Errorf("lifecycle TryExclusive after AttachRef returned = %v, want nil (attach releases lifecycle promptly)", err)
	}
}

// TestAttachRef_BlocksBehindLifecycleHolderThenSucceeds proves the ordering
// invariant end to end within one process: while another party holds
// lifecycle.lock EXCLUSIVE, AttachRef cannot register a new refs SHARED
// holder — it must wait for the lifecycle lock, deadline-bounded via ctx —
// and it succeeds promptly once that party releases.
func TestAttachRef_BlocksBehindLifecycleHolderThenSucceeds(t *testing.T) {
	dir := mustDir(t)
	lc, err := OpenLifecycleLock(dir)
	if err != nil {
		t.Fatalf("OpenLifecycleLock: %v", err)
	}
	if err := lc.TryExclusive(); err != nil {
		t.Fatalf("lc.TryExclusive: %v", err)
	}

	// `start` before context.WithTimeout, not after: WithTimeout is what
	// anchors the deadline, so sampling the clock afterwards measures only the
	// tail of the window and silently subtracts any scheduling gap (GC or
	// preemption under -race on a loaded runner) from the measurement instead
	// of from the wait. That is the same defect that made the sibling test in
	// lifecycle_test.go fail CI at 87ms against a 100ms deadline. A context
	// deadline timer never fires early, so anchoring first makes the assertion
	// below sound by construction rather than by 10ms of luck.
	start := time.Now()
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer shortCancel()
	if _, err := AttachRefUnderLifecycle(shortCtx, dir, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("AttachRef while lifecycle held = %v, want wrapping context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed < 90*time.Millisecond {
		t.Errorf("AttachRef returned after %v, want roughly the 100ms deadline", elapsed)
	}

	if err := lc.Close(); err != nil { // releases lifecycle
		t.Fatalf("lc.Close: %v", err)
	}

	longCtx, longCancel := context.WithTimeout(context.Background(), time.Second)
	defer longCancel()
	rl, err := AttachRefUnderLifecycle(longCtx, dir, nil)
	if err != nil {
		t.Fatalf("AttachRef after lifecycle release = %v, want success", err)
	}
	defer rl.Close()
}

// TestTryReapProof_FailsWhileRefsHeld proves the attach-vs-transition
// ordering from the transition side: a caller with a live refs SHARED
// holder (via AttachRef) makes TryReapProof refuse, non-blocking, and fn
// never runs.
func TestTryReapProof_FailsWhileRefsHeld(t *testing.T) {
	dir := mustDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	rl, err := AttachRefUnderLifecycle(ctx, dir, nil)
	if err != nil {
		t.Fatalf("AttachRef: %v", err)
	}
	defer rl.Close()

	ran := false
	err = TryReapProof(dir, func() error { ran = true; return nil })
	if !errors.Is(err, ErrHeld) {
		t.Errorf("TryReapProof while a ref is attached = %v, want ErrHeld", err)
	}
	if ran {
		t.Error("TryReapProof ran fn despite a live ref holder")
	}
}

// TestTryReapProof_FailsWhileLifecycleHeld proves the other half: a
// concurrent lifecycle transition (or another attach mid-handshake) also
// makes TryReapProof refuse, non-blocking.
func TestTryReapProof_FailsWhileLifecycleHeld(t *testing.T) {
	dir := mustDir(t)
	lc, err := OpenLifecycleLock(dir)
	if err != nil {
		t.Fatalf("OpenLifecycleLock: %v", err)
	}
	defer lc.Close()
	if err := lc.TryExclusive(); err != nil {
		t.Fatalf("lc.TryExclusive: %v", err)
	}

	ran := false
	err = TryReapProof(dir, func() error { ran = true; return nil })
	if !errors.Is(err, ErrHeld) {
		t.Errorf("TryReapProof while lifecycle held = %v, want ErrHeld", err)
	}
	if ran {
		t.Error("TryReapProof ran fn despite a concurrent lifecycle holder")
	}
}

// TestTryReapProof_SucceedsAndRunsFnWithZeroHolders is the success path:
// with no attach and no concurrent lifecycle holder, TryReapProof proves
// zero refs holders and runs fn exactly once, propagating its return value.
func TestTryReapProof_SucceedsAndRunsFnWithZeroHolders(t *testing.T) {
	dir := mustDir(t)
	ran := 0
	if err := TryReapProof(dir, func() error { ran++; return nil }); err != nil {
		t.Fatalf("TryReapProof with zero holders = %v, want nil", err)
	}
	if ran != 1 {
		t.Errorf("fn ran %d times, want 1", ran)
	}

	sentinel := errors.New("boom")
	if err := TryReapProof(dir, func() error { return sentinel }); !errors.Is(err, sentinel) {
		t.Errorf("TryReapProof propagated %v, want sentinel", err)
	}
}

// TestTryReapProof_SucceedsAfterAttachedRefReleases closes the loop: once
// the attached ref releases, the SAME dir's reap proof succeeds.
func TestTryReapProof_SucceedsAfterAttachedRefReleases(t *testing.T) {
	dir := mustDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	rl, err := AttachRefUnderLifecycle(ctx, dir, nil)
	if err != nil {
		t.Fatalf("AttachRef: %v", err)
	}
	if err := TryReapProof(dir, func() error { return nil }); !errors.Is(err, ErrHeld) {
		t.Fatalf("TryReapProof while attached = %v, want ErrHeld", err)
	}
	if err := rl.Close(); err != nil {
		t.Fatalf("rl.Close: %v", err)
	}
	if err := TryReapProof(dir, func() error { return nil }); err != nil {
		t.Errorf("TryReapProof after release = %v, want nil", err)
	}
}

// --- U04c2: AttachRefUnderLifecycle -------------------------------------
//
// The composed create/attach transition: the caller's fn runs under the
// lifecycle EXCLUSIVE lock, and the refs SHARED lock is taken BEFORE that
// lock is released, so no other process can ever observe the transition as
// finished with zero holders.

// TestAttachRefUnderLifecycle_FnRunsUnderExclusiveLifecycle: while fn runs,
// the lifecycle lock is genuinely held — a would-be reaper's non-blocking
// proof reports ErrHeld rather than running.
func TestAttachRefUnderLifecycle_FnRunsUnderExclusiveLifecycle(t *testing.T) {
	dir := t.TempDir()
	ran := false
	rl, err := AttachRefUnderLifecycle(context.Background(), dir, func() error {
		ran = true
		if perr := TryReapProof(dir, func() error { return nil }); !errors.Is(perr, ErrHeld) {
			t.Errorf("inside fn, TryReapProof = %v, want ErrHeld (the lifecycle lock must be held)", perr)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("AttachRefUnderLifecycle: %v", err)
	}
	defer rl.Close()
	if !ran {
		t.Fatal("fn never ran")
	}
	// After returning: the REFS lock is held (so a reaper still cannot act)
	// but the LIFECYCLE lock is free (so another transition is not blocked
	// behind this session's lifetime).
	if perr := TryReapProof(dir, func() error { return nil }); !errors.Is(perr, ErrHeld) {
		t.Errorf("after attach, TryReapProof = %v, want ErrHeld (the reference must be held)", perr)
	}
	lc, err := OpenLifecycleLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lc.Close()
	if lerr := lc.TryExclusive(); lerr != nil {
		t.Errorf("after attach, the lifecycle lock is still held (%v) — it must cover the transition only", lerr)
	}
	lc.Unlock()
}

// TestAttachRefUnderLifecycle_FnErrorTakesNoReference: a refused transition
// returns fn's error unwrapped, takes no reference, and leaves BOTH locks
// free — nothing half-registered.
func TestAttachRefUnderLifecycle_FnErrorTakesNoReference(t *testing.T) {
	dir := t.TempDir()
	sentinel := errors.New("refused under the lock")
	rl, err := AttachRefUnderLifecycle(context.Background(), dir, func() error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("AttachRefUnderLifecycle err = %v, want the sentinel unwrapped", err)
	}
	if rl != nil {
		t.Fatal("a refused transition must return no reference")
	}
	if perr := TryReapProof(dir, func() error { return nil }); perr != nil {
		t.Errorf("after a refused transition, TryReapProof = %v, want nil (both locks free)", perr)
	}
}

// TestAttachRefUnderLifecycle_SecondAttachWaitsForTheTransition: a second
// attach cannot slip in while a transition is mid-flight; it acquires
// promptly once the transition releases. This is the in-process shape of the
// cross-process guarantee workflow/launch's session tests measure end to end.
func TestAttachRefUnderLifecycle_SecondAttachWaitsForTheTransition(t *testing.T) {
	dir := t.TempDir()
	inFn := make(chan struct{})
	finish := make(chan struct{})
	acquired := make(chan error, 1)

	go func() {
		rl, err := AttachRefUnderLifecycle(context.Background(), dir, func() error {
			close(inFn)
			<-finish
			return nil
		})
		if err == nil {
			defer rl.Close()
		}
		acquired <- err
	}()

	<-inFn
	second := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		rl, err := AttachRefUnderLifecycle(ctx, dir, nil)
		if err != nil {
			second <- -1
			return
		}
		defer rl.Close()
		second <- time.Since(start)
	}()

	select {
	case d := <-second:
		t.Fatalf("a second attach acquired in %s while a transition was still mid-flight", d)
	case <-time.After(50 * time.Millisecond):
	}
	close(finish)
	if err := <-acquired; err != nil {
		t.Fatalf("first attach: %v", err)
	}
	d := <-second
	if d < 0 {
		t.Fatal("second attach failed")
	}
}

// --- U-fix-lifecycle: the expired-original-ctx gap -----------------------
//
// fn (the create/attach transition) can legitimately run for the full
// sandbox create-poll window — up to fifteen minutes in production — while
// STILL holding the lifecycle lock. ctx, sized only for the lifecycle
// acquire that happens BEFORE fn runs, is therefore very likely already
// past its own deadline by the time AttachRefUnderLifecycle reaches the
// refs SHARED acquire AFTER fn returns. These tests prove that stale
// deadline cannot turn a merely BRIEF refs contention into "fn's
// already-started child is now live with no reference held".

// TestAttachRefUnderLifecycle_ExpiredOriginalCtx_BriefRefsContentionStillSucceeds
// is the core regression: with the original ctx already expired before fn
// even runs (exactly the shape a 15-minute create leaves behind), a refs.lock
// EXCLUSIVE holder that clears within milliseconds must not fail the attach —
// the refs acquire needs its OWN fresh budget, not ctx's spent one.
func TestAttachRefUnderLifecycle_ExpiredOriginalCtx_BriefRefsContentionStillSucceeds(t *testing.T) {
	dir := t.TempDir()

	holder, err := OpenRefLease(dir)
	if err != nil {
		t.Fatalf("OpenRefLease holder: %v", err)
	}
	if err := holder.TryExclusive(); err != nil {
		t.Fatalf("holder TryExclusive: %v", err)
	}
	timer := time.AfterFunc(50*time.Millisecond, func() {
		holder.Unlock()
		holder.Close()
	})
	defer timer.Stop()

	// Exactly the shape a 15-minute fn leaves ctx in: already past its
	// deadline before the refs acquire even starts.
	expired, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-expired.Done()

	fnRan := false
	rl, err := AttachRefUnderLifecycle(expired, dir, func() error {
		fnRan = true
		return nil
	})
	if err != nil {
		t.Fatalf("AttachRefUnderLifecycle with an expired original ctx + brief refs contention = %v, want nil (the refs acquire must use a FRESH budget, not ctx's spent one)", err)
	}
	defer rl.Close()
	if !fnRan {
		t.Fatal("fn never ran")
	}
}

// TestAttachRefUnderLifecycle_RefsContentionOutlastingFreshBudget_ReleasesBothLocks
// proves the failure mode this fix must still handle safely: contention that
// does NOT clear within the fresh budget still fails (bounded, not hung) and
// leaves neither lock held — lifecycle is free for the next transition, and
// nothing beyond fn's own side effects is left dangling inside this package.
// (workflow/launch's tests prove the caller-level consequence: a child
// already started by fn must be killed, never left running unreferenced.)
func TestAttachRefUnderLifecycle_RefsContentionOutlastingFreshBudget_ReleasesBothLocks(t *testing.T) {
	dir := t.TempDir()
	restore := RefAcquireTimeout
	RefAcquireTimeout = 40 * time.Millisecond
	t.Cleanup(func() { RefAcquireTimeout = restore })

	holder, err := OpenRefLease(dir)
	if err != nil {
		t.Fatalf("OpenRefLease holder: %v", err)
	}
	if err := holder.TryExclusive(); err != nil {
		t.Fatalf("holder TryExclusive: %v", err)
	}
	defer holder.Close() // held for the whole test: contention never clears

	expired, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-expired.Done()

	start := time.Now()
	_, err = AttachRefUnderLifecycle(expired, dir, func() error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("AttachRefUnderLifecycle under sustained refs contention = %v, want a wrapped context.DeadlineExceeded from the FRESH budget", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("AttachRefUnderLifecycle took %s, want bounded near the shrunk 40ms budget, not a hang", elapsed)
	}

	lc, err := OpenLifecycleLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lc.Close()
	if err := lc.TryExclusive(); err != nil {
		t.Errorf("lifecycle lock still held after a failed refs acquire = %v, want free", err)
	}
}

// TestAttachRefUnderLifecycle_LifecycleTimeoutNamesAnotherTransition proves
// requirement (3): a lifecycle-lock acquire that times out says so in terms
// an operator can act on, not just "context deadline exceeded".
func TestAttachRefUnderLifecycle_LifecycleTimeoutNamesAnotherTransition(t *testing.T) {
	dir := t.TempDir()
	blocker, err := OpenLifecycleLock(dir)
	if err != nil {
		t.Fatalf("OpenLifecycleLock blocker: %v", err)
	}
	defer blocker.Close()
	if err := blocker.TryExclusive(); err != nil {
		t.Fatalf("blocker TryExclusive: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = AttachRefUnderLifecycle(ctx, dir, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("AttachRefUnderLifecycle while lifecycle held = %v, want wrapping context.DeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), "another create or attach is in progress") {
		t.Errorf("error %q must identify another create/attach in progress", err.Error())
	}
}

// TestReferencesHeld_AnswersFromTheLockNotAPid: `pix ls` could not say why a
// sandbox was still there, so six surviving boxes read as "teardown is broken"
// when four of them simply had shells that had never exited. This is the
// read-only form of the question teardown asks, and it must answer from the
// LOCK — doc.go is explicit that a pid is reused the instant its owner exits
// and may never stand in for liveness.
func TestReferencesHeld_AnswersFromTheLockNotAPid(t *testing.T) {
	dir := t.TempDir()

	held, err := ReferencesHeld(dir)
	if err != nil {
		t.Fatalf("free lease: %v", err)
	}
	if held {
		t.Error("a lease nobody holds must report free — that is the box teardown removes")
	}

	// A second open file description is a genuinely independent holder: flock is
	// per-fd, so this is the same situation as another shell on the host.
	rl, err := OpenRefLease(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer rl.Close()
	if err := rl.AcquireShared(context.Background()); err != nil {
		t.Fatal(err)
	}

	held, err = ReferencesHeld(dir)
	if err != nil {
		t.Fatalf("held lease: %v", err)
	}
	if !held {
		t.Error("a shared reference must report held, or ls tells a user to expect a teardown that cannot happen")
	}

	// Releasing must flip it back: this is what makes the column track reality
	// rather than a one-time observation.
	if err := rl.Unlock(); err != nil {
		t.Fatal(err)
	}
	if held, err = ReferencesHeld(dir); err != nil || held {
		t.Errorf("after release: held=%v err=%v, want free", held, err)
	}
}

// TestReferencesHeld_DoesNotConsumeTheReference: asking must not become taking.
// If this probe left the exclusive side held, one `pix ls` would block every
// attach and teardown on the host.
func TestReferencesHeld_DoesNotConsumeTheReference(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		if held, err := ReferencesHeld(dir); err != nil || held {
			t.Fatalf("call %d: held=%v err=%v — the probe is holding its own lock", i, held, err)
		}
	}
	if err := TryReapProof(dir, func() error { return nil }); err != nil {
		t.Errorf("teardown could not prove zero references after ls asked: %v", err)
	}
}
