package lease

// capFDScanLimit bounds a numeric fd-scan range: it returns cur (the
// RLIMIT_NOFILE-style soft limit reported by the kernel) narrowed to
// [0, max]. This is pure arithmetic, deliberately factored out of
// lock_process_fds_darwin_test.go's darwinFDScanLimit so the capping
// behavior itself — the part guarding against an "unlimited" or absurdly
// large soft limit turning a self-check into a huge syscall loop — has
// unit coverage that runs on every platform this package is tested on, not
// only on a real Darwin host where the rest of that file's fcntl-based
// scanning can actually execute.
func capFDScanLimit(cur uint64, max int) int {
	if max < 0 {
		max = 0
	}
	if cur > uint64(max) {
		return max
	}
	return int(cur)
}
