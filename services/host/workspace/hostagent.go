package workspace

import (
	"os"
	"path/filepath"
	"strings"
)

// HostAgentDir is the on-disk location a PRE-retirement `pix host` used to
// provision as PI_CODING_AGENT_DIR: $XDG_STATE_HOME/pix/host-agent (default
// ~/.local/state/pix/host-agent). `pix host` (the unsandboxed escape hatch) was
// retired and its launcher/provisioning code deleted — nothing installs into
// this dir any more — but the path is kept as the well-known location a pack's
// `pack rm` still tidies up a PRE-retirement install's leftover wrapper
// binaries from (workflow/pack's clearHostPackWrappers), so an upgrader is
// never left with stale executables under a directory nothing references any
// more.
func HostAgentDir() string {
	if x := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); x != "" {
		return filepath.Join(x, "pix", "host-agent")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".local", "state", "pix", "host-agent")
}

// TaskArtifactRoot is the durable base dir for harvested artifacts:
// $XDG_DATA_HOME/pix/artifacts (default ~/.local/share/pix/artifacts).
// Deliberately under DATA_HOME, not the STATE tree the clones live in, so
// `state reset`/`uninstall` never reach it.
func TaskArtifactRoot() string {
	if x := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); x != "" {
		return filepath.Join(x, "pix", "artifacts")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".local", "share", "pix", "artifacts")
}

// TaskStateRoot is the base dir for all task state:
// $XDG_STATE_HOME/pix/tasks (default ~/.local/state/pix/tasks).
func TaskStateRoot() string {
	if x := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); x != "" {
		return filepath.Join(x, "pix", "tasks")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".local", "state", "pix", "tasks")
}
