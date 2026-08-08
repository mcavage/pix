//go:build unix

package lease

// U04c1 review fix: flock-on-a-path has a TOCTOU the naive open+flock
// sequence never closes. open(2) and flock(2) are two syscalls, not one, so
// a path that gets unlinked — or unlinked and recreated — between them (or
// even after a successful flock, before the caller acts on the "I hold it"
// result) can leave a caller holding a lock on an ORPHANED inode: the
// process genuinely holds a kernel flock, but nobody who opens the CURRENT
// path will ever contend with it, because they open a DIFFERENT inode. That
// is a stale lease that looks held but excludes no one — the exact bug
// class flockHandle.validateLive (lock.go) closes.
//
// These tests reproduce the "blocked old inode, then path recreation" shape
// deterministically and IN-PROCESS: rather than racing a timing window (which
// would be nondeterministic), each test drives the exact sequence by hand —
// open, then unlink+recreate (or unlink, or symlink) BEFORE the acquire call
// returns to the caller — and asserts the fix's two guarantees: (1) the
// caller never gets back a lock proven stale, and (2) the lock it DOES end
// up holding is genuinely effective against the CURRENT path, verified by a
// fresh independent handle correctly observing contention.

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// replaceFile unlinks path and creates a brand-new file in its place —
// deterministically simulating "another process (a reaper, a recreate-on-
// clean-boot path, ...) removed and recreated this lock file" without
// depending on timing.
func replaceFile(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove %s: %v", path, err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("recreate %s: %v", path, err)
	}
}

// assertGenuinelyHeld opens a FRESH RefLease at dir and confirms
// TryExclusive reports ErrHeld — the only way to prove a shared/exclusive
// holder's lock is actually on the file CURRENTLY linked at refs.lock,
// rather than an orphaned inode nobody opening the live path would ever see.
func assertGenuinelyHeld(t *testing.T, dir string) {
	t.Helper()
	prober, err := OpenRefLease(dir)
	if err != nil {
		t.Fatalf("OpenRefLease prober: %v", err)
	}
	defer prober.Close()
	if err := prober.TryExclusive(); !errors.Is(err, ErrHeld) {
		t.Errorf("fresh prober TryExclusive = %v, want ErrHeld (the held lock must be on the CURRENT path's inode)", err)
	}
}

func assertGenuinelyHeldLifecycle(t *testing.T, dir string) {
	t.Helper()
	prober, err := OpenLifecycleLock(dir)
	if err != nil {
		t.Fatalf("OpenLifecycleLock prober: %v", err)
	}
	defer prober.Close()
	if err := prober.TryExclusive(); !errors.Is(err, ErrHeld) {
		t.Errorf("fresh prober TryExclusive = %v, want ErrHeld (the held lock must be on the CURRENT path's inode)", err)
	}
}

// --- flockHandle.validateLive, unit-level ---

func TestValidateLive_UnchangedFileIsValid(t *testing.T) {
	dir := mustDir(t)
	rl, err := OpenRefLease(dir)
	if err != nil {
		t.Fatalf("OpenRefLease: %v", err)
	}
	defer rl.Close()
	if err := rl.h.validateLive(); err != nil {
		t.Errorf("validateLive on an untouched fresh handle = %v, want nil", err)
	}
}

func TestValidateLive_UnlinkedWithoutRecreateIsStale(t *testing.T) {
	dir := mustDir(t)
	rl, err := OpenRefLease(dir)
	if err != nil {
		t.Fatalf("OpenRefLease: %v", err)
	}
	defer rl.h.f.Close()
	if err := os.Remove(rl.Path()); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := rl.h.validateLive(); !errors.Is(err, errStaleLock) {
		t.Errorf("validateLive after unlink (no recreate) = %v, want errStaleLock", err)
	}
}

func TestValidateLive_ReplacedByNewFileIsStale(t *testing.T) {
	dir := mustDir(t)
	rl, err := OpenRefLease(dir)
	if err != nil {
		t.Fatalf("OpenRefLease: %v", err)
	}
	defer rl.h.f.Close()
	replaceFile(t, rl.Path())
	if err := rl.h.validateLive(); !errors.Is(err, errStaleLock) {
		t.Errorf("validateLive after unlink+recreate = %v, want errStaleLock", err)
	}
}

