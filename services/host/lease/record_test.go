//go:build unix

package lease

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestCreateRecord_WritesImmutableFields0600(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sbx-1")
	rec, err := CreateRecord(dir, "sbx-1")
	if err != nil {
		t.Fatalf("CreateRecord: %v", err)
	}
	if rec.InstanceID != "sbx-1" {
		t.Errorf("InstanceID = %q, want sbx-1", rec.InstanceID)
	}
	if rec.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
	if rec.CreatedPID != os.Getpid() {
		t.Errorf("CreatedPID = %d, want %d", rec.CreatedPID, os.Getpid())
	}
	fi, err := os.Stat(filepath.Join(dir, recordFileName))
	if err != nil {
		t.Fatalf("Stat record: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("record file perm = %o, want 0600", perm)
	}
	dfi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat dir: %v", err)
	}
	if perm := dfi.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir perm = %o, want 0700", perm)
	}
}

func TestCreateRecord_SecondCallSameIDIsNoopAndImmutable(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sbx-1")
	first, err := CreateRecord(dir, "sbx-1")
	if err != nil {
		t.Fatalf("CreateRecord #1: %v", err)
	}
	second, err := CreateRecord(dir, "sbx-1")
	if err != nil {
		t.Fatalf("CreateRecord #2: %v", err)
	}
	if !first.CreatedAt.Equal(second.CreatedAt) {
		t.Errorf("CreatedAt changed across create calls: %v -> %v", first.CreatedAt, second.CreatedAt)
	}
	if first.CreatedPID != second.CreatedPID {
		t.Errorf("CreatedPID changed across create calls: %v -> %v", first.CreatedPID, second.CreatedPID)
	}
}

func TestCreateRecord_RefusesInstanceIDMismatch(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sbx-1")
	if _, err := CreateRecord(dir, "sbx-1"); err != nil {
		t.Fatalf("CreateRecord: %v", err)
	}
	if _, err := CreateRecord(dir, "sbx-2"); err == nil {
		t.Error("CreateRecord with a different instance id on an existing dir = nil error, want refusal")
	}
}

func TestCreateRecord_RejectsUnsafeInstanceID(t *testing.T) {
	root := t.TempDir()
	if _, err := CreateRecord(filepath.Join(root, "x"), "../escape"); err == nil {
		t.Error("CreateRecord with a traversal id = nil error, want refusal")
	}
}

func TestReadRecord_RoundTrips(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sbx-1")
	created, err := CreateRecord(dir, "sbx-1")
	if err != nil {
		t.Fatalf("CreateRecord: %v", err)
	}
	read, err := ReadRecord(dir)
	if err != nil {
		t.Fatalf("ReadRecord: %v", err)
	}
	if read.InstanceID != created.InstanceID || !read.CreatedAt.Equal(created.CreatedAt) || read.CreatedPID != created.CreatedPID {
		t.Errorf("ReadRecord = %+v, want %+v", read, created)
	}
}

func TestReadRecord_MissingIsError(t *testing.T) {
	root := t.TempDir()
	if _, err := ReadRecord(filepath.Join(root, "never-created")); err == nil {
		t.Error("ReadRecord on a nonexistent dir = nil error, want error")
	}
}

// TestCreateRecord_NoLeftoverTempFileAfterWrite pins writeRecordOnce's
// temp-then-link install: the same-directory os.CreateTemp file must never
// survive a successful create.
func TestCreateRecord_NoLeftoverTempFileAfterWrite(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sbx-1")
	if _, err := CreateRecord(dir, "sbx-1"); err != nil {
		t.Fatalf("CreateRecord: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("leftover temp file after a successful CreateRecord: %s", e.Name())
		}
	}
}

// TestCreateRecord_ConcurrentDifferentIdentityLoserIsRefusedNotSilentlyAccepted
// is the concurrency half of "existing record must never be overwritten by
// different identity": N goroutines race CreateRecord on the SAME never-
// before-seen directory, each asking for a DIFFERENT instance id. Exactly
// one wins (its id ends up in record.json); under the OLD implementation,
// every LOSER's O_EXCL-open failure with EEXIST was handled by an
// unconditional `return readRecordFile(path)` — handing back the WINNER's
// record as if it were the loser's own, with no identity check at all. This
// test proves every loser is refused instead.
func TestCreateRecord_ConcurrentDifferentIdentityLoserIsRefusedNotSilentlyAccepted(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sbx-1")
	const n = 20

	type result struct {
		rec *Record
		err error
	}
	results := make([]result, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec, err := CreateRecord(dir, fmt.Sprintf("inst-%d", i))
			results[i] = result{rec, err}
		}(i)
	}
	wg.Wait()

	final, err := ReadRecord(dir)
	if err != nil {
		t.Fatalf("ReadRecord: %v", err)
	}

	successes := 0
	for i, r := range results {
		wantID := fmt.Sprintf("inst-%d", i)
		if wantID == final.InstanceID {
			if r.err != nil {
				t.Errorf("winner call (id %s) returned an error: %v", wantID, r.err)
			}
			if r.rec == nil || r.rec.InstanceID != wantID {
				t.Errorf("winner call (id %s) returned mismatched record %+v", wantID, r.rec)
			}
			successes++
			continue
		}
		// Every loser must be refused — never silently handed back a
		// DIFFERENT instance id's record as if it were its own.
		if r.err == nil {
			t.Errorf("loser call (id %s) got no error, want a relabel refusal (final record is %s); got record %+v", wantID, final.InstanceID, r.rec)
		}
	}
	if successes != 1 {
		t.Errorf("exactly one caller should have matched the final record's identity, got %d", successes)
	}

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("leftover temp file after concurrent CreateRecord: %s", e.Name())
		}
	}
}

// TestCreateRecord_ConcurrentSameIdentityAllSucceedIdempotently is the
// companion positive case: every racer asking for the SAME identity must
// succeed with an identical record — concurrency must not turn an
// idempotent, legitimate retry into a spurious refusal.
func TestCreateRecord_ConcurrentSameIdentityAllSucceedIdempotently(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sbx-1")
	const n = 20
	results := make([]*Record, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec, err := CreateRecordFor(dir, "sbx-1", "pix-x")
			results[i], errs[i] = rec, err
		}(i)
	}
	wg.Wait()

	for i := range results {
		if errs[i] != nil {
			t.Errorf("call %d: %v", i, errs[i])
			continue
		}
		if results[i].InstanceID != "sbx-1" || results[i].Name != "pix-x" {
			t.Errorf("call %d: record = %+v", i, results[i])
		}
	}
}
