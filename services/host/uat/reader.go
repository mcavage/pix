package uat

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var redactionRegex = regexp.MustCompile(`(?i)(bearer|cookie|token|api_key|apikey)[:=]\s*["']?[a-zA-Z0-9_\-\.]+["']?`)

func ReadArtifact(root, path string, maxSize int64, tailLines int, cursor int64) ([]byte, int64, error) {
	if filepath.IsAbs(path) {
		return nil, 0, errors.New("absolute path not allowed")
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, 0, err
	}

	fullPath := filepath.Join(absRoot, path)

	rel, err := filepath.Rel(absRoot, fullPath)
	if err != nil {
		return nil, 0, err
	}
	if strings.HasPrefix(rel, "..") || rel == ".." {
		return nil, 0, errors.New("path escapes root")
	}

	curr := absRoot
	parts := strings.Split(rel, string(filepath.Separator))
	for _, part := range parts {
		if part == "" {
			continue
		}
		curr = filepath.Join(curr, part)
		info, err := os.Lstat(curr)
		if err != nil {
			return nil, 0, fmt.Errorf("failed to stat component %s: %w", part, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, 0, fmt.Errorf("symlink encountered at component: %s", part)
		}
	}

	f, err := os.OpenFile(fullPath, os.O_RDONLY, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to open file: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, errors.New("not a regular file")
	}

	if info.Size() > maxSize {
		return nil, 0, fmt.Errorf("file too large")
	}

	if cursor > info.Size() {
		cursor = info.Size()
	}

	if cursor > 0 {
		_, err = f.Seek(cursor, io.SeekStart)
		if err != nil {
			return nil, 0, err
		}
	}

	bytesToRead := info.Size() - cursor
	content := make([]byte, bytesToRead)
	var totalRead int64
	for totalRead < bytesToRead {
		n, err := f.Read(content[totalRead:])
		totalRead += int64(n)
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, 0, err
		}
	}
	content = content[:totalRead]

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

	redacted := redactionRegex.ReplaceAll(content, []byte("$1: [REDACTED]"))

	return redacted, info.Size(), nil
}
