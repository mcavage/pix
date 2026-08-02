//go:build !unix

// serve_ctl_windows.go keeps `serve stop`/`serve status` compiling on non-unix
// platforms (M1). There is no unix signalling there, so every probe/signal
// reports failure — Stop then treats the pid as not-running and refuses,
// which is the honest degrade (managed/lazy serve is unix-only anyway).

package service

import (
	"fmt"
	"syscall"
)

// killProcess: no unix signals on this platform.
func killProcess(pid int, sig syscall.Signal) error {
	return fmt.Errorf("signalling pid %d (sig %d) is not supported on this platform", pid, sig)
}

// discoverServeProcs: no orphan discovery on this platform (managed/lazy serve
// is unix-only). Returns no candidates so Stop degrades to "not running".
func discoverServeProcs() ([]int, error) { return nil, nil }
