//go:build windows

package sys

// withFlock takes a BLOCKING advisory exclusive lock on lockPath for the duration.
// For Windows, just run the function.
func withFlock(lockPath string, fn func() error) error {
	return fn()
}
