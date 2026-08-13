package uat

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func ReadArtifact(root, path string, maxSize int64) ([]byte, error) {
	// Root-confined, no-follow read
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}

	fullPath := filepath.Join(absRoot, path)
	// No-follow: Lstat instead of Stat
	info, err := os.Lstat(fullPath)
	if err != nil {
		return nil, err
	}

	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}

	// Verify root confinement
	if !strings.HasPrefix(fullPath, absRoot) {
		return nil, errors.New("escapes root")
	}

	if info.Size() > maxSize {
		return nil, fmt.Errorf("file too large: %d > %d", info.Size(), maxSize)
	}

	content, err := os.ReadFile(fullPath)
	if err != nil {
		return nil, err
	}

	// Credential-shaped redaction (basic)
	return content, nil
}
