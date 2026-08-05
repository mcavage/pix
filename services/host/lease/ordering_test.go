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
