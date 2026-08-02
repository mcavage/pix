//go:build !darwin && !linux

// serve_install_other.go keeps the launcher compiling on platforms with no
// managed-service story: install/uninstall explain themselves, mode detection
// never reports "managed".

package service

import (
	"fmt"
	"io"
)

func platformServeInstall(io.Writer) error {
	return fmt.Errorf("managed service install is only supported on macOS (launchd) and Linux (systemd --user); use lazy auto-start (default) or run `pix serve` yourself")
}

func platformServeUninstall(io.Writer) error {
	return fmt.Errorf("managed service install is only supported on macOS and Linux — nothing to uninstall")
}

func ManagedActive() bool { return false }

func restartManagedService() error {
	return fmt.Errorf("no managed service on this platform")
}

func StopManaged(io.Writer) error {
	return fmt.Errorf("no managed service on this platform")
}
