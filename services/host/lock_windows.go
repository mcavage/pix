//go:build !unix

// lock_windows.go keeps pi-stack-host compiling on non-unix platforms (the
// launcher's cross-compile gate builds the whole module). There is no
// syscall.Flock there, so advisory locking degrades to a LOUD no-op: sqlite's
// own file locking still protects db integrity, but the daemon-vs-restore
// mutual exclusion the flock provides on unix is unavailable.

package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"pi-stack/host/config"
)

// acquireLock (non-unix): no advisory flock available. Create the lock file
// for path parity, warn once, and return a no-op release.
func acquireLock(path string) (release func(), err error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create lock dir %s: %w", dir, err)
		}
	}
	log.Printf("warning: advisory file locking is not supported on this platform; skipping store lock %s", path)
	return func() {}, nil
}

// lockMemoryStoreOrFatal mirrors the unix contract (see lock.go); on this
// platform it degrades to the warn-and-proceed acquireLock above.
var acquireMemLockFn = acquireLock

func lockMemoryStoreOrFatal(fatal func(format string, a ...any)) func() {
	if fatal == nil {
		fatal = log.Fatalf
	}
	release, err := acquireMemLockFn(config.MemoryLockPath())
	if err != nil {
		fatal("memory: could not prepare store lock: %v", err)
		return func() {}
	}
	return release
}
