//go:build !unix

// serve_start_windows.go keeps the launcher compiling on non-unix platforms
// (M1): detached spawn (Setsid) and syscall.Flock do not exist there, so lazy
// auto-start degrades to a clear "run `pi-stack serve` yourself" error and the
// lock helpers run their critical sections unserialized (the daemon's own
// store locking — where available — remains the backstop).

package main

import "fmt"

// spawnDetachedServe: no detached-session spawn on this platform.
func spawnDetachedServe(string, []string, string) (serveChildHandle, error) {
	return serveChildHandle{}, fmt.Errorf("auto-start is not supported on this platform; run `pi-stack serve` yourself")
}

// readFileNoSymlink: O_NOFOLLOW does not exist on this platform; refuse to
// read at all rather than risk following a symlink. tailFileLines degrades to
// "" (the same safe-empty result the unix TOCTOU fix produces on a symlink).
func readFileNoSymlink(string) ([]byte, error) {
	return nil, fmt.Errorf("log tail is not supported on this platform")
}

// tryServeSpawnLock: no flock on this platform; run fn unserialized. The spawn
// itself fails above with a clear message, so nothing can double-fork.
func tryServeSpawnLock(fn func() error) (bool, error) {
	return true, fn()
}

// withFlock: no flock on this platform; run fn unserialized (task-lifecycle
// callers lose cross-process mutual exclusion, not correctness of a single
// process).
func withFlock(_ string, fn func() error) error {
	return fn()
}
