//go:build unix

// packtruststore_unix.go carries the unix-only symlink-safe reader for the
// pack trust store (R2-02): O_NOFOLLOW makes the open itself atomically
// refuse a symlink, closing the Lstat-then-ReadFile TOCTOU window the old
// loadPackTrustStore had (an attacker racing a symlink in between the check
// and the read could make it follow an arbitrary file). Mirrors
// readFileNoSymlink (serve_start_unix.go) but lives here so packtruststore.go
// stays the single owner of the trust-store file format/contract; the two
// are not merged into one helper because their callers want different
// not-found/degrade contracts (log-tail degrades to "", the trust store must
// propagate a real error so the caller can fail closed).

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// readPackTrustStoreFile reads path via a single O_NOFOLLOW open: there is no
// separate Lstat check to race, so a symlink planted at any point up to (and
// including) the open call itself is refused, never dereferenced. A missing
// file still surfaces as an os.IsNotExist-compatible error (the *PathError
// ENOENT wrapping os.OpenFile already does), preserving loadPackTrustStore's
// absent -> fresh-empty-store contract.
func readPackTrustStoreFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("%s is a symlink; refusing to read through it", path)
		}
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}
