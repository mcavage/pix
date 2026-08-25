//go:build windows

package recreatelog

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// lockRangeLow/lockRangeHigh are the byte range LockFileEx/UnlockFileEx lock:
// Windows has no whole-file advisory lock analogous to unix flock, so this
// locks the maximal 0..0xFFFFFFFF:0xFFFFFFFF range instead — the documented
// idiom other cross-platform Go file lockers (e.g. gofrs/flock) use for "the
// entire file, however large it grows".
const (
	lockRangeLow  = 0xFFFFFFFF
	lockRangeHigh = 0xFFFFFFFF
)

// tryLockExclusive attempts a NON-blocking exclusive LockFileEx on f,
// mirroring lock_unix.go's tryLockExclusive contract exactly: true+nil on
// success, false+nil when another holder already has it (so
// withAppendLock's shared retry loop can poll it under the same deadline
// unix does), and a non-nil error only for a genuine failure.
// LOCKFILE_FAIL_IMMEDIATELY is what makes this non-blocking; without it
// LockFileEx blocks the calling thread until the lock is free.
func tryLockExclusive(f *os.File) (bool, error) {
	ol := new(windows.Overlapped)
	err := windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, lockRangeLow, lockRangeHigh, ol,
	)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return false, err
}

// unlockExclusive releases a lock tryLockExclusive acquired.
func unlockExclusive(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, lockRangeLow, lockRangeHigh, ol)
}

// openNoFollow is the Windows fallback for a platform with no atomic
// O_NOFOLLOW at open(2): an Lstat-then-open sequence, the same pattern
// hosttrust/nofollow_other.go and workspace/state_other.go already use for
// this exact gap. This narrows, but does not fully close, the TOCTOU window
// a symlink swapped in between the Lstat and the Open could exploit — a
// documented, good-faith fallback (no build in this tree currently
// exercises Windows end to end), never a stub that skips the check.
func openNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("recreatelog: refusing to follow symlink at %s", path)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return os.OpenFile(path, flag, perm)
}
