//go:build !unix

package hosttrust

import (
	"fmt"
	"os"
)

// openNoFollow is the non-unix fallback for platforms with no O_NOFOLLOW: it
// falls back to an Lstat-then-open sequence, kept ONLY so this package keeps
// building on a non-unix target (see the unix implementation's doc comment
// for the TOCTOU this narrows but does not fully close on this platform). No
// build in this tree currently exercises a non-unix GOOS end to end, so this
// is a good-faith fallback rather than a hardened guarantee.
func openNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%s is a symlink; refusing to follow it", path)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return os.OpenFile(path, flag, perm)
}
