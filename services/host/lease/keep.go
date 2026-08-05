//go:build unix

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
