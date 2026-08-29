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

	// Name is the sandbox name this instance was created under, set only by
	// CreateRecordFor (the create-intent recovery path — see
	// workflow/launch/createintent.go). It is ADDITIVE and OPTIONAL: a
	// record.json written before this field existed, or written through the
	// plain CreateRecord the pix-* name-keyed lifecycle already uses (where
	// the lease directory's own key already IS the name), decodes with
	// Name == "" and every existing reader — ReferencesHeld, ReadKeep, the
	// reap.go instance-id mismatch check — is unaffected, because none of
	// them look at it. Once non-empty, Name is immutable exactly like
	// InstanceID: a later CreateRecordFor call for the same dir with a
	// DIFFERENT non-empty name is refused, the same relabel refusal
	// InstanceID already gets.
	Name string `json:"name,omitempty"`
}

const recordFileName = "record.json"

func CreateRecord(dir, instanceID string) (*Record, error) {
	return createRecord(dir, instanceID, "")
}

// CreateRecordFor is CreateRecord plus the sandbox Name a create-intent
// recovery flow needs recorded alongside the instance id: the ONE thing a
// later fresh-probe removal decision (DecideEnvRemoval) must be able to
// re-derive from this directory alone, since the directory's own key is not
// guaranteed to be the sandbox name for an environment-identity-keyed create
// (unlike the pix-* workspace-keyed lifecycle, where it already is). name
// must be non-empty; use CreateRecord when there is no name to bind (the
// existing pix-* lifecycle, which needs none).
func CreateRecordFor(dir, instanceID, name string) (*Record, error) {
	if name == "" {
		return nil, fmt.Errorf("lease: CreateRecordFor requires a non-empty sandbox name")
	}
	return createRecord(dir, instanceID, name)
}

func createRecord(dir, instanceID, name string) (*Record, error) {
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
		if name != "" && existing.Name != "" && existing.Name != name {
			return nil, fmt.Errorf("lease: %s already records name %q, refusing to relabel as %q", path, existing.Name, name)
		}
		return existing, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	rec := &Record{InstanceID: instanceID, CreatedAt: time.Now().UTC(), CreatedPID: os.Getpid(), Name: name}
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
