//go:build linux

package lease

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// scanFDsForTargets resolves this process's own open file descriptors via
// /proc/self/fd, whose entries are symlinks to the path (or pseudo-path) each
// fd number refers to. This is Linux-only: macOS's fdesc mount does not
// expose /dev/fd entries as symlinks (os.Readlink fails EINVAL there), and
// the package's cross-process CLOEXEC proof
// (TestCLOEXEC_ChildDoesNotInheritLeaseFd, in lock_process_test.go) no longer
// needs path resolution on ANY platform — the parent already knows the exact
// fd numbers of both the unprotected comparison fd and the lease fd before it
// execs the child, so the child only has to ask fcntl(F_GETFD) about those
// two exact numbers (see fdOpenInThisProcess there). This function, and the
// self-test below, survive purely to keep the path-based technique itself
// under direct, in-process coverage on the one platform where reading it back
// via /proc/self/fd is exact and race-free.
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

// TestScanFDsForTargets_FindsOpenFDByPath is a direct, in-process check of
// scanFDsForTargets above: does opening a file put its path exactly where the
// scan says it belongs, and does a path that was never opened stay absent.
// Linux-only (see the doc comment on scanFDsForTargets for why); this is NOT
// the cross-process proof — that is
// TestCLOEXEC_ChildDoesNotInheritLeaseFd in lock_process_test.go, which runs
// on every unix this package builds for via exact fd numbers instead.
func TestScanFDsForTargets_FindsOpenFDByPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "watched")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer f.Close()

	found, err := scanFDsForTargets(map[string]string{
		"WATCHED": path,
		"MISSING": filepath.Join(dir, "never-opened"),
	})
	if err != nil {
		t.Fatalf("scanFDsForTargets: %v", err)
	}
	if !found["WATCHED"] {
		t.Errorf("scanFDsForTargets did not find the open fd for %s among %v", path, found)
	}
	if found["MISSING"] {
		t.Error("scanFDsForTargets reported a path that was never opened as found")
	}
}
