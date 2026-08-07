//go:build linux

package lease

import (
	"fmt"
	"os"
	"path/filepath"
)

// scanFDsForTargets resolves this process's own open file descriptors via
// /proc/self/fd, whose entries are symlinks to the path (or pseudo-path) each
// fd number refers to. See lock_process_fds_darwin_test.go for the Darwin
// analogue, which cannot use readlink because macOS fd entries are not
// symlinks.
func scanFDsForTargets(targets map[string]string) (map[string]bool, error) {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return nil, fmt.Errorf("readdir /proc/self/fd: %w", err)
	}
	found := map[string]bool{}
	for _, e := range entries {
		link, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name()))
		if err != nil {
			continue
		}
		for label, target := range targets {
			if target != "" && link == target {
				found[label] = true
			}
		}
	}
	return found, nil
}
