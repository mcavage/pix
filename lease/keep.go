package lease

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// KeepState is a mutable, identity-bound marker: "this identity currently
// wants the sandbox kept alive". Unlike Record it can change, but only in
// ways that preserve the identity binding — nothing may clear or overwrite a
// keep set by a DIFFERENT identity. This is the primitive a higher-level
// reaper policy checks alongside the Lease's holder proof, without ever
// trusting a PID (Record.CreatedPID is advisory only) as proof of who set it.
type KeepState struct {
	Identity  string    `json:"identity"`
	UpdatedAt time.Time `json:"updated_at"`
}

const (
	keepFileName     = "keep.json"
	keepLockFileName = "keep.lock"
	// keepGuardTimeout bounds the internal read-modify-write guard around
	// keep.json. It is NOT a liveness signal like the Lease — it only
	// serializes a fast local file write — so a short, fixed bound is
	// correct: a caller stuck longer than this is stuck on something else.
	keepGuardTimeout = 2 * time.Second
)

// SetKeep records that identity wants dir's sandbox kept alive. It succeeds
// if no keep is currently set, or if the existing keep is already bound to
// the SAME identity (refreshing UpdatedAt). It is refused if a DIFFERENT
// identity currently holds the keep: ownership does not change hands by
// being overwritten, it must be explicitly cleared by its holder first.
func SetKeep(dir, identity string) error {
	if identity == "" {
		return errors.New("lease: empty identity")
	}
	return withKeepGuard(dir, func() error {
		path := filepath.Join(dir, keepFileName)
		existing, err := readKeepFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return err
			}
			existing = nil
		}
		if existing != nil && existing.Identity != identity {
			return fmt.Errorf("lease: keep on %s is held by %q, refusing to bind %q", dir, existing.Identity, identity)
		}
		state := &KeepState{Identity: identity, UpdatedAt: time.Now().UTC()}
		return writeKeepFile(path, state)
	})
}

// ClearKeep releases identity's keep on dir. It is a no-op if no keep is
// currently set, and refused if the keep is bound to a DIFFERENT identity —
// clearing someone else's keep is not this function's job; a caller building
// a force-clear policy on top must do so explicitly and visibly, not through
// this call silently succeeding.
func ClearKeep(dir, identity string) error {
	if identity == "" {
		return errors.New("lease: empty identity")
	}
	return withKeepGuard(dir, func() error {
		path := filepath.Join(dir, keepFileName)
		existing, err := readKeepFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if existing.Identity != identity {
			return fmt.Errorf("lease: keep on %s is held by %q, refusing to clear as %q", dir, existing.Identity, identity)
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("lease: remove keep %s: %w", path, err)
		}
		return nil
	})
}

// ReadKeep reports the current keep state for dir, and whether one is set.
func ReadKeep(dir string) (*KeepState, bool, error) {
	state, err := readKeepFile(filepath.Join(dir, keepFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return state, true, nil
}

// withKeepGuard serializes a read-modify-write on keep.json using a SEPARATE
// lock file (keep.lock), not the sandbox's reference Lease. Reusing the
// reference lease.lock here would be a correctness bug, not just a style
// choice: flock conflicts are per OPEN FILE DESCRIPTION, so a process that
// already holds a SHARED reference lock via one fd and then opened lease.lock
// again for an exclusive RMW guard via a second fd would deadlock against
// itself. keep.lock exists only to guard this file; it says nothing about
// whether the sandbox has live holders.
func withKeepGuard(dir string, fn func() error) error {
	path := filepath.Join(dir, keepLockFileName)
	f, err := openNoFollow(path, syscall.O_RDWR|syscall.O_CREAT, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	ctx, cancel := context.WithTimeout(context.Background(), keepGuardTimeout)
	defer cancel()
	if err := flockDeadline(ctx, int(f.Fd()), syscall.LOCK_EX, path); err != nil {
		return fmt.Errorf("lease: keep guard on %s: %w", dir, err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

func readKeepFile(path string) (*KeepState, error) {
	f, err := openNoFollow(path, syscall.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var state KeepState
	if err := json.NewDecoder(f).Decode(&state); err != nil {
		return nil, fmt.Errorf("lease: corrupt keep state at %s: %w", path, err)
	}
	return &state, nil
}

// writeKeepFile writes state to path via a 0600 temp file + rename, so a
// reader never observes a partially written keep.json.
func writeKeepFile(path string, state *KeepState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("lease: marshal keep state: %w", err)
	}
	tmp := path + ".tmp"
	f, err := openNoFollow(tmp, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("lease: write keep state %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("lease: close keep state %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("lease: rename keep state into place %s: %w", path, err)
	}
	return nil
}
