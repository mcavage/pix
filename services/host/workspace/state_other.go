//go:build !unix

// state_other.go is the portable fallback for hosts with no O_NOFOLLOW at
// open(2) (Windows): the same Lstat-then-open check state.go always used,
// kept here for platforms that cannot refuse the symlink atomically. The
// residual check-to-open TOCTOU window is an accepted tradeoff on this path
// only — the posture this package documents for staying portable.
package workspace

import (
	"fmt"
	"os"
)

func readStateFileNoFollow(path string) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symlink; refusing to read workspace state through it", path)
	}
	return os.ReadFile(path)
}
