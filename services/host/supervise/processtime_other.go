// processtime_other.go — every platform besides Linux and Darwin: no kernel
// start-time source is wired, so identity by start time is always unknown.
// This is the conservative default, not a gap to silently work around —
// revalidateOrphan treats "unknown" as "refuse to kill", never as "assume
// safe".

//go:build !linux && !darwin

package supervise

// processStartTime always reports unknown on this platform.
func processStartTime(pid int) (uint64, bool) {
	return 0, false
}
