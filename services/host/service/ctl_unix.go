//go:build unix

// ctl_unix.go: the real signal + process-discovery shims for `serve stop` /
// `serve status` (syscall.Kill and pgrep are unix-only).

package service

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// killProcess sends sig to pid (sig 0 = liveness probe).
func killProcess(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }

// discoverServeProcs finds candidate `pix-host serve` pids when the pidfile is
// gone (e.g. `pix reset` moved the config dir out from under a running daemon).
// It is deliberately LOOSE — every candidate is verified before it is signalled,
// so widening the search cannot widen what gets killed.
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
