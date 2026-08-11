package supervise

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// hostpath.go — where a supervised unit's binary is looked for, and what PATH it
// runs with.
//
// This file exists because "resolved on PATH at launch" quietly means nothing in
// the environment that actually launches serve. launchd hands a login service:
//
//	PATH => /usr/bin:/bin:/usr/sbin:/sbin
//
// and every host binary in this system lives somewhere else — snow-proxy and
// slack-mcp in ~/.local/bin, snow and gog and op under Homebrew. So a daemon
// declared with `command` resolved perfectly in a developer's shell and failed
// permanently under the managed service, which is the ONLY way real users run
// it. The failure even said "is not on PATH", which is actively misleading to
// someone whose interactive PATH does have it.
//
// The same gap hits the CHILD. A daemon inheriting launchd's PATH starts, binds,
// and answers its health check while being unable to find the vendor CLI it
// shells out to — so the capability is broken with every row green, which is the
// exact defect class this release was about.
//
// The list is PINNED IN CODE, not config. It widens where an unpinned binary may
// come from, so it must not be something a manifest can extend: a pack that
// could add a directory could point `command` anywhere and keep the same name on
// the consent screen.

// hostBinDirs are searched IN ADDITION to PATH, in this order, for a host
// binary. They are the three places a Mac puts user-installed tools: pix's own
// convention, Homebrew on Apple Silicon, and Homebrew on Intel plus the general
// /usr/local convention.
func hostBinDirs() []string {
	dirs := []string{"/opt/homebrew/bin", "/usr/local/bin"}
	if home, err := os.UserHomeDir(); err == nil {
		// First: a binary the user built for themselves wins over a packaged one
		// of the same name, which is what `install.sh` writing here means.
		dirs = append([]string{filepath.Join(home, ".local", "bin")}, dirs...)
	}
	return dirs
}

// errCommandNotFound is returned when a unit's command is nowhere to be found.
var errCommandNotFound = errors.New("supervise: command not found")

// resolveHostCommand finds an executable by name: PATH first, so an explicit
// PATH still wins, then the fixed directories.
//
// The error names every place it looked. "not on PATH" was useless under
// launchd, where PATH is four system directories and the answer the user needs
// is which directory the installer should have written to.
func resolveHostCommand(name string) (string, error) {
	if strings.ContainsRune(name, filepath.Separator) {
		return "", fmt.Errorf("%w: %q must be a bare binary name, not a path", errCommandNotFound, name)
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	dirs := hostBinDirs()
	for _, d := range dirs {
		cand := filepath.Join(d, name)
		fi, err := os.Stat(cand)
		if err != nil || fi.IsDir() || fi.Mode().Perm()&0o111 == 0 {
			continue
		}
		return cand, nil
	}
	return "", fmt.Errorf("%w: %q is not in PATH (%s) or any of %s",
		errCommandNotFound, name, os.Getenv("PATH"), strings.Join(dirs, ", "))
}

// withHostBinPath appends the same fixed directories to the child's PATH, so a
// daemon can find the tools it shells out to.
//
// It rewrites PATH only when the unit's allowlist ALREADY granted it. A unit
// that did not ask for PATH must not be handed one — the allowlist is the whole
// contract, and widening it here would make it a suggestion.
func withHostBinPath(env []string) []string {
	out := make([]string, len(env))
	copy(out, env)
	for i, kv := range out {
		if !strings.HasPrefix(kv, "PATH=") {
			continue
		}
		cur := strings.TrimPrefix(kv, "PATH=")
		have := make(map[string]bool)
		for _, d := range filepath.SplitList(cur) {
			have[d] = true
		}
		add := make([]string, 0, 3)
		for _, d := range hostBinDirs() {
			if !have[d] {
				add = append(add, d)
			}
		}
		if len(add) > 0 {
			out[i] = "PATH=" + cur + string(os.PathListSeparator) + strings.Join(add, string(os.PathListSeparator))
		}
		break
	}
	return out
}
