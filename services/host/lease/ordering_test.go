//go:build unix

package lease

import (
	"context"
	"errors"
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

	rl, err := AttachRef(ctx, dir)
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

	shortCtx, shortCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer shortCancel()
	start := time.Now()
	if _, err := AttachRef(shortCtx, dir); !errors.Is(err, context.DeadlineExceeded) {
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
	rl, err := AttachRef(longCtx, dir)
	if err != nil {
		t.Fatalf("AttachRef after lifecycle release = %v, want success", err)
	}
	defer rl.Close()
}

// TestWithLifecycle_SerializesAgainstConcurrentTransition proves
// WithLifecycle actually excludes a second concurrent lifecycle transition,
// not merely "usually" — the second call blocks until the first's fn
// returns and releases the lock.
func TestWithLifecycle_SerializesAgainstConcurrentTransition(t *testing.T) {
	dir := mustDir(t)
	release := make(chan struct{})
	entered := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = WithLifecycle(ctx, dir, func() error {
			close(entered)
			<-release
			return nil
		})
	}()

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first WithLifecycle never entered fn")
	}

	shortCtx, shortCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer shortCancel()
	start := time.Now()
	err := WithLifecycle(shortCtx, dir, func() error { return nil })
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("second WithLifecycle while first held = %v, want wrapping context.DeadlineExceeded", err)
	}
	if elapsed < 90*time.Millisecond {
		t.Errorf("second WithLifecycle returned after %v, want roughly the 100ms deadline", elapsed)
	}
	close(release)
}

// TestTryReapProof_FailsWhileRefsHeld proves the attach-vs-transition
// ordering from the transition side: a caller with a live refs SHARED
// holder (via AttachRef) makes TryReapProof refuse, non-blocking, and fn
// never runs.
func TestTryReapProof_FailsWhileRefsHeld(t *testing.T) {
	dir := mustDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	rl, err := AttachRef(ctx, dir)
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
	rl, err := AttachRef(ctx, dir)
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
		rl, err := AttachRef(ctx, dir)
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
