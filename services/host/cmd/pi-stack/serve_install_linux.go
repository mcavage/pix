//go:build linux

// serve_install_linux.go is the thin Linux dispatch for the managed login
// service (systemd --user): all logic + argv choices live (unit-tested) in
// serve_install.go; this file only binds the real runner and $HOME.

package main

import (
	"io"
	"os"
)

// platformServeInstall installs the systemd --user unit.
func platformServeInstall(out io.Writer) error {
	bin, err := resolvedHostBinary()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	return systemdInstall(realCmdRunner, realInstallFS(), home, bin, capturedServeEnv(os.Getenv), out)
}

// platformServeUninstall removes the systemd --user unit.
func platformServeUninstall(out io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	return systemdUninstall(realCmdRunner, realInstallFS(), home, out)
}

// managedServiceActive reports whether the systemd --user unit is active (used
// by config propagation's lifecycle-mode detection).
func managedServiceActive() bool { return systemdActive(realCmdRunner) }

// restartManagedService restarts the systemd --user unit.
func restartManagedService() error { return systemdRestart(realCmdRunner) }

// stopManagedService stops the systemd --user unit so Restart= stops respawning
// it (without disabling it).
func stopManagedService(out io.Writer) error { return systemdStop(realCmdRunner, out) }
