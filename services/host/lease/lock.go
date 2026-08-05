//go:build unix

package lease

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// ErrHeld is returned by TryExclusive (on either lock kind in this package)
// when another holder currently has it.
var ErrHeld = errors.New("lease: held by another holder")

const refsLockFileName = "refs.lock"

// pollInterval bounds how often a deadline-bounded acquire retries
// LOCK_NB — flock has no native timeout, so a bounded poll is how "deadline"
// is implemented over it. Short enough that a deadline of a few hundred
// milliseconds is still meaningfully bounded, long enough not to spin a CPU.
const pollInterval = 5 * time.Millisecond

// flockHandle is the shared flock-on-a-dedicated-file primitive behind both
// lock kinds in this package (RefLease's refs.lock and LifecycleLock's
// lifecycle.lock — see lifecycle.go). Factoring it out here is a structural
// move, not a behavioral one: U04c1 reshards what used to be a single
// lease.lock file into two files serving two different jobs (live-reference
// counting vs. lifecycle-transition serialization), and both need the exact
// same open/acquire/try/unlock/close machinery, hardening included.
type flockHandle struct {
	f    *os.File
	path string
}

// openFlockFile opens (creating if absent) the file named leaf inside dir.
// dir must already exist (via CreateRecord, or an explicit caller-driven
// ensureSandboxDir) — this does not create it, only the lock file inside it,
// and refuses to operate through a symlink at either dir or the lock file
// itself. The fd is opened O_CLOEXEC (see openNoFollow) so a later exec by
// this process never hands a child a working reference to a lock it did not
// ask to hold.
func openFlockFile(dir, leaf string) (*flockHandle, error) {
	if err := refuseSymlink(dir); err != nil {
		return nil, err
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("lease: sandbox dir %s: %w", dir, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("lease: %s is not a directory", dir)
	}
	path := filepath.Join(dir, leaf)
	f, err := openNoFollow(path, syscall.O_RDWR|syscall.O_CREAT, 0o600)
	if err != nil {
		return nil, err
	}
	return &flockHandle{f: f, path: path}, nil
}

func (h *flockHandle) Path() string { return h.path }
func (h *flockHandle) Fd() uintptr  { return h.f.Fd() }

func (h *flockHandle) tryExclusive() error {
	err := syscall.Flock(int(h.f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return ErrHeld
		}
		return &os.PathError{Op: "flock", Path: h.path, Err: err}
	}
	return nil
}

func (h *flockHandle) unlock() error {
	if err := syscall.Flock(int(h.f.Fd()), syscall.LOCK_UN); err != nil {
		return &os.PathError{Op: "flock", Path: h.path, Err: err}
	}
	return nil
}

func (h *flockHandle) close() error {
	_ = h.unlock()
	return h.f.Close()
}

// errStaleLock is the sentinel validateLive returns when the fd h holds no
// longer names the file currently linked at h.path: either it was unlinked,
// or replaced by a different file at the same path (unlink+recreate, or a
// rename into place). It signals "reopen and retry", never "give up" — see
// acquireValidated / tryExclusiveValidated, the only two ways this package
// ever completes an acquisition.
var errStaleLock = errors.New("lease: lock file was replaced or unlinked")

// maxStaleRetries bounds tryExclusiveValidated's stale-inode retry loop.
// TryExclusive is a non-blocking, single-instant proof — it must never wait
// on a lock genuinely HELD by someone else — but a replace/unlink race is a
// fast, bounded reopen-and-recheck, not a wait, so a small attempt budget
// (rather than a context deadline TryExclusive has never taken) is what
// closes the TOCTOU without changing its non-blocking contract. A path an
// attacker replaces on literally every open call is a denial-of-service, not
// a race this package can "win" by retrying harder; refusing after a bounded
// number of attempts is the correct failure mode there.
const maxStaleRetries = 32

// validateLive confirms h's fd still names the file CURRENTLY linked at
// h.path, closing the classic flock-on-a-path TOCTOU: open(2) and flock(2)
// are two syscalls, not one, so a path that gets unlinked — or unlinked and
// recreated — between them (or even after a successful flock, before the
// caller acts on it) can leave this handle holding a lock on an orphaned
// inode that nobody opening the CURRENT path will ever contend with: a lock
// that LOOKS held but excludes no one, the exact shape of a stale lease this
// package must never hand back.
//
// Three checks, all required, run in this order (lstat/symlink FIRST, so a
// hostile symlink replacement is refused even when the fd's old inode has
// ALSO already dropped to Nlink == 0 — the ordinary shape of "unlink, then
// symlink the same path"):
//
//  1. lstat(path) succeeds and is not a symlink — a missing path is treated
//     the same as an unlinked inode (see 2); a symlink is REFUSED OUTRIGHT,
//     never retried, because a symlink materializing at the lock path
//     mid-acquire is a hostile replacement, not a benign recreate race.
//  2. fstat(fd).Nlink > 0 — the inode this fd refers to is still linked from
//     some directory entry at all (an unlinked-but-still-open file reports
//     Nlink == 0 on Linux the instant the last link is removed).
//  3. os.SameFile(fstat, lstat) — the file CURRENTLY linked at path is the
//     SAME inode (device+inode) this fd refers to, not a different file that
//     was renamed or created into the same path after this fd was opened.
func (h *flockHandle) validateLive() error {
	// lstat(path) — and the symlink refusal — run FIRST, ahead of the fstat
	// nlink check: a hostile symlink replacement must be refused outright
	// even when the fd's old inode has ALSO already dropped to Nlink == 0
	// (the ordinary shape of "unlink, then put a symlink at the same path").
	// Checking nlink first would misclassify that as a retryable stale-lock
	// case instead of the hard, non-retried refusal it must be.
	lfi, err := os.Lstat(h.path)
	if err != nil {
		if os.IsNotExist(err) {
			return errStaleLock
		}
		return fmt.Errorf("lease: lstat %s: %w", h.path, err)
	}
	if lfi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("lease: refusing to follow symlink at %s", h.path)
	}
	ffi, err := h.f.Stat()
	if err != nil {
		return fmt.Errorf("lease: fstat %s: %w", h.path, err)
	}
	st, ok := ffi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("lease: fstat %s: unexpected stat type %T", h.path, ffi.Sys())
	}
	if st.Nlink == 0 {
		return errStaleLock
	}
	if !os.SameFile(ffi, lfi) {
		return errStaleLock
	}
	return nil
}

