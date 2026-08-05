// Package launcher is the running binary's own identity: its version, and the
// sibling pix-host it is paired with.
package launcher

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Version is stamped at build time via -ldflags. An unstamped build reports
// "dev" and tracks the kit's main branch.
var Version = "dev"

// FindHostBinary resolves the sibling pix-host and verifies it reports the SAME
// version as this binary. A mismatch is an error, not a warning: two halves of
// one release that disagree is exactly the state that produces bugs nobody can
func FindHostBinary() (string, error) {
	verify := func(path string) (string, error) {
		out, err := exec.Command(path, "version").CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("pix-host at %s cannot report its version: %v", path, err)
		}
		hostVersion := strings.TrimSpace(string(out))
		if hostVersion != Version {
			return "", fmt.Errorf("pix-host version %q at %s does not match pix version %q; reinstall both binaries together", hostVersion, path, Version)
		}
		return path, nil
	}
	if self, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(self), "pix-host")
		if fi, err := os.Stat(sibling); err == nil && !fi.IsDir() {
			return verify(sibling)
		}
	}
	if p, err := exec.LookPath("pix-host"); err == nil {
		return verify(p)
	}
	return "", fmt.Errorf("pix-host not found next to this binary or on PATH")
}
