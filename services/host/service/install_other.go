//go:build !darwin

// serve_install_other.go is the ONE non-darwin compile stub: pix's managed
// login service is launchd-only (macOS host), but `services/host` must still
// `go build`/`go test` under GOOS=linux — that's the toolchain the Linux

package service

import "io"

// ErrUnsupportedHost is returned by every managed-service entry point on a
// non-darwin GOOS. Managed `pix serve install` is macOS (launchd) only.
var ErrUnsupportedHost = errUnsupportedHost{}

type errUnsupportedHost struct{}

func (errUnsupportedHost) Error() string {
	return "managed service install is only supported on macOS (launchd); use lazy auto-start (default) or run `pix serve` yourself"
}

func platformServeInstall(io.Writer) error   { return ErrUnsupportedHost }
func platformServeUninstall(io.Writer) error { return ErrUnsupportedHost }

func ManagedActive() bool { return false }

func restartManagedService() error { return ErrUnsupportedHost }

func StopManaged(io.Writer) error { return ErrUnsupportedHost }
