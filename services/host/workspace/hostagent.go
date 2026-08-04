package workspace

import (
	"os"
	"path/filepath"
	"strings"
)

// HostAgentDir is the on-disk root a pack's host-exec wrappers install under:
// $XDG_STATE_HOME/pix/host-agent (default ~/.local/state/pix/host-agent).
// `pix host` (the unsandboxed escape hatch that once also provisioned this
// dir as PI_CODING_AGENT_DIR) was retired — but workflow/pack's
// RefreshHostPackWrappers/installHostPackWrappersStaged still install a
// pack's accepted [[bin]]/host-proxy set into HostPackBinDir() (this dir's
// bin/ subdir) at `pack use`, and `pack rm` tidies it back up
// (clearHostPackWrappers) — this is core pack host-exec, not host-mode
// execution.
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
