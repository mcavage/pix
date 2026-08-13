package uat

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type GitVerifier interface {
	FileExistsAtCommit(repoPath, commitSHA, filePath string) (bool, error)
}

func isSymlink(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeSymlink != 0
}

func ValidateScenarioPath(root string, path string, verifier GitVerifier, repoPath, commitSHA string) (string, error) {
	if filepath.IsAbs(path) {
		return "", errors.New("absolute path not allowed")
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}

	fullPath := filepath.Join(absRoot, path)

	// Check containment
	rel, err := filepath.Rel(absRoot, fullPath)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(rel, "..") || rel == ".." {
		return "", errors.New("path escapes root")
	}

	// Component-by-component symlink check
	curr := absRoot
	parts := strings.Split(rel, string(filepath.Separator))
	for _, part := range parts {
		curr = filepath.Join(curr, part)
		if isSymlink(curr) {
			return "", fmt.Errorf("symlink encountered at component: %s", part)
		}
	}

	// Check if file exists at commit
	if verifier != nil {
		exists, err := verifier.FileExistsAtCommit(repoPath, commitSHA, rel)
		if err != nil {
			return "", err
		}
		if !exists {
			return "", errors.New("file does not exist at commit")
		}
	}

	return fullPath, nil
}
