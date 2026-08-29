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

// checkExistingRecord reads path (if present) and validates it against a
// requested instanceID/name, returning:
//
//   - (nil, nil) when nothing exists at path yet;
//   - (existing, nil) when something exists AND it matches exactly — the
//     idempotent "already recorded this" case, whether this call is a plain
//     repeat or the LOSER of a create race that lands on the SAME identity
//     the winner wrote;
//   - (nil, err) when something exists but records a DIFFERENT identity: a
//     relabel attempt, always refused. This is what makes "the loser of a
//     concurrent create race for a DIFFERENT identity is refused, never
//     silently handed the winner's record as if it were its own" true
//     regardless of WHERE the race is observed — the pre-write check below
//     and the post-EEXIST-or-post-link-failure re-check both route through
//     this one function, so neither can drift from the other's notion of
//     "matches".
func checkExistingRecord(path, instanceID, name string) (*Record, error) {
	existing, err := readRecordFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if existing.InstanceID != instanceID {
		return nil, fmt.Errorf("lease: %s already records instance %q, refusing to relabel as %q", path, existing.InstanceID, instanceID)
	}
	if name != "" && existing.Name != "" && existing.Name != name {
		return nil, fmt.Errorf("lease: %s already records name %q, refusing to relabel as %q", path, existing.Name, name)
	}
	return existing, nil
}

func createRecord(dir, instanceID, name string) (*Record, error) {
	if err := ValidateInstanceID(instanceID); err != nil {
		return nil, err
	}
	if err := ensureSandboxDir(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, recordFileName)

	if existing, err := checkExistingRecord(path, instanceID, name); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}

	rec := &Record{InstanceID: instanceID, CreatedAt: time.Now().UTC(), CreatedPID: os.Getpid(), Name: name}
	data, err := json.Marshal(rec)
	if err != nil {
		return nil, fmt.Errorf("lease: marshal record: %w", err)
	}

	switch werr := writeRecordOnce(path, data); {
	case werr == nil:
		return rec, nil
	case errors.Is(werr, os.ErrExist):
		// Lost a create race: something now exists at path (link(2) never
		// overwrites an existing name, so "lost" and "corrupt" cannot be
		// confused here — see writeRecordOnce). Re-validate it against what
		// THIS caller asked for through the SAME check the pre-write path
		// used, rather than trusting readRecordFile's raw result: a loser
		// racing a DIFFERENT identity must still be refused, never handed
		// the winner's record as if it were its own.
		existing, cerr := checkExistingRecord(path, instanceID, name)
		if cerr != nil {
			return nil, cerr
		}
		if existing == nil {
			// Vanishingly unlikely (the winner's own record vanished between
			// the failed link and this re-read) but not ours to paper over.
			return nil, fmt.Errorf("lease: lost a create race for %s but could not read the winner's record", path)
		}
		return existing, nil
	default:
		return nil, werr
	}
}

// writeRecordOnce durably installs data at path EXACTLY ONCE, and never
// leaves a reader observing a corrupt or partial final file even across a
// crash. The old direct approach — open path itself with
// O_CREAT|O_EXCL|O_WRONLY, then write into it — made write-once a syscall
// guarantee (a concurrent creator loses the race with EEXIST rather than
// clobbering the winner), but the EXCL'd name was reachable at open(2), one
// syscall before a single byte of content existed: a crash between that
// open and the write left EXACTLY the file a reader must never see —
// present, EEXIST-able, and truncated or empty.
//
// This closes that window by never letting path exist at all until its
// content is complete AND durable:
//
//  1. write the full content to a same-directory temp file, 0600;
//  2. fsync the temp file's data, then close it — by the time step 3 runs,
//     the bytes are on stable storage, not merely buffered;
//  3. os.Link(tmp, path) — link(2), unlike rename(2), REFUSES rather than
//     replaces when path already exists (EEXIST), so this keeps the exact
//     write-once guarantee O_EXCL gave, just reachable only once the
//     content it publishes is already durable rather than merely claimed;
//  4. remove the temp name (link created a SECOND directory entry for the
//     same inode; the original temp name is now redundant either way) and
//     best-effort fsync the parent directory, so the new directory entry
//     survives a crash too — losing that second fsync can at worst make the
//     entry itself not yet visible after a crash (as if the create had not
//     happened at all), never partial or corrupt, since step 2 already made
//     the CONTENT durable before step 3 ever published a name for it.
func writeRecordOnce(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the link below has succeeded and removed it

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("lease: write record temp file %s: %w", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("lease: fsync record temp file %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("lease: close record temp file %s: %w", tmpPath, err)
	}

	if err := os.Link(tmpPath, path); err != nil {
		return err
	}
	_ = os.Remove(tmpPath)

	if df, derr := os.Open(dir); derr == nil {
		_ = df.Sync()
		_ = df.Close()
	}
	return nil
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
