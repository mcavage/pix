// processtime_darwin.go — Darwin's process start-time identity source: the
// kernel process table entry, read via sysctl(KERN_PROC, KERN_PROC_PID, pid)
// — no /proc filesystem exists on macOS to read a text stat file from, so
// this asks the kernel directly through the same sysctl `ps`/`top` use.

//go:build darwin

package supervise

import "golang.org/x/sys/unix"

// processStartTime asks the Darwin kernel for pid's kinfo_proc via
// SysctlKinfoProc("kern.proc.pid", pid) and reads P_starttime (struct
// extern_proc, a struct timeval) — the darwin analogue of /proc/pid/stat
// field 22 on Linux: a value a pid-reusing successor process can never
// share with the process it replaced. ok is false when the sysctl fails
// (pid does not exist, or this process lacks permission to see it) or
// reports a start time encodeStartTime will not vouch for; the caller must
// then refuse to bind identity by start time rather than treat that as
// proof of anything.
func processStartTime(pid int) (uint64, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, false
	}
	return encodeStartTime(int64(kp.Proc.P_starttime.Sec), int32(kp.Proc.P_starttime.Usec))
}
