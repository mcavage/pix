//go:build unix

package lease

import (
	"os"
	"path/filepath"
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
