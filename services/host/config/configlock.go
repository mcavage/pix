//go:build unix

// Config lock: an exclusive advisory flock that serializes atomic config
// rewrites so a `pi-stack migrate` config transaction and a concurrent
// `pi-stack config set`/`unset` never lose an update. Unlike the memory lock
// (services/host/lock.go), which is NON-blocking and refuses on contention, this
// one BLOCKS: a config writer should wait its turn and then re-read fresh under
// the lock, not fail. Unix-only, matching where pi-stack-host runs.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ConfigLockPath is <config-dir>/.config.lock — the advisory lock file
// WithConfigLock takes. A sibling of config.toml so every writer resolves the
// same path.
func ConfigLockPath() string {
	dir, err := ConfigDir()
	if err != nil {
		return ".config.lock"
	}
	return filepath.Join(dir, ".config.lock")
}

// WithConfigLock runs fn while holding an EXCLUSIVE, BLOCKING advisory flock on
// ConfigLockPath(), releasing it (and closing the fd) afterward. It creates the
// config dir (0700) and lock file (0600) as needed. Blocking is deliberate: a
// config rewrite should serialize behind any in-flight one and then proceed,
// re-reading fresh under the lock, rather than refusing. fn's error is returned
// unchanged; a lock/open failure is returned wrapped.
func WithConfigLock(fn func() error) error {
	path := ConfigLockPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open config lock %s: %w", path, err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock %s: %w", path, err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn()
}
