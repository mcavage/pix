// launchpack.go — the pack-shaped inputs to a LAUNCH, on the launch side of the
// boundary. Resolving and TRUST-VERIFYING the active pack stack is workflow/
// pack's authority (see its launch.go); launch is handed the resulting
// packinfo.LaunchContribution by the composition root and only folds it in.
package launch

import (
	"fmt"
	"io"
	"slices"

	"pix/host/config"
	"pix/host/packinfo"
	"pix/host/workspace"
)

// ApplyPackContribution folds a VERIFIED pack stack's contribution into these
// launch options and returns the effective pack root ("" when no pack applied).
// Every fold is deduplicated against what the user already asked for, so a pack
// can add a skill dir, a mixin kit or a preloaded MCP server but never repeat
// one.
func (o *RunOpts) ApplyPackContribution(c packinfo.LaunchContribution) string {
	for _, dir := range c.Skills {
		if !slices.Contains(o.Skills, dir) {
			o.Skills = append(o.Skills, dir)
		}
	}
	// The ephemeral mixin kits carrying a pack's non-host bin/ wrappers go in the
	// DEDICATED PackKits field, never o.Kits (see the field doc: folding them into
	// the --kit escape hatch would silently drop the base image kit).
	for _, kit := range c.Kits {
		if !slices.Contains(o.PackKits, kit) {
			o.PackKits = append(o.PackKits, kit)
		}
	}
	for _, name := range c.MCPNames {
		if !slices.Contains(o.StaticMCP, name) {
			o.StaticMCP = append(o.StaticMCP, name)
		}
	}
	return c.Root
}

func WritePackContextFiles(cfg *config.Config, o RunOpts, effectivePack string, warn io.Writer) {
	if _, err := workspace.EnsureGitExclude(o.Workspace); err != nil {
		fmt.Fprintf(warn, "pix: could not add .pix ws state to git excludes: %v\n", err)
	}
	WriteOllamaBridgeFile(o.Workspace, cfg.OllamaBridgeModel)
	var activePack *packinfo.Info
	if effectivePack != "" {
		if lp, lerr := packinfo.LoadPack(effectivePack); lerr == nil {
			activePack = lp
		}
	}
	packinfo.WriteMemoryScope(o.Workspace, activePack)
}
