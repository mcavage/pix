//go:build unix

package lease

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Record is the immutable identity of a sandbox instance: written exactly
// once at creation and never modified again. InstanceID and CreatedAt are
// what make it "immutable" in the load-bearing sense.
type Record struct {
	InstanceID string    `json:"instance_id"`
	CreatedAt  time.Time `json:"created_at"`

	// CreatedPID is the PID that created this record, kept ONLY for a human
	// reading the file on disk. PIDs are ADVISORY ONLY: a PID can be recycled
	// by an unrelated process the instant its owner exits, so nothing in this
	// package (or anything built on it) may treat CreatedPID as proof of
	// liveness, ownership, or exclusivity. The flock in lock.go is the only
	// correctness primitive; this field must never become one.
	CreatedPID int `json:"created_pid"`
}

const recordFileName = "record.json"

// CreateRecord writes the record for instanceID into dir (creating dir 0700
// if needed) exactly once and returns it. dir should come from SandboxDir.
//
// A second call for the SAME instanceID is a no-op that returns the EXISTING
// record unchanged — it does not overwrite CreatedAt/CreatedPID, because the
// record is immutable once created. A call naming a DIFFERENT instanceID than
// the one already on disk at dir is refused: a lease directory belongs to
// exactly one instance for its lifetime, and silently re-labelling it would
// let two different sandbox lifetimes alias the same directory.
func CreateRecord(dir, instanceID string) (*Record, error) {
	if err := ValidateInstanceID(instanceID); err != nil {
		return nil, err
	}
	if err := ensureSandboxDir(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, recordFileName)

	if existing, err := readRecordFile(path); err == nil {
		if existing.InstanceID != instanceID {
			return nil, fmt.Errorf("lease: %s already records instance %q, refusing to relabel as %q", path, existing.InstanceID, instanceID)
		}
		return existing, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	rec := &Record{InstanceID: instanceID, CreatedAt: time.Now().UTC(), CreatedPID: os.Getpid()}
	data, err := json.Marshal(rec)
	if err != nil {
		return nil, fmt.Errorf("lease: marshal record: %w", err)
	}

	// O_EXCL makes write-once a syscall-level guarantee, not merely an
	// application-level check-then-write: a concurrent creator loses the race
	// with EEXIST rather than silently clobbering the winner's record.
	f, err := openNoFollow(path, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return readRecordFile(path)
		}
		return nil, err
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return nil, fmt.Errorf("lease: write record %s: %w", path, err)
	}
	return rec, nil
}

// ReadRecord reads the existing immutable record from dir.
func ReadRecord(dir string) (*Record, error) {
	return readRecordFile(filepath.Join(dir, recordFileName))
}

func readRecordFile(path string) (*Record, error) {
	f, err := openNoFollow(path, syscall.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var rec Record
	if err := json.NewDecoder(f).Decode(&rec); err != nil {
		return nil, fmt.Errorf("lease: corrupt record at %s: %w", path, err)
	}
	return &rec, nil
}
