//go:build unix

package lease

import (
	"fmt"
	"os"
	"path/filepath"
)

// RetireAbsentInstance retires a previous lifetime before a new create. The
// caller MUST hold lifecycle EX. The nonblocking refs acquire cannot deadlock
// against a reaper (which also acquires nonblocking), and proves no old session
// is still alive. confirmAbsent must make a fresh runtime probe under these
// locks. Unlike ClearState, this preserves every lock inode: the caller still
// holds lifecycle EX and will acquire refs SH after the new create.
func RetireAbsentInstance(dir string, confirmAbsent func() bool) error {
	refs, err := OpenRefLease(dir)
	if err != nil {
		return err
	}
	defer refs.Close()
	if err := refs.TryExclusive(); err != nil {
		return fmt.Errorf("lease: previous session still holds a reference or cannot be checked: %w", err)
	}
	defer refs.Unlock()
	return withKeepGuard(dir, func() error {
		if confirmAbsent == nil || !confirmAbsent() {
			return fmt.Errorf("lease: sandbox is present or could not be verified absent; previous identity was retained")
		}
		for _, name := range []string{keepFileName, recordFileName} {
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		return nil
	})
}
