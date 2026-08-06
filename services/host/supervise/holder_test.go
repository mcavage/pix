// holder_test.go — Holder's own concurrency contract, isolated from the
// go-plugin fixture: Use must never let a call start running against the
// dispensed impl AFTER Clear+Drain has already declared the unit fully
// drained. Run with -race; the fix also removes an unsynchronized
// WaitGroup.Add(1)-after-a-satisfied-Wait() pattern the race detector cannot
// see on its own (it is a logic race, not a memory race), so the invariant
// below is checked directly rather than left to -race alone.
package supervise

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestHolderUseRLockExcludesConcurrentClear is a DETERMINISTIC regression for
// the exact bug: the old Use() read the impl (via Get, under RLock, then
// released) and only incremented the drain refcount AFTER releasing that
// lock, leaving a window where a concurrent Clear (drops the impl) + Drain
// (waits on the refcount) could run entirely in between, report "drained",
// and let stopChild kill the unit while this call was still about to invoke
// fn against it.
//
// Rather than hoping a goroutine gets preempted inside that few-instruction
// window (empirically unreliable: a multi-minute stress run with the OLD
// split-then-add shape never reproduced it), this pins the interleaving
// exactly there via Holder's test-only afterCheckBeforeRegister seam: while
// Use is paused at that point (which the fix keeps INSIDE the read-lock
// section), a concurrent Clear must be unable to complete, because Lock()
// cannot proceed while any RLock is held. That is precisely the invariant
// that closes the race, proven every run, not occasionally.
func TestHolderUseRLockExcludesConcurrentClear(t *testing.T) {
	h := &Holder{}
	h.Set(struct{}{}, nil)

	clearReturned := make(chan struct{})
	proceed := make(chan struct{})
	var clearStarted sync.WaitGroup
	clearStarted.Add(1)
	h.afterCheckBeforeRegister = func() {
		go func() {
			clearStarted.Done()
			h.Clear()
			close(clearReturned)
		}()
		clearStarted.Wait()
		select {
		case <-clearReturned:
			t.Error("Clear() completed while Use still held the read lock before registering inflight — the read lock does not exclude it")
		case <-time.After(30 * time.Millisecond):
			// Expected: Clear is blocked acquiring the write lock.
		}
		close(proceed)
	}

	called := false
	err := h.Use(func(impl any) error {
		called = true
		<-proceed // hold the call open until the assertion above has run
		return nil
	})
	h.afterCheckBeforeRegister = nil
	if err != nil {
		t.Fatalf("Use failed: %v", err)
	}
	if !called {
		t.Fatal("fn never ran")
	}
	<-clearReturned // let the now-unblocked Clear finish so the goroutine exits cleanly
}

// TestHolderUseNeverMissesInflightBeforeDrain is a stress/soak companion to
// the deterministic test above: many callers hammering Use() while a
// supervisor loop repeatedly Set()s, Clear()s and Drain()s, asserting the
// same invariant end-to-end through the real API (no test seam) — a useful
// tripwire under -race across many iterations, even though the deterministic
// test above is what actually proves the fix rather than hopes for it.
func TestHolderUseNeverMissesInflightBeforeDrain(t *testing.T) {
	h := &Holder{}
	var running int32  // calls currently executing fn(impl)
	var violated int32 // set if Drain ever reported "drained" while running > 0

	fn := func(impl any) error {
		atomic.AddInt32(&running, 1)
		// Hold the call open briefly to widen the window a real caller
		// (an RPC health probe, a real unit call) would also occupy.
		time.Sleep(200 * time.Microsecond)
		atomic.AddInt32(&running, -1)
		return nil
	}

	const iterations = 3000
	stop := make(chan struct{})
	var callers sync.WaitGroup

	// Several goroutines hammering Use() as fast as they can, racing the
	// supervisor loop below on every possible interleaving.
	for w := 0; w < 8; w++ {
		callers.Add(1)
		go func() {
			defer callers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = h.Use(fn)
			}
		}()
	}

	// The supervisor side: Set (unit up), then immediately Clear+Drain (the
	// exact stopChild sequence), checking the invariant on every cycle.
	for i := 0; i < iterations; i++ {
		h.Set(struct{}{}, nil)
		h.Clear()
		if h.Drain(5*time.Millisecond) && atomic.LoadInt32(&running) != 0 {
			atomic.StoreInt32(&violated, 1)
		}
	}
	close(stop)
	callers.Wait()

	if atomic.LoadInt32(&violated) != 0 {
		t.Fatal("Drain reported every call drained while a Use call's fn was still executing")
	}
}

// TestHolderDrainLeaksNoGoroutinesOnTimeout proves a Drain that times out
// against a permanently stuck Use call costs NOTHING beyond its own call
// stack: Drain polls the atomic in-flight counter rather than spawning a
// goroutine to block on a WaitGroup, so unlike a goroutine-per-call design
// (which leaks one more blocked goroutine per restart that times out
// draining the same stuck call, unbounded over the process's lifetime),
// many repeated timed-out Drain calls must leave the goroutine count flat.
func TestHolderDrainLeaksNoGoroutinesOnTimeout(t *testing.T) {
	h := &Holder{}
	h.Set(struct{}{}, nil)

	release := make(chan struct{})
	started := make(chan struct{})
	go func() {
		_ = h.Use(func(impl any) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	runtime.GC()
	before := runtime.NumGoroutine()
	for i := 0; i < 50; i++ {
		if h.Drain(2 * time.Millisecond) {
			t.Fatal("Drain reported drained while a call was still blocked in fn")
		}
	}
	runtime.GC()
	after := runtime.NumGoroutine()
	// Allow a little slack for the runtime/test scheduler's own housekeeping
	// goroutines, but 50 timed-out drains must not have left 50 (or even a
	// handful of) new goroutines behind.
	if after > before+2 {
		t.Fatalf("goroutine count grew from %d to %d over 50 timed-out Drain calls", before, after)
	}

	close(release)
	if !h.Drain(time.Second) {
		t.Fatal("Drain did not observe completion of the released call")
	}
}