// reopen drops h's current fd and opens h.path fresh in its place — the
// "retry open" half of the retry-open-and-acquire loop in acquireValidated
// and tryExclusiveValidated, used only after validateLive reports
// errStaleLock. It goes through openNoFollow exactly like the original open
// did, so O_NOFOLLOW and O_CLOEXEC hold on every retry, not just the first
// attempt.
func (h *flockHandle) reopen() error {
	_ = h.f.Close() // best-effort: this fd is being replaced either way
	f, err := openNoFollow(h.path, syscall.O_RDWR|syscall.O_CREAT, 0o600)
	if err != nil {
		return err
	}
	h.f = f
	return nil
}

// acquireValidated blocks (deadline-bounded via ctx) until it holds a
// flock(how) on a fd PROVEN, by validateLive, to still name the file
// currently linked at h.path — reopening and re-acquiring on every
// replace/unlink race it detects, until either that proof succeeds or ctx's
// deadline elapses. Every exported AcquireShared/AcquireExclusive in this
// package goes through this, never the raw flockDeadline call directly, so
// the TOCTOU close applies uniformly to every blocking acquisition.
func (h *flockHandle) acquireValidated(ctx context.Context, how int) error {
	for {
		if err := flockDeadline(ctx, int(h.f.Fd()), how, h.path); err != nil {
			return err
		}
		verr := h.validateLive()
		if verr == nil {
			return nil
		}
		_ = h.unlock()
		if !errors.Is(verr, errStaleLock) {
			return verr
		}
		if ctx.Err() != nil {
			return fmt.Errorf("lease: %w acquiring lock on %s (still stale after a replace/unlink retry)", ctx.Err(), h.path)
		}
		if err := h.reopen(); err != nil {
			return err
		}
	}
}

// tryExclusiveValidated attempts the EXCLUSIVE lock non-blocking, exactly
// like tryExclusive, but additionally proves (via validateLive) that the fd
// it just locked still names the file currently linked at h.path —
// reopening and retrying, bounded by maxStaleRetries, on every replace/
// unlink race it detects. A genuine ErrHeld (someone else holds it right
// now) is returned immediately, never retried — only a stale-inode result
// triggers the reopen loop, and that loop is a handful of fast local
// syscalls, never a wait for another holder to release.
func (h *flockHandle) tryExclusiveValidated() error {
	for attempt := 0; ; attempt++ {
		if err := h.tryExclusive(); err != nil {
			return err
		}
		verr := h.validateLive()
		if verr == nil {
			return nil
		}
		_ = h.unlock()
		if !errors.Is(verr, errStaleLock) {
			return verr
		}
		if attempt >= maxStaleRetries {
			return fmt.Errorf("lease: %s replaced/unlinked on every attempt (%d retries)", h.path, maxStaleRetries)
		}
		if err := h.reopen(); err != nil {
			return err
		}
	}
}

// flockDeadline polls a non-blocking flock(how) on fd until it succeeds or
// ctx is done. It is the shared deadline-bounded primitive behind every
// blocking acquire in this package (RefLease, LifecycleLock, and keep.go's
// short-lived internal RMW guard).
//
// The poll uses ONE ticker for the whole wait, not a fresh time.After per
// iteration: time.After allocates a new Timer on every call and — inside a
// tight retry loop — leaves each of those timers live (and unstoppable) until
// it fires, one per poll instead of one per call. A single time.NewTicker is
// allocated once, reused for every tick, and its defer Stop() releases the
// underlying runtime timer the moment this function returns instead of
// leaving it to fire on its own.
func flockDeadline(ctx context.Context, fd int, how int, path string) error {
	tryLock := func() (bool, error) {
		err := syscall.Flock(fd, how|syscall.LOCK_NB)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return false, &os.PathError{Op: "flock", Path: path, Err: err}
		}
		return false, nil
	}
	if ok, err := tryLock(); ok || err != nil {
		return err
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("lease: %w acquiring lock on %s", ctx.Err(), path)
		case <-ticker.C:
			if ok, err := tryLock(); ok || err != nil {
				return err
			}
		}
	}
}

