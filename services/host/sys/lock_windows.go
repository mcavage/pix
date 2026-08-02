//go:build !unix

package sys

// withFlock degrades to running fn unlocked on platforms without syscall.Flock.
// The launcher's supported platforms are unix; this keeps GOOS=windows
// compiling rather than pretending to serialize.
func withFlock(_ string, fn func() error) error { return fn() }