func TestValidateLive_ReplacedBySymlinkIsRefusedNotStale(t *testing.T) {
	dir := mustDir(t)
	rl, err := OpenRefLease(dir)
	if err != nil {
		t.Fatalf("OpenRefLease: %v", err)
	}
	defer rl.h.f.Close()
	path := rl.Path()
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	elsewhere := dir + "-elsewhere"
	if err := os.Mkdir(elsewhere, 0o700); err != nil {
		t.Fatalf("Mkdir elsewhere: %v", err)
	}
	if err := os.Symlink(elsewhere, path); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	err = rl.h.validateLive()
	if err == nil {
		t.Fatal("validateLive with a symlink at path = nil, want refusal")
	}
	if errors.Is(err, errStaleLock) {
		t.Errorf("validateLive with a symlink at path returned errStaleLock (would trigger a retry), want a hard, non-retried refusal")
	}
}

// --- RefLease.AcquireShared / TryExclusive ---

func TestAcquireShared_SurvivesUnlinkRecreateAndIsGenuinelyHeld(t *testing.T) {
	dir := mustDir(t)
	rl, err := OpenRefLease(dir)
	if err != nil {
		t.Fatalf("OpenRefLease: %v", err)
	}
	defer rl.Close()

	// Simulate the race directly: the fd this RefLease holds now points at
	// an inode that has been unlinked and replaced, BEFORE the acquire ever
	// runs — the worst case, since the very first flock(2) this handle does
	// will succeed on the now-orphaned old inode.
	replaceFile(t, rl.Path())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := rl.AcquireShared(ctx); err != nil {
		t.Fatalf("AcquireShared after unlink+recreate = %v, want nil (transparent reopen+retry)", err)
	}
	assertGenuinelyHeld(t, dir)
}

func TestTryExclusive_SurvivesUnlinkRecreateAndIsGenuinelyHeld(t *testing.T) {
	dir := mustDir(t)
	rl, err := OpenRefLease(dir)
	if err != nil {
		t.Fatalf("OpenRefLease: %v", err)
	}
	defer rl.Close()

	replaceFile(t, rl.Path())

	if err := rl.TryExclusive(); err != nil {
		t.Fatalf("TryExclusive after unlink+recreate = %v, want nil (transparent reopen+retry)", err)
	}
	assertGenuinelyHeld(t, dir)
}

// TestAcquireShared_UnlinkWithoutRecreateSelfHeals proves the degrade case:
// if the lock file is unlinked and NEVER recreated by anyone else, the
// retry's reopen (O_CREAT) recreates it itself and proceeds — the same
// "creating if absent" contract OpenRefLease already has, just applied
// transparently mid-retry instead of only on the very first open.
func TestAcquireShared_UnlinkWithoutRecreateSelfHeals(t *testing.T) {
	dir := mustDir(t)
	rl, err := OpenRefLease(dir)
	if err != nil {
		t.Fatalf("OpenRefLease: %v", err)
	}
	defer rl.Close()

	if err := os.Remove(rl.Path()); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := rl.AcquireShared(ctx); err != nil {
		t.Fatalf("AcquireShared after unlink (no recreate) = %v, want nil (self-heals by recreating)", err)
	}
	if _, err := os.Stat(rl.Path()); err != nil {
		t.Errorf("lock file missing after self-heal: %v", err)
	}
	assertGenuinelyHeld(t, dir)
}

