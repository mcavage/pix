//go:build unix

package lease

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ClearState removes the lease state THIS PACKAGE owns for dir — the
// immutable creation record, the identity-bound keep and its guard, and the
// two lock files — and then removes dir itself, unless something this package
// does not own is still in it.
//
// It is the LAST step of a PROVEN teardown, never a repair tool. A caller must
// already hold TryReapProof's kernel-verified zero-holder proof (lifecycle EX
// + refs EX, both non-blocking) and must already have confirmed the sandbox
// itself is gone; clearing the record on any weaker evidence is how a live
// sandbox loses the identity a later teardown would need.
//
// The record goes FIRST, deliberately: it is the file whose survival would
// break the NEXT create for this session key (CreateRecord refuses to relabel
// a lease directory for a different instance), so it is the one removal that
// must not be skipped by a later error in this sequence.
//
// Unlinking a lock file the caller currently HOLDS is safe by construction: an
// flock lives on the open file description, so the lock stays held until that
// fd closes, and any concurrent acquirer notices the unlinked/replaced inode
// (validateLive's errStaleLock) and reopens rather than trusting a lock that
// excludes nobody.
//
// A leftover file this package did not write leaves dir in place (ENOTEMPTY is
// not an error here): deleting state a different owner wrote is not this
// function's call.
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
