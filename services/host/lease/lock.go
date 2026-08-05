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
func (h *flockHandle) acquire(ctx context.Context, how int) error {
	return flockDeadline(ctx, int(h.f.Fd()), how, h.path)
}

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
// advisory lock. Multiple processes may hold the shared lock at once; this
// is the "I am a live reference" declaration a keep-alive holder makes.
func (l *RefLease) AcquireShared(ctx context.Context) error {
	return l.h.acquire(ctx, syscall.LOCK_SH)
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
// every current holder to drain.
func (l *RefLease) AcquireExclusive(ctx context.Context) error {
	return l.h.acquire(ctx, syscall.LOCK_EX)
}

// TryExclusive attempts the EXCLUSIVE lock exactly once, non-blocking.
// Success is the zero-holder proof: no shared holder and no other exclusive
// holder is alive right now, because the kernel would otherwise have
// refused. It does not retry, and it does not release automatically — call
// Unlock (or Close) when the caller is done acting on that proof. This is
// the primitive a lifecycle reaper calls before destroying sandbox state
// (see TryReapProof, which pairs it with LifecycleLock correctly).
func (l *RefLease) TryExclusive() error { return l.h.tryExclusive() }

// Unlock drops whatever lock this RefLease currently holds. Safe to call
// when not currently holding one.
func (l *RefLease) Unlock() error { return l.h.unlock() }

// Close unlocks (best-effort — the kernel would do this on fd close anyway;
// this makes it immediate and explicit) and closes the underlying file.
func (l *RefLease) Close() error { return l.h.close() }
