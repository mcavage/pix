//go:build unix

package recreatelog

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// tryLockExclusive attempts a NON-blocking exclusive flock on f, reporting
// whether it was acquired. "Would block" (another holder has it) is not an
// error — withAppendLock's retry loop is what turns repeated false results
// into a bounded wait; only a genuine syscall failure is an error here.
func tryLockExclusive(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return false, nil
	}
	return false, err
}

// unlockExclusive releases a lock tryLockExclusive acquired.
func unlockExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// openNoFollow opens path with O_NOFOLLOW so a symlink at path is refused
// (ELOOP), never followed — the same primitive services/host/lease uses,
// duplicated here rather than imported: recreatelog holds zero internal
// imports on purpose (see guard_test.go's F10 and doc.go's "no L1 siblings").
func openNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	fd, err := syscall.Open(path, flag|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, uint32(perm))
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("recreatelog: refusing to follow symlink at %s", path)
		}
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}
