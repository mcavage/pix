//go:build unix

package lease

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func ClearState(dir string) error {
	if err := refuseSymlink(dir); err != nil {
		return err
	}
	var errs []error
	for _, name := range []string{
		recordFileName,
		keepFileName,
		keepFileName + ".tmp",
		keepLockFileName,
		refsLockFileName,
		lifecycleLockFileName,
	} {
		// os.Remove unlinks the NAME, so a symlink at one of these paths is
		// removed rather than followed — there is no target to clobber.
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("lease: clear %s: %w", name, err))
		}
	}
	if err := os.Remove(dir); err != nil && !os.IsNotExist(err) && !isNotEmpty(err) {
		errs = append(errs, fmt.Errorf("lease: remove lease dir %s: %w", dir, err))
	}
	return errors.Join(errs...)
}

// isNotEmpty reports the "directory still has entries" errno, which POSIX
// permits an implementation to report as either ENOTEMPTY or EEXIST (macOS
// and Linux differ on some filesystems), so both are treated as the same
// non-error outcome.
func isNotEmpty(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST)
}
