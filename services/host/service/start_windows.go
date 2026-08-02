//go:build !unix

// serve_start_windows.go keeps the launcher compiling on non-unix platforms
// (M1): detached spawn (Setsid) and syscall.Flock do not exist there, so lazy
// auto-start degrades to a clear "run `pix serve` yourself" error and the
// lock helpers run their critical sections unserialized (the daemon's own
// store locking — where available — remains the backstop).

package service

import (
	"fmt"
	"pix/host/sys"
)

// spawnDetachedServe: no detached-session spawn on this platform.
func spawnDetachedServe(string, []string, string) (serveChildHandle, error) {
	return serveChildHandle{}, fmt.Errorf("auto-start is not supported on this platform; run `pix serve` yourself")
}

// ReadFileNoSymlink: O_NOFOLLOW does not exist on this platform; refuse to
// read at all rather than risk following a symlink. tailFileLines degrades to
// "" (the same safe-empty result the unix TOCTOU fix produces on a symlink).
func ReadFileNoSymlink(string) ([]byte, error) {
	return nil, fmt.Errorf("log tail is not supported on this platform")
}

// tryServeSpawnLock: no flock on this platform; run fn unserialized. The spawn
// itself fails above with a clear message, so nothing can double-fork.
func tryServeSpawnLock(fn func() error) (bool, error) {
	return true, fn()
}

// withFlock delegates to sys, which owns the one implementation (and its
// //go:build split). Kept as a name here because the serve lock path reads
// better without a package qualifier, and because deleting the name would churn
// call sites for no gain.
func withFlock(lockPath string, fn func() error) error { return sys.Lock(lockPath, fn) }