// TestAcquireShared_ContinuousReplaceRetriesWithinDeadline drives an
// adversarial version of the same race: a background goroutine keeps
// replacing the lock file in a tight loop for the whole span of a bounded
// context. AcquireShared must neither return a stale success early nor hang
// past the deadline — it keeps retrying, bounded by ctx, and returns a
// DeadlineExceeded-wrapped error once ctx expires (since something is
// replacing the file on essentially every attempt, it may never observe a
// stable file within the window).
//
// Root cause of this test's original flake (investigated, not guessed): it
// is a TEST-ONLY synchronization bug, not a production one. acquireValidated
// (lock.go) never returns success without validateLive proving the fd it
// holds is still the CURRENT path's inode — that proof is correct. The bug
// was this test's own racer goroutine: it mutated the file in an
// unconditional, uncoordinated loop with no idea whether AcquireShared had
// already won, so a stray unlink+recreate could still land in the gap
// between AcquireShared returning a genuine success and this test's fresh
// prober observing it — first because the original code only stopped the
// racer via a `defer` that (for the nil-err path) ran AFTER that observation
// already happened, and residually, even once that ordering is fixed, because
// closing a "please stop" channel can't interrupt a mutation the racer
// goroutine already committed to moments earlier on an independent OS
// thread. Both classes are closed the same way: the racer now goes through
// the SAME kernel flock arbitration AcquireShared itself uses (a fresh
// RefLease.TryExclusive() probe) before every mutation, so "is it safe to
// replace this file right now" is answered by the kernel, atomically, the
// instant before the racer acts — not by a side-channel signal that can go
// stale between being read and being acted on. Once AcquireShared genuinely
// holds the file, every subsequent racer probe conflicts (EX vs the held SH)
// and the racer stands down for good; the assertion below is therefore exact,
// not merely likely.
func TestAcquireShared_ContinuousReplaceRetriesWithinDeadline(t *testing.T) {
	dir := mustDir(t)
	rl, err := OpenRefLease(dir)
	if err != nil {
		t.Fatalf("OpenRefLease: %v", err)
	}
	defer rl.Close()

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			// Probe-before-mutate: only replace the file if a fresh,
			// independent handle can currently win an EX lock on it. If
			// AcquireShared already holds a genuine SH lock on the CURRENT
			// inode, this conflicts (ErrHeld) and the racer skips this
			// iteration instead of blindly unlinking out from under a
			// winner — the kernel, not a side-channel flag, is the
			// arbiter of "is it safe to replace this file right now".
			if prober, perr := OpenRefLease(dir); perr == nil {
				if prober.TryExclusive() == nil {
					os.Remove(rl.Path())
					os.WriteFile(rl.Path(), nil, 0o600)
				}
				prober.Close()
			}
			time.Sleep(time.Microsecond)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = rl.AcquireShared(ctx)
	elapsed := time.Since(start)

	// Stop the racer and BLOCK until it has fully exited before inspecting
	// any post-acquire state, so a straggler iteration already in flight
	// (harmless now that it self-checked before mutating) is never still
	// running when we assert. A `defer` placed before an early `return`
	// inside the `if err == nil` branch below still only runs AFTER that
	// branch's statements (including assertGenuinelyHeld) have executed, so
	// it can't serve as this barrier.
	close(stop)
	<-done

	if elapsed > 2*time.Second {
		t.Fatalf("AcquireShared under continuous replacement took %v, want bounded near the 150ms deadline (never hang)", elapsed)
	}
	// Either outcome is acceptable evidence the retry loop is bounded and
	// correct: it may win the race and return nil (holding a genuinely live
	// lock), or it may exhaust ctx and return a wrapped DeadlineExceeded.
	// What it must NEVER do is hang indefinitely or silently return a stale
	// success without a live, contendable lock.
	if err == nil {
		assertGenuinelyHeld(t, dir)
		return
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("AcquireShared under continuous replacement = %v, want nil or a wrapped context.DeadlineExceeded", err)
	}
}

