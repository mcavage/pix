//go:build windows

package gworkspace

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"pix/host/sys"
)

// snapshotGogCredentials safely opens the user-provided OAuth client JSON,
// preventing TOCTOU symlink/FIFO attacks where possible, and returns a private,
// immutable snapshot path in a 0700 temp directory. The caller must invoke
// cleanup() when done. Max size is 1MB.
func snapshotGogCredentials(path string) (string, func(), error) {
	// Windows lacks O_NOFOLLOW in syscall, so we Lstat first to reject
	// symlinks, then Open and ensure they are the same file.
	lfi, err := os.Lstat(path)
	if err != nil {
		return "", nil, fmt.Errorf("lstat credentials: %w", err)
	}
	if !lfi.Mode().IsRegular() {
		return "", nil, fmt.Errorf("credentials must be a regular file (symlinks rejected), got %v", lfi.Mode())
	}
	if lfi.Size() > 1024*1024 {
		return "", nil, fmt.Errorf("credentials file too large (max 1MB)")
	}

	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return "", nil, fmt.Errorf("opening credentials: %w", err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return "", nil, fmt.Errorf("stating credentials: %w", err)
	}
	if !os.SameFile(lfi, fi) {
		return "", nil, fmt.Errorf("credentials file changed between lstat and open (symlink swap rejected)")
	}
	if !fi.Mode().IsRegular() {
		return "", nil, fmt.Errorf("credentials must be a regular file, got %v", fi.Mode())
	}

	tmpDir, err := os.MkdirTemp("", "pix-gog-*")
	if err != nil {
		return "", nil, fmt.Errorf("creating temp dir: %w", err)
	}

	cleanup := func() {
		_ = os.RemoveAll(tmpDir)
	}

	data, err := io.ReadAll(io.LimitReader(f, 1024*1024+1))
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("reading credentials: %w", err)
	}
	if len(data) > 1024*1024 {
		cleanup()
		return "", nil, fmt.Errorf("credentials file too large (max 1MB)")
	}

	if err := sys.AtomicWriteInDir(tmpDir, "credentials.json", data, 0o600); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("writing snapshot: %w", err)
	}

	return filepath.Join(tmpDir, "credentials.json"), cleanup, nil
}
