package workspace

import (
	"os"
	"path/filepath"
	"strings"
)

// HostAgentDir is the on-disk root a pack's host-exec wrappers install under:
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
