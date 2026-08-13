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
	os.WriteFile(artifact, []byte("content"), 0644)

	t.Run("valid", func(t *testing.T) {
		content, err := ReadArtifact(root, "artifact.txt", 1024)
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != "content" {
			t.Errorf("expected content, got %s", string(content))
		}
	})

	t.Run("max-size", func(t *testing.T) {
		_, err := ReadArtifact(root, "artifact.txt", 5)
		if err == nil {
			t.Error("expected error due to max size")
		}
	})
}
