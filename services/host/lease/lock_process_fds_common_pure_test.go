package lease

import "testing"

// TestCapFDScanLimit exercises the pure fd-scan bound (see
// lock_process_fds_common_test.go) on every platform this package builds
// for, since it is the one piece of the Darwin numeric-range scanner
// (lock_process_fds_darwin_test.go) that needs no fcntl/Getrlimit syscall
// and so needs no real Darwin host to prove correct.
func TestCapFDScanLimit(t *testing.T) {
	cases := []struct {
		name string
		cur  uint64
		max  int
		want int
	}{
		{"well under the cap passes through unchanged", 64, 65536, 64},
		{"exactly at the cap passes through unchanged", 65536, 65536, 65536},
		{"one over the cap is clamped down to it", 65537, 65536, 65536},
		{"an absurdly large soft limit is clamped to the cap", 1 << 40, 65536, 65536},
		{"a zero soft limit yields zero, not the cap", 0, 65536, 0},
		{"a zero cap always yields zero", 100, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := capFDScanLimit(tc.cur, tc.max); got != tc.want {
				t.Errorf("capFDScanLimit(%d, %d) = %d, want %d", tc.cur, tc.max, got, tc.want)
			}
		})
	}
}
