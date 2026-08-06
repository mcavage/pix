// processtime.go — the OS-independent piece of process start-time identity:
// packing a kernel-reported (seconds, microseconds) pair into one stable
// uint64 a pid-reusing successor process can never coincidentally share.
// This file carries NO build tag on purpose, so its parsing/identity logic
// is unit-testable on every platform (including the Linux box that runs CI),
// even though only processtime_darwin.go ever calls it for real.

package supervise

// encodeStartTime packs a (seconds, microseconds)-since-epoch pair — the
// shape darwin's kinfo_proc.p_starttime (a struct timeval) reports a
// process's birth in — into a single monotonically-ordered uint64 identity
// value. It refuses (ok=false) a negative or out-of-range component rather
// than silently wrap or truncate: a value this function will not vouch for
// must never be compared as if it were a trustworthy identity.
func encodeStartTime(sec int64, usec int32) (v uint64, ok bool) {
	if sec < 0 || usec < 0 || usec >= 1_000_000 {
		return 0, false
	}
	return uint64(sec)*1_000_000 + uint64(usec), true
}