// TestAcquireShared_ReplaceDuringBlockedAcquireIsGenuinelyHeld is the
// deterministic regression for the flake investigated in
// TestAcquireShared_ContinuousReplaceRetriesWithinDeadline. That test drives
// "replace lands while AcquireShared is in-flight" by racing a clock against
// a free-running background goroutine; the flake turned out to be in the
// TEST's own synchronization (see the barrier fix above), not in
// acquireValidated — but a clock race, even a fixed one, still can't PROVE
// the in-flight-replace path is exercised on every run. This test forces
// that exact shape by hand, with zero reliance on scheduling or sleeps:
//
//  1. A second handle takes an EX lock on the CURRENT inode BEFORE the
//     AcquireShared goroutine is even started, so — by program order, not
//     timing — its first flock(2) attempt is guaranteed to observe
//     contention and block on that same (about-to-be-replaced) inode.
//  2. The file is replaced while that flock(2) is still pending, then the
//     EX lock is released: the pending flock(2) now succeeds on the
//     now-orphaned inode, forcing validateLive to detect staleness and
//     reopen — deterministically exercising the mid-flight-replace path
//     that TestAcquireShared_ContinuousReplaceRetriesWithinDeadline could
//     only ever hit by chance.
//  3. AcquireShared must still return nil, and a fresh prober must still
//     observe the lock as genuinely held on the CURRENT path.
func TestAcquireShared_ReplaceDuringBlockedAcquireIsGenuinelyHeld(t *testing.T) {
	dir := mustDir(t)
	rl, err := OpenRefLease(dir)
	if err != nil {
		t.Fatalf("OpenRefLease: %v", err)
	}
	defer rl.Close()

	// Deterministically force "replace lands mid-acquire": take an EX lock
	// on the CURRENT inode first, so AcquireShared's first flock(2) is
	// guaranteed (by program order, not timing) to observe contention and
	// block on that same inode.
	blocker, err := OpenRefLease(dir)
	if err != nil {
		t.Fatalf("OpenRefLease blocker: %v", err)
	}
	if err := blocker.TryExclusive(); err != nil {
		t.Fatalf("blocker TryExclusive: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	acquireErr := make(chan error, 1)
	go func() { acquireErr <- rl.AcquireShared(ctx) }()

	// rl's goroutine is now guaranteed blocked on the pre-replace inode
	// (blocker still holds it). Replace the file out from under it, then
	// release the blocker so the pending flock(2) succeeds on the now-
	// orphaned inode, forcing exactly one validateLive-detected stale retry.
	replaceFile(t, rl.Path())
	if err := blocker.Close(); err != nil {
		t.Fatalf("blocker Close: %v", err)
	}

	select {
	case err = <-acquireErr:
	case <-time.After(3 * time.Second):
		t.Fatal("AcquireShared did not return after the forced mid-flight replace (want it to reopen and succeed)")
	}
	if err != nil {
		t.Fatalf("AcquireShared after forced mid-flight replace = %v, want nil (transparent reopen+retry)", err)
	}

	// No further mutation is in flight at this point (the racer this test
	// replaces was the source of the flake, not a needed ingredient): this
	// assertion is now unconditionally deterministic.
	assertGenuinelyHeld(t, dir)
}

// --- LifecycleLock.AcquireExclusive / TryExclusive ---

func TestLifecycleLock_AcquireExclusive_SurvivesUnlinkRecreateAndIsGenuinelyHeld(t *testing.T) {
	dir := mustDir(t)
	lc, err := OpenLifecycleLock(dir)
	if err != nil {
		t.Fatalf("OpenLifecycleLock: %v", err)
	}
	defer lc.Close()

	replaceFile(t, lc.Path())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := lc.AcquireExclusive(ctx); err != nil {
		t.Fatalf("AcquireExclusive after unlink+recreate = %v, want nil (transparent reopen+retry)", err)
	}
	assertGenuinelyHeldLifecycle(t, dir)
}

func TestLifecycleLock_TryExclusive_SurvivesUnlinkRecreateAndIsGenuinelyHeld(t *testing.T) {
	dir := mustDir(t)
	lc, err := OpenLifecycleLock(dir)
	if err != nil {
		t.Fatalf("OpenLifecycleLock: %v", err)
	}
	defer lc.Close()

	replaceFile(t, lc.Path())

	if err := lc.TryExclusive(); err != nil {
		t.Fatalf("TryExclusive after unlink+recreate = %v, want nil (transparent reopen+retry)", err)
	}
	assertGenuinelyHeldLifecycle(t, dir)
}

// --- Composed ordering helpers: AttachRefUnderLifecycle / TryReapProof ---
//
// These prove the fix reaches the composed operations too, not just the raw
// RefLease/LifecycleLock primitives. Each opens a FRESH LifecycleLock
// internally on every call, so exercising their OWN internal race (not just "it works when the
// file happens to already be replaced before the call starts", which proves
// nothing new) needs the replace to land AFTER that internal open but BEFORE
// the internal acquire completes. Rather than a timing-dependent background
// racer (nondeterministic: the composed call might win the race before the
// racer ever gets a turn), these tests force that exact window
// deterministically: hold the SAME lifecycle lock ourselves first, so the
// composed call's internal AcquireExclusive is GUARANTEED to block on an fd
// it has already opened on the pre-replace inode — replace the file while
// it waits, then release, and the composed call's acquire lands on the now-
// orphaned inode and must detect and recover exactly like the direct
// LifecycleLock.AcquireExclusive test above.

func TestAttachRef_SurvivesUnlinkRecreateRace(t *testing.T) {
	dir := mustDir(t)
	// Warm both lock files into existence.
	warm, err := AttachRefUnderLifecycle(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("warm AttachRef: %v", err)
	}
	if err := warm.Close(); err != nil {
		t.Fatalf("warm Close: %v", err)
	}

	blocker, err := OpenLifecycleLock(dir)
	if err != nil {
		t.Fatalf("OpenLifecycleLock blocker: %v", err)
	}
	lifecyclePath := blocker.Path()
	if err := blocker.TryExclusive(); err != nil {
		t.Fatalf("blocker TryExclusive: %v", err)
	}

	type attachResult struct {
		rl  *RefLease
		err error
	}
	resultCh := make(chan attachResult, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		rl, err := AttachRefUnderLifecycle(ctx, dir, nil)
		resultCh <- attachResult{rl, err}
	}()

	// Give AttachRef's goroutine time to open its OWN lifecycle.lock handle
	// (on the CURRENT, pre-replace inode) and enter flockDeadline's poll
	// loop, blocked behind blocker's held exclusive lock.
	time.Sleep(50 * time.Millisecond)

	// The real race: replace lifecycle.lock out from under AttachRef's
	// ALREADY-OPEN fd while it is still blocked waiting for blocker to
	// release. Its next successful flock(2) will land on the now-orphaned
	// inode unless the fix's post-acquire validate+reopen+retry catches it.
	replaceFile(t, lifecyclePath)

	if err := blocker.Unlock(); err != nil {
		t.Fatalf("blocker Unlock: %v", err)
	}
	blocker.Close()

	var res attachResult
	select {
	case res = <-resultCh:
	case <-time.After(2 * time.Second):
		t.Fatal("AttachRef never returned after the blocker released")
	}
	if res.err != nil {
		t.Fatalf("AttachRef under lifecycle-lock replace race = %v, want nil (transparent reopen+retry)", res.err)
	}
	defer res.rl.Close()

	// Prove AttachRef's retried lifecycle acquisition released cleanly (not
	// leaked on the orphaned inode, and not left held on the new one): a
	// fresh handle on the CURRENT lifecycle lock file must be immediately
	// acquirable.
	fresh, err := OpenLifecycleLock(dir)
	if err != nil {
		t.Fatalf("OpenLifecycleLock fresh: %v", err)
	}
	if err := fresh.TryExclusive(); err != nil {
		t.Errorf("fresh lifecycle TryExclusive after AttachRef returned = %v, want nil (AttachRef must not leak the lifecycle lock)", err)
	}
	fresh.Close()

	// And the refs lock AttachRef left held is genuinely live.
	if err := TryReapProof(dir, func() error { return nil }); !errors.Is(err, ErrHeld) {
		t.Errorf("TryReapProof after AttachRef under replace race = %v, want ErrHeld (ref must be genuinely live)", err)
	}
}

func TestTryReapProof_SurvivesUnlinkRecreateRace(t *testing.T) {
	dir := mustDir(t)
	// Warm both files (an attach opens lifecycle.lock and refs.lock).
	warm, err := AttachRefUnderLifecycle(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("warm AttachRef: %v", err)
	}
	if err := warm.Close(); err != nil {
		t.Fatalf("warm Close: %v", err)
	}

	lc, err := OpenLifecycleLock(dir)
	if err != nil {
		t.Fatalf("OpenLifecycleLock: %v", err)
	}
	lifecyclePath := lc.Path()
	lc.Close()
	rl, err := OpenRefLease(dir)
	if err != nil {
		t.Fatalf("OpenRefLease: %v", err)
	}
	refsPath := rl.Path()
	rl.Close()

	replaceFile(t, lifecyclePath)
	replaceFile(t, refsPath)

	ran := false
	if err := TryReapProof(dir, func() error { ran = true; return nil }); err != nil {
		t.Fatalf("TryReapProof after unlink+recreate = %v, want nil (zero-holder proof still reachable)", err)
	}
	if !ran {
		t.Error("TryReapProof's fn never ran")
	}
}
