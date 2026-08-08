// processtime_linux.go — Linux's process start-time identity source:
// /proc/pid/stat field 22 (clock ticks since boot), the kernel's own answer
// to "when was THIS process born".

//go:build linux

package supervise

import (
	"os"
	"strconv"
	"strings"
)

// processStartTime reads pid's START TIME from /proc/pid/stat field 22
// (clock ticks since boot). ok is false wherever /proc/pid/stat cannot be
// read or parsed (pid gone, or a hostile/unexpected format); the caller must
// then refuse to bind identity by start time rather than treat that as proof
// of anything.
func processStartTime(pid int) (uint64, bool) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	// comm (field 2) is parenthesized and may itself contain spaces or ')';
	// splitting on the LAST ')' is the standard safe way to parse the rest of
	// /proc/pid/stat regardless of what the process name contains.
	s := string(raw)
	idx := strings.LastIndexByte(s, ')')
	if idx < 0 || idx+2 > len(s) {
		return 0, false
	}
	fields := strings.Fields(s[idx+1:])
	// fields[0] is field 3 (state); starttime is field 22 overall, index 22-3
	// in this 0-based slice.
	const starttimeIdx = 22 - 3
	if len(fields) <= starttimeIdx {
		return 0, false
	}
	v, err := strconv.ParseUint(fields[starttimeIdx], 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
