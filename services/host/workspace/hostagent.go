package workspace

import (
	"os"
	"path/filepath"
	"strings"
)

// HostAgentDir is PI_CODING_AGENT_DIR for host mode:
// $XDG_STATE_HOME/pix/host-agent (default ~/.local/state/pix/host-agent).
// State-flavored on purpose (rebuildable symlinks + installs, never precious),
// beside tasks/ — honoring XDG_STATE_HOME exactly like taskStateRoot.
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
