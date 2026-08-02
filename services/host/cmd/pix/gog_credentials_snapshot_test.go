package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestSnapshotGogCredentials_RegularFile(t *testing.T) {
	dir := t.TempDir()
	credPath := filepath.Join(dir, "creds.json")
	content := []byte(`{"web":{"client_id":"foo"}}`)
	if err := os.WriteFile(credPath, content, 0o644); err != nil {
		t.Fatal(err)
	}

	snapPath, cleanup, err := snapshotGogCredentials(credPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cleanup()

	snapContent, err := os.ReadFile(snapPath)
	if err != nil {
		t.Fatalf("reading snapshot: %v", err)
	}
	if !bytes.Equal(snapContent, content) {
		t.Errorf("snapshot content %q != original %q", snapContent, content)
	}

	// Verify permissions
	fi, err := os.Stat(snapPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("snapshot permissions = %v, want 0600", fi.Mode().Perm())
	}
	dirFi, err := os.Stat(filepath.Dir(snapPath))
	if err != nil {
		t.Fatal(err)
	}
	if dirFi.Mode().Perm() != 0o700 {
		t.Errorf("snapshot dir permissions = %v, want 0700", dirFi.Mode().Perm())
	}

	// Verify cleanup
	cleanup()
	if _, err := os.Stat(filepath.Dir(snapPath)); !os.IsNotExist(err) {
		t.Errorf("expected temp dir to be removed, got err = %v", err)
	}
}

func TestSnapshotGogCredentials_SymlinkRejected(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	if err := os.WriteFile(target, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(dir, "symlink.json")
	if err := os.Symlink(target, symlink); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}

	_, cleanup, err := snapshotGogCredentials(symlink)
	if err == nil {
		cleanup()
		t.Fatalf("expected error for symlink, got nil")
	}
}

func TestSnapshotGogCredentials_OversizeRejected(t *testing.T) {
	dir := t.TempDir()
	credPath := filepath.Join(dir, "large.json")
	f, err := os.Create(credPath)
	if err != nil {
		t.Fatal(err)
	}
	// Write slightly more than 1MB
	if err := f.Truncate(1024*1024 + 10); err != nil {
		t.Fatal(err)
	}
	f.Close()

	_, cleanup, err := snapshotGogCredentials(credPath)
	if err == nil {
		cleanup()
		t.Fatalf("expected error for oversized file, got nil")
	}
}

func TestSnapshotGogCredentials_SourceSwapDoesNotAffectSnapshot(t *testing.T) {
	dir := t.TempDir()
	credPath := filepath.Join(dir, "creds.json")
	content1 := []byte("first content")
	if err := os.WriteFile(credPath, content1, 0o644); err != nil {
		t.Fatal(err)
	}

	snapPath, cleanup, err := snapshotGogCredentials(credPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer cleanup()

	// Modify the original file
	content2 := []byte("second content")
	if err := os.WriteFile(credPath, content2, 0o644); err != nil {
		t.Fatal(err)
	}

	// snapshot must remain unchanged
	snapContent, err := os.ReadFile(snapPath)
	if err != nil {
		t.Fatalf("reading snapshot: %v", err)
	}
	if !bytes.Equal(snapContent, content1) {
		t.Errorf("snapshot was affected by source modification! got %q, want %q", snapContent, content1)
	}
}
