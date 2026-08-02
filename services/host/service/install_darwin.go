//go:build darwin

// serve_install_darwin.go is the thin macOS dispatch for the managed login
// service: all logic + argv choices live (unit-tested) in serve_install.go;
// this file only binds the real runner, uid, and $HOME.

package service

import (
	"io"
	"os"
)

// platformServeInstall installs the launchd LaunchAgent.
func platformServeInstall(out io.Writer) error {
	bin, err := resolvedHostBinary()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	return launchdInstall(realCmdRunner, realInstallFS(), os.Getuid(), home, bin, capturedServeEnv(os.Getenv), out)
}

// platformServeUninstall removes the launchd LaunchAgent.
func platformServeUninstall(out io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	return launchdUninstall(realCmdRunner, realInstallFS(), os.Getuid(), home, out)
}

// ManagedActive reports whether the launchd unit is loaded (used by
// config propagation's lifecycle-mode detection).
func ManagedActive() bool { return launchdActive(realCmdRunner, os.Getuid()) }

// restartManagedService kickstarts the launchd unit in place.
func restartManagedService() error { return launchdRestart(realCmdRunner, os.Getuid()) }

// StopManaged boots the launchd unit out so KeepAlive stops respawning it
// (without removing the plist).
func StopManaged(out io.Writer) error {
	return launchdStop(realCmdRunner, os.Getuid(), out)
}
