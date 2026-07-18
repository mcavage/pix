//go:build !unix

// serve_ctl_windows.go keeps `serve stop`/`serve status` compiling on non-unix
// platforms (M1). There is no unix signalling there, so every probe/signal
// reports failure — stopServe then treats the pid as not-running and refuses,
// which is the honest degrade (managed/lazy serve is unix-only anyway).

package main

import (
	"fmt"
	"syscall"
)

// killProcess: no unix signals on this platform.
func killProcess(pid int, sig syscall.Signal) error {
	return fmt.Errorf("signalling pid %d (sig %d) is not supported on this platform", pid, sig)
}
