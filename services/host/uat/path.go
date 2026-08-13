package uat

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

func ValidateScenarioPath(root, path string) (string, error) {
	if filepath.IsAbs(path) {
		return "", errors.New("absolute path not allowed")
	}

	// 1. Join with root to get absolute path
	// 2. EvalSymlinks to resolve any symlinks and get the true path
	fullPath := filepath.Join(root, path)

	// We need the root to be absolute to compare
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}

	// EvalSymlinks requires the path to exist, which might not be the case for all UAT scenarios
	// But for artifact/scenario reading, it should exist.
	// If it doesn't exist, we might have to be more careful.
	// Let's assume it exists for now based on the prompt's focus on "artifact reader".
	realPath, err := filepath.EvalSymlinks(fullPath)
	if err != nil {
		// If the file doesn't exist, we can't eval symlinks.
		// As a fallback, we can check if the directory part exists.
		// For now, let's assume it should exist.
		return "", fmt.Errorf("could not evaluate path: %w", err)
	}

	// Ensure realPath is inside absRoot
	if !strings.HasPrefix(realPath, absRoot) {
		return "", errors.New("path escapes root")
	}

	return realPath, nil
}
