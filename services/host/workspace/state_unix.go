//go:build unix

// state_unix.go gives ReadStateFile's FILE-level symlink refusal to the
// kernel: O_NOFOLLOW at open(2) means the open itself fails on a symlink,
// closing the Lstat-then-open TOCTOU window a separate check-then-read
// leaves open (something swaps the destination for a symlink between the
// check and the read). The directory-level check in state.go stays
// Lstat-based on every platform — a directory lookup has no comparable
// O_NOFOLLOW-guarded open to hook here, and that window is not this fix's
// target.
package workspace

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// readStateFileNoFollow reads path via O_NOFOLLOW: a symlink at path is
// refused atomically by the open(2) call, never followed. A missing path
// still returns an os.IsNotExist-wrapping error, same as before.
func readStateFileNoFollow(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("%s is a symlink; refusing to read workspace state through it", path)
		}
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}
