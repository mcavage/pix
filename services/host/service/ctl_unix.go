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

// processExited closes the Unix zombie gap in kill(pid, 0): a terminated child
// continues to answer signal 0 until its parent reaps it, even though it has
// released its ports and cannot handle another signal. launchd can expose that
// state briefly after bootout. Treat only ps's explicit zombie state as exited;
// any missing or ambiguous answer remains live and preserves fail-closed signal
// escalation.
func processExited(pid int) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && processStatExited(string(out))
}

func processStatExited(stat string) bool {
	stat = strings.TrimSpace(stat)
	return stat != "" && stat[0] == 'Z'
}

// discoverServeProcs finds candidate `pix-host serve` pids when the pidfile is
// gone (e.g. the config dir was moved out from under a running daemon by hand).
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
