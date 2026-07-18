//go:build unix

// serve_ctl_unix.go: the real signal shim for `serve stop`/`serve status`
// (syscall.Kill only exists on unix; the windows sibling degrades with a clear
// error so GOOS=windows compiles — M1).

package main

import "syscall"

// killProcess sends sig to pid (sig 0 = liveness probe).
func killProcess(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }
