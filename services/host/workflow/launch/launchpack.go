// launchpack.go writes the small workspace-local files the in-VM
// ollama-bridge and recall/capture extensions read at session start. The
// pack-shaped contribution this file used to fold in (skills/knowledge
// dirs, mixin kits, preloaded MCP servers verified by workflow/pack) was
// deleted in the Pix v2 cutover along with the pack system itself
// (docs/design/pix-v2-architecture.md §14) — v2 has no `--pack` flag and no
// per-workspace memory scope marker.
package launch

import (
	"fmt"
	"io"

	"pix/host/config"
	"pix/host/workspace"
)

// WriteWorkspaceContextFiles writes the workspace-local hints a launch always
// leaves behind: the git-exclude entry for .pix/ workspace state, the active
// Ollama bridge model, and the memory-capture mode. Best-effort: a failure to
// add the git exclude only warns, it never blocks a launch.
func WriteWorkspaceContextFiles(cfg *config.Config, o RunOpts, warn io.Writer) {
	if _, err := workspace.EnsureGitExclude(o.Workspace); err != nil {
		fmt.Fprintf(warn, "pix: could not add .pix ws state to git excludes: %v\n", err)
	}
	WriteOllamaBridgeFile(o.Workspace, cfg.OllamaBridgeModel)
	WriteMemoryCaptureFile(o.Workspace, cfg.MemoryCapture)
}
