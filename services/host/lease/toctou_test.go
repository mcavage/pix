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

// --- RefLease.AcquireShared / AcquireExclusive / TryExclusive ---

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

func TestAcquireExclusive_SurvivesUnlinkRecreateAndIsGenuinelyHeld(t *testing.T) {
	dir := mustDir(t)
	rl, err := OpenRefLease(dir)
	if err != nil {
		t.Fatalf("OpenRefLease: %v", err)
	}
	defer rl.Close()

	replaceFile(t, rl.Path())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := rl.AcquireExclusive(ctx); err != nil {
		t.Fatalf("AcquireExclusive after unlink+recreate = %v, want nil (transparent reopen+retry)", err)
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
			os.Remove(rl.Path())
			os.WriteFile(rl.Path(), nil, 0o600)
			time.Sleep(time.Microsecond)
		}
	}()
	defer func() { close(stop); <-done }()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = rl.AcquireShared(ctx)
	elapsed := time.Since(start)

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

// --- Composed ordering helpers: AttachRef / WithLifecycle / TryReapProof ---
//
// These prove the fix reaches the composed operations too, not just the raw
// RefLease/LifecycleLock primitives — exactly the U04c1 review concern.
// AttachRef and WithLifecycle open a FRESH LifecycleLock internally on every
// call, so exercising their OWN internal race (not just "it works when the
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
	warm, err := AttachRef(context.Background(), dir)
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
		rl, err := AttachRef(ctx, dir)
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

func TestWithLifecycle_SurvivesUnlinkRecreateRace(t *testing.T) {
	dir := mustDir(t)
	blocker, err := OpenLifecycleLock(dir)
	if err != nil {
		t.Fatalf("OpenLifecycleLock blocker: %v", err)
	}
	path := blocker.Path()
	if err := blocker.TryExclusive(); err != nil {
		t.Fatalf("blocker TryExclusive: %v", err)
	}

	type wlResult struct {
		ran bool
		err error
	}
	resultCh := make(chan wlResult, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		ran := false
		err := WithLifecycle(ctx, dir, func() error { ran = true; return nil })
		resultCh <- wlResult{ran, err}
	}()

	time.Sleep(50 * time.Millisecond)
	replaceFile(t, path)
	if err := blocker.Unlock(); err != nil {
		t.Fatalf("blocker Unlock: %v", err)
	}
	blocker.Close()

	var res wlResult
	select {
	case res = <-resultCh:
	case <-time.After(2 * time.Second):
		t.Fatal("WithLifecycle never returned after the blocker released")
	}
	if res.err != nil {
		t.Fatalf("WithLifecycle under lifecycle-lock replace race = %v, want nil (transparent reopen+retry)", res.err)
	}
	if !res.ran {
		t.Error("WithLifecycle's fn never ran")
	}

	fresh, err := OpenLifecycleLock(dir)
	if err != nil {
		t.Fatalf("OpenLifecycleLock fresh: %v", err)
	}
	defer fresh.Close()
	if err := fresh.TryExclusive(); err != nil {
		t.Errorf("fresh lifecycle TryExclusive after WithLifecycle returned = %v, want nil (WithLifecycle must not leak the lock)", err)
	}
}

func TestTryReapProof_SurvivesUnlinkRecreateRace(t *testing.T) {
	dir := mustDir(t)
	// Warm both files.
	if err := WithLifecycle(context.Background(), dir, func() error { return nil }); err != nil {
		t.Fatalf("warm WithLifecycle: %v", err)
	}
	warm, err := AttachRef(context.Background(), dir)
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
