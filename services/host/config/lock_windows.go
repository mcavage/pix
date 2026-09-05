//go:build windows

package config

// withFileLock on Windows just runs fn: see sys/lock_windows.go for the
// same posture in the sibling package this file deliberately does not
// import (see lock_unix.go's doc comment for why).
func withFileLock(lockPath string, fn func() error) error {
	return fn()
}
