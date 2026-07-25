//go:build unix

// serve_ctl_unix.go: the real signal shim for `serve stop`/`serve status`
// (syscall.Kill only exists on unix; the windows sibling degrades with a clear
// error so GOOS=windows compiles — M1).

package main

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// killProcess sends sig to pid (sig 0 = liveness probe).
func killProcess(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }

// discoverServeProcs finds candidate `pix-host serve` pids when the pidfile
// is gone (e.g. `pix reset` moved the config dir out from under a running
// daemon). It is deliberately LOOSE: `pgrep -f pix-host` returns anything
// whose command line mentions our binary; the caller re-verifies each pid with
// verifyServeProc before signalling, so a false positive here is harmless (it is
// filtered out) and a blind kill is never possible. pgrep exit 1 = no match =>
// (nil, nil). Its own pid is excluded so `serve stop` never lists itself.
func discoverServeProcs() ([]int, error) {
	out, err := exec.Command("pgrep", "-f", "pix-host").Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return nil, nil // pgrep: no matching process
		}
		return nil, err
	}
	self := syscall.Getpid()
	var pids []int
	for _, f := range strings.Fields(string(out)) {
		if p, e := strconv.Atoi(f); e == nil && p > 0 && p != self {
			pids = append(pids, p)
		}
	}
	return pids, nil
}
