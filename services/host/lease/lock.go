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

const pollInterval = 5 * time.Millisecond

// flockHandle is the shared flock-on-a-dedicated-file primitive behind both
// lock kinds here (RefLease's refs.lock and LifecycleLock's lifecycle.lock):
type flockHandle struct {
	f    *os.File
	path string
}

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

var errStaleLock = errors.New("lease: lock file was replaced or unlinked")

const maxStaleRetries = 32

func (h *flockHandle) validateLive() error {
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

func (h *flockHandle) reopen() error {
	_ = h.f.Close() // best-effort: this fd is being replaced either way
	f, err := openNoFollow(h.path, syscall.O_RDWR|syscall.O_CREAT, 0o600)
	if err != nil {
		return err
	}
	h.f = f
	return nil
}

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

// tryExclusiveValidated is tryExclusive plus validateLive's proof, reopening
// and retrying (bounded by maxStaleRetries) on a detected replace/unlink race.
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

// RefLease is a per-sandbox LIVE-REFERENCE lock: an flock on refs.lock inside
// the sandbox's lease directory.
type RefLease struct {
	h *flockHandle
}

// OpenRefLease opens (creating if absent) the refs.lock file inside dir.
func OpenRefLease(dir string) (*RefLease, error) {
	h, err := openFlockFile(dir, refsLockFileName)
	if err != nil {
		return nil, err
	}
	return &RefLease{h: h}, nil
}

func (l *RefLease) Path() string { return l.h.Path() }

// Fd exposes the raw file descriptor, so a test can assert CLOEXEC and the
// kernel's release-on-close behavior on the real fd.
func (l *RefLease) Fd() uintptr { return l.h.Fd() }

func (l *RefLease) AcquireShared(ctx context.Context) error {
	return l.h.acquireValidated(ctx, syscall.LOCK_SH)
}

func (l *RefLease) TryExclusive() error { return l.h.tryExclusiveValidated() }

func (l *RefLease) Unlock() error { return l.h.unlock() }

func (l *RefLease) Close() error { return l.h.close() }
