//go:build !windows

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// snapshotGogCredentials safely opens the user-provided OAuth client JSON,
// preventing TOCTOU symlink/FIFO attacks, and returns a private, immutable
// snapshot path in a 0700 temp directory. The caller must invoke cleanup()
// when done. Max size is 1MB.
func snapshotGogCredentials(path string) (string, func(), error) {
	// Open with O_NOFOLLOW to reject symlinks instantly, and O_NONBLOCK so
	// FIFOs/devices don't hang the launcher.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", nil, fmt.Errorf("opening credentials (symlinks/FIFOs rejected): %w", err)
	}
	defer f.Close()

	// Fstat the already-opened FD to ensure it's a true regular file.
	fi, err := f.Stat()
	if err != nil {
		return "", nil, fmt.Errorf("stating credentials: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return "", nil, fmt.Errorf("credentials must be a regular file, got %v", fi.Mode())
	}
	if fi.Size() > 1024*1024 {
		return "", nil, fmt.Errorf("credentials file too large (max 1MB)")
	}

	// Create a private launcher-owned 0700 temp dir.
	tmpDir, err := os.MkdirTemp("", "pi-stack-gog-*")
	if err != nil {
		return "", nil, fmt.Errorf("creating temp dir: %w", err)
	}

	cleanup := func() {
		_ = os.RemoveAll(tmpDir)
	}

	// Read the contents securely bounded by LimitReader.
	// We clear O_NONBLOCK just in case, but regular files ignore it.
	data, err := io.ReadAll(io.LimitReader(f, 1024*1024+1))
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("reading credentials: %w", err)
	}
	if len(data) > 1024*1024 {
		cleanup()
		return "", nil, fmt.Errorf("credentials file too large (max 1MB)")
	}

	// Write the snapshot immutably inside the 0700 dir.
	// atomicWriteInDir does a temp-file + rename so it's symlink-safe.
	if err := atomicWriteInDir(tmpDir, "credentials.json", data, 0o600); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("writing snapshot: %w", err)
	}

	return filepath.Join(tmpDir, "credentials.json"), cleanup, nil
}
