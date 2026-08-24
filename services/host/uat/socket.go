package uat

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// SocketFileName is the fixed relay socket name inside a UAT session's
// runner state directory (the same 0700 directory RegisterMCP creates at
// <state>/sessions/<sessionID> and removeSessionState later deletes).
const SocketFileName = "uat.sock"

// SessionSocketPath returns the transport socket path for a session's runner
// state directory. sessionStateDir must already be that per-session 0700
// directory, not the shared UAT state root.
func SessionSocketPath(sessionStateDir string) string {
	return filepath.Join(sessionStateDir, SocketFileName)
}

// ValidateSocketDir hardens the directory a UAT relay socket lives in before
// either side (the uat-worker listener or the uat-mcp gateway dialer) touches
// it. It reuses the same real-directory-not-symlink shape
// removeSessionState/RegisterMCP already enforce on the session state
// directory, plus an explicit permission and ownership check: a
// world/group-readable or other-uid directory here would let a local peer
// intercept or redirect the whole UAT transport.
func ValidateSocketDir(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("UAT socket directory must be absolute: %s", dir)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("stat UAT socket directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("UAT socket directory %s must not be a symlink", dir)
	}
	if !info.IsDir() {
		return fmt.Errorf("UAT socket directory %s is not a directory", dir)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("UAT socket directory %s must be 0700, got %o", dir, info.Mode().Perm())
	}
	if err := checkOwnedByCurrentUser(dir, info); err != nil {
		return err
	}
	return nil
}

// ValidateSocketPath hardens the socket path itself, ahead of a Listen or
// Dial: the containing directory must pass ValidateSocketDir, and any
// existing file at path must not be a symlink — a pre-planted symlink there
// could redirect a connect/listen to an attacker-chosen path outside the
// session directory entirely.
func ValidateSocketPath(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("UAT socket path must be absolute")
	}
	if err := ValidateSocketDir(filepath.Dir(path)); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat UAT socket path: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("UAT socket path %s must not be a symlink", path)
	}
	return nil
}
