package uat

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadArtifact(t *testing.T) {
	tmpDir, _ := os.MkdirTemp("", "uat-reader")
	defer os.RemoveAll(tmpDir)

	root := filepath.Join(tmpDir, "root")
	os.Mkdir(root, 0755)

	artifact := filepath.Join(root, "artifact.txt")
	content := "line1\nline2\nline3\nAPIKEY: secret123"
	os.WriteFile(artifact, []byte(content), 0644)

	t.Run("valid", func(t *testing.T) {
		res, err := ReadArtifact(root, "artifact.txt", 1024, 0)
		if err != nil {
			t.Fatal(err)
		}
		// Check redaction
		if string(res) != "line1\nline2\nline3\nAPIKEY: [REDACTED]" {
			t.Errorf("expected redacted, got %s", string(res))
		}
	})

	t.Run("tail", func(t *testing.T) {
		res, err := ReadArtifact(root, "artifact.txt", 1024, 2)
		if err != nil {
			t.Fatal(err)
		}
		expected := "line3\nAPIKEY: [REDACTED]"
		if string(res) != expected {
			t.Errorf("expected tail %s, got %s", expected, string(res))
		}
	})

	t.Run("symlink-fail", func(t *testing.T) {
		symlink := filepath.Join(root, "symlink.txt")
		os.Symlink(artifact, symlink)
		_, err := ReadArtifact(root, "symlink.txt", 1024, 0)
		if err == nil {
			t.Error("expected error for symlink")
		}
	})

	t.Run("oversize", func(t *testing.T) {
		_, err := ReadArtifact(root, "artifact.txt", 2, 0)
		if err == nil {
			t.Error("expected error for oversize file")
		}
	})
}
