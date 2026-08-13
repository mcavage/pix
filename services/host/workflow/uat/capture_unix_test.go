//go:build unix

package uat

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadCaptureFileNoFollow_Unix(t *testing.T) {
	tmpDir := t.TempDir()

	// Test regular file
	regPath := filepath.Join(tmpDir, "regular.txt")
	err := os.WriteFile(regPath, []byte("hello"), 0644)
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	content, err := readCaptureFileNoFollow(regPath)
	if err != nil {
		t.Fatalf("expected nil err, got: %v", err)
	}
	if content != "hello" {
		t.Errorf("expected 'hello', got %q", content)
	}

	// Test symlink
	symPath := filepath.Join(tmpDir, "symlink.txt")
	err = os.Symlink(regPath, symPath)
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	_, err = readCaptureFileNoFollow(symPath)
	if err == nil {
		t.Error("expected error when reading symlink, got nil")
	}

	// Test not found
	_, err = readCaptureFileNoFollow(filepath.Join(tmpDir, "missing.txt"))
	if err == nil {
		t.Error("expected error for missing file, got nil")
	}
}
