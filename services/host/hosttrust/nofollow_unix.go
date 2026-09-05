//go:build unix

package hosttrust

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// openNoFollow opens path with O_NOFOLLOW so a symlink at path is refused at
// the SAME syscall that does the open, not a separate Lstat beforehand — the
// TOCTOU an Lstat-then-open (or Lstat-then-ReadFile) sequence leaves is a
// window where a symlink swapped in between the two calls is silently
// followed by the second one. This is the same primitive
// services/host/lease and services/host/recreatelog use, duplicated here
// rather than imported: an L1 package may not import an L1 sibling
// (doc.go), and O_NOFOLLOW is a few lines, not a shared dependency worth a
// boundary exception for.
func openNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	fd, err := syscall.Open(path, flag|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, uint32(perm))
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("%s is a symlink; refusing to follow it", path)
		}
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}
