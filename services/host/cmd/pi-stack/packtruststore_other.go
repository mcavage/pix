//go:build !unix

// packtruststore_other.go keeps pi-stack-host compiling on non-unix platforms
// (R2-02): O_NOFOLLOW does not exist there, so this degrades to the previous
// Lstat-then-ReadFile check — a residual, ACCEPTED TOCTOU window on this
// platform only, the same posture lock_windows.go documents for advisory
// locking (no reparse-point-aware open in this module's dependency set). It
// still refuses an established symlink; unix (packtruststore_unix.go) is the
// platform that closes the race atomically.

package main

import (
	"fmt"
	"os"
)

// readPackTrustStoreFile: best-effort symlink refusal via Lstat, then
// ReadFile. See the package comment above for why this platform cannot close
// the TOCTOU window the way unix does.
func readPackTrustStoreFile(path string) ([]byte, error) {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symlink; refusing to read through it", path)
	}
	return os.ReadFile(path)
}
