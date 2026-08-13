package uat

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

var redactionRegex = regexp.MustCompile(`(?i)(bearer|cookie|token|api_key|apikey)[:=]\s*["']?[a-zA-Z0-9_\-\.]+["']?`)

func ReadArtifact(root, path string, maxSize int64, tailLines int) ([]byte, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}

	fullPath := filepath.Join(absRoot, path)

	// Component-by-component symlink check must be done BEFORE opening
	// Actually, the ValidateScenarioPath handles this. But let's be safe.

	// Open with O_NOFOLLOW
	f, err := os.OpenFile(fullPath, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open file (possible symlink): %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}

	if info.Size() > maxSize {
		return nil, fmt.Errorf("file too large")
	}

	content := make([]byte, info.Size())
	_, err = f.Read(content)
	if err != nil {
		return nil, err
	}

	// Tail lines
	if tailLines > 0 {
		scanner := bufio.NewScanner(bytes.NewReader(content))
		var lines []string
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
		}
		if tailLines < len(lines) {
			content = []byte(strings.Join(lines[len(lines)-tailLines:], "\n"))
		}
	}

	// Redact
	redacted := redactionRegex.ReplaceAll(content, []byte("$1: [REDACTED]"))

	return redacted, nil
}
