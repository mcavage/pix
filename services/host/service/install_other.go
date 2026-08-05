//go:build !darwin

// install_other.go is the ONE non-darwin compile stub: the managed login service
// is launchd-only (macOS host), but `services/host` must still build and test
// under GOOS=linux (the sandbox toolchain). Every managed entry point answers
// with the same refusal, and ManagedActive is false — so mode detection on Linux
// always resolves to lazy/foreground/down, never managed.

package service

import (
	"errors"
	"io"
)

var errUnsupportedHost = errors.New(
	"managed service install is only supported on macOS (launchd); use lazy auto-start (default) or run `pix serve` yourself")

func platformServeInstall(io.Writer) error   { return errUnsupportedHost }
func platformServeUninstall(io.Writer) error { return errUnsupportedHost }
func restartManagedService() error           { return errUnsupportedHost }
func StopManaged(io.Writer) error            { return errUnsupportedHost }
func ManagedActive() bool                    { return false }