// RefLease is a per-sandbox LIVE-REFERENCE lock: an flock-backed advisory
// lock on refs.lock inside the sandbox's lease directory.
//
// Holding the SHARED lock is how a process declares "I am a live reference to
// this sandbox, do not reap it" — many processes may hold it at once.
// Successfully taking the EXCLUSIVE lock NON-BLOCKING (TryExclusive) is the
// proof that zero holders — shared or exclusive — currently exist: the only
// signal a reaper is allowed to trust before destroying sandbox state,
// because unlike a PID list it cannot be stale. The kernel releases every
// flock the instant its owning file descriptor closes, including on a
// SIGKILLed process, with no cleanup handler required and no window for a
// leaked reference.
//
// RefLease is deliberately narrow: it says nothing about lifecycle
// transitions (create/destroy/state change) — that serialization is
// LifecycleLock's job (lifecycle.go). See AttachRef/WithLifecycle/
// TryReapProof for how the two compose safely.
type RefLease struct {
	h *flockHandle
}

// Lease is a compatibility alias for RefLease, preserved because pre-reshard
// (U04a) callers may still spell the type this way.
//
// Deprecated: use RefLease.
type Lease = RefLease

// OpenRefLease opens (creating if absent) the refs.lock file inside dir.
func OpenRefLease(dir string) (*RefLease, error) {
	h, err := openFlockFile(dir, refsLockFileName)
	if err != nil {
		return nil, err
	}
	return &RefLease{h: h}, nil
}

// Open is a compatibility alias for OpenRefLease.
//
// Deprecated: use OpenRefLease.
func Open(dir string) (*Lease, error) { return OpenRefLease(dir) }

// Path returns the lock file path, for logging/diagnostics.
func (l *RefLease) Path() string { return l.h.Path() }

// Fd exposes the raw file descriptor. It exists for tests asserting CLOEXEC
// and kernel-release behavior directly; production callers should not need
// it — use AcquireShared/AcquireExclusive/TryExclusive/Unlock instead.
func (l *RefLease) Fd() uintptr { return l.h.Fd() }

// AcquireShared blocks (deadline-bounded via ctx) until it holds a SHARED
// advisory lock PROVEN to still be on the file currently linked at this
// lease's path (see flockHandle.validateLive) — a replace/unlink race
// detected immediately after the flock transparently reopens and retries,
// within ctx's deadline, rather than ever handing back a lock on an
// orphaned inode. Multiple processes may hold the shared lock at once; this
// is the "I am a live reference" declaration a keep-alive holder makes.
func (l *RefLease) AcquireShared(ctx context.Context) error {
	return l.h.acquireValidated(ctx, syscall.LOCK_SH)
}

// AcquireExclusive blocks (deadline-bounded via ctx) until it holds the
// EXCLUSIVE advisory lock. Exclusive excludes every shared holder too, so it
// also serves as a blocking wait for zero holders; TryExclusive is the
// non-blocking version used as a proof rather than a wait.
//
// Prefer TryExclusive (or TryReapProof) for the zero-holder proof: a
// blocking exclusive acquire on refs.lock can wait indefinitely behind a
// long-lived shared holder, which is never what a reaper wants. This method
// stays for callers that genuinely want to wait, deadline-bounded, for
// every current holder to drain. Like AcquireShared, a detected replace/
// unlink race transparently reopens and retries within ctx's deadline
// rather than returning a lock proven stale.
func (l *RefLease) AcquireExclusive(ctx context.Context) error {
	return l.h.acquireValidated(ctx, syscall.LOCK_EX)
}

// TryExclusive attempts the EXCLUSIVE lock exactly once, non-blocking.
// Success is the zero-holder proof: no shared holder and no other exclusive
// holder is alive right now, because the kernel would otherwise have
// refused. It does not retry, and it does not release automatically — call
// Unlock (or Close) when the caller is done acting on that proof. This is
// the primitive a lifecycle reaper calls before destroying sandbox state
// (see TryReapProof, which pairs it with LifecycleLock correctly). A
// replace/unlink race detected immediately after the flock is retried
// (bounded, non-blocking — see flockHandle.tryExclusiveValidated) rather
// than handed back as a stale zero-holder proof.
func (l *RefLease) TryExclusive() error { return l.h.tryExclusiveValidated() }

// Unlock drops whatever lock this RefLease currently holds. Safe to call
// when not currently holding one.
func (l *RefLease) Unlock() error { return l.h.unlock() }

// Close unlocks (best-effort — the kernel would do this on fd close anyway;
// this makes it immediate and explicit) and closes the underlying file.
func (l *RefLease) Close() error { return l.h.close() }
