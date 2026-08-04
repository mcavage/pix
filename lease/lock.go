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

// ErrHeld is returned by TryExclusive when another holder (shared or
// exclusive) currently has the lease.
var ErrHeld = errors.New("lease: held by another holder")

const lockFileName = "lease.lock"

// pollInterval bounds how often a deadline-bounded acquire retries
// LOCK_NB — flock has no native timeout, so a bounded poll is how "deadline"
// is implemented over it. Short enough that a deadline of a few hundred
// milliseconds is still meaningfully bounded, long enough not to spin a CPU.
const pollInterval = 5 * time.Millisecond

// Lease is a per-sandbox reference lock: an flock-backed advisory lock on a
// dedicated file inside the sandbox's lease directory.
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
type Lease struct {
	f    *os.File
	path string
}

// Open opens (creating if absent) the lease file inside dir. dir must already
// exist (via CreateRecord, or an explicit caller-driven ensureSandboxDir) —
// Open does not create it, only the lock file inside it, and refuses to
// operate through a symlink at either dir or the lock file itself. The lock
// file's fd is opened O_CLOEXEC (see openNoFollow) so a later exec by this
// process never hands a child a working reference to a lock it did not ask
// to hold.
func Open(dir string) (*Lease, error) {
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
	path := filepath.Join(dir, lockFileName)
	f, err := openNoFollow(path, syscall.O_RDWR|syscall.O_CREAT, 0o600)
	if err != nil {
		return nil, err
	}
	return &Lease{f: f, path: path}, nil
}

// Path returns the lock file path, for logging/diagnostics.
func (l *Lease) Path() string { return l.path }

// Fd exposes the raw file descriptor. It exists for tests asserting CLOEXEC
// and kernel-release behavior directly; production callers should not need
// it — use AcquireShared/AcquireExclusive/TryExclusive/Unlock instead.
func (l *Lease) Fd() uintptr { return l.f.Fd() }

// AcquireShared blocks (deadline-bounded via ctx) until it holds a SHARED
// advisory lock. Multiple processes may hold the shared lock at once; this
// is the "I am a live reference" declaration a keep-alive holder makes.
func (l *Lease) AcquireShared(ctx context.Context) error {
	return l.acquire(ctx, syscall.LOCK_SH)
}

// AcquireExclusive blocks (deadline-bounded via ctx) until it holds the
// EXCLUSIVE advisory lock. Exclusive excludes every shared holder too, so it
// also serves as a blocking wait for zero holders; TryExclusive is the
// non-blocking version used as a proof rather than a wait.
func (l *Lease) AcquireExclusive(ctx context.Context) error {
	return l.acquire(ctx, syscall.LOCK_EX)
}

func (l *Lease) acquire(ctx context.Context, how int) error {
	if err := flockDeadline(ctx, int(l.f.Fd()), how, l.path); err != nil {
		return err
	}
	return nil
}

// flockDeadline polls a non-blocking flock(how) on fd until it succeeds or
// ctx is done. It is the shared deadline-bounded primitive behind both
// Lease's acquires and keep.go's short-lived internal RMW guard.
func flockDeadline(ctx context.Context, fd int, how int, path string) error {
	for {
		err := syscall.Flock(fd, how|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return &os.PathError{Op: "flock", Path: path, Err: err}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("lease: %w acquiring lock on %s", ctx.Err(), path)
		case <-time.After(pollInterval):
		}
	}
}

// TryExclusive attempts the EXCLUSIVE lock exactly once, non-blocking.
// Success is the zero-holder proof: no shared holder and no other exclusive
// holder is alive right now, because the kernel would otherwise have
// refused. It does not retry, and it does not release automatically — call
// Unlock (or Close) when the caller is done acting on that proof. This is
// the primitive a lifecycle reaper calls before destroying sandbox state.
func (l *Lease) TryExclusive() error {
	err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return ErrHeld
		}
		return &os.PathError{Op: "flock", Path: l.path, Err: err}
	}
	return nil
}

// Unlock drops whatever lock this Lease currently holds. Safe to call when
// not currently holding one.
func (l *Lease) Unlock() error {
	if err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN); err != nil {
		return &os.PathError{Op: "flock", Path: l.path, Err: err}
	}
	return nil
}

// Close unlocks (best-effort — the kernel would do this on fd close anyway;
// this makes it immediate and explicit) and closes the underlying file.
func (l *Lease) Close() error {
	_ = l.Unlock()
	return l.f.Close()
}
