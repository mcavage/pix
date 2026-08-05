// launchpack.go — applying the active pack to a LAUNCH.
package launch

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/secret"
	"pix/host/sys"
	"pix/host/workflow/pack"
	"pix/host/workspace"
)

// ApplyPackStackToLaunch applies each active pack in stack order, folding every
// applied pack's MCP servers into the create-time preload set. It returns the
// last effectively-applied root, with the same "" contract as
// ApplyPackToLaunch. warn carries the degrade notes through, unchanged.
func ApplyPackStackToLaunch(cfg *config.Config, o *RunOpts, env hostenv.Env, warn io.Writer) (string, error) {
	roots := pack.ActivePackRoots(cfg, o.Pack)
	if len(roots) == 0 {
		return "", nil
	}
	originalPack, originalOverride := cfg.Pack, o.Pack
	defer func() { cfg.Pack, o.Pack = originalPack, originalOverride }()
	var effective string
	for _, root := range roots {
		cfg.Pack, o.Pack = root, ""
		applied, err := ApplyPackToLaunch(cfg, o, env, warn)
		if err != nil {
			return "", err
		}
		if applied == "" {
			continue
		}
		effective = applied
		p, err := pack.LoadPack(applied)
		if err != nil {
			return "", err
		}
		for _, name := range pack.McpNames(p) {
			if !slices.Contains(o.StaticMCP, name) {
				o.StaticMCP = append(o.StaticMCP, name)
			}
		}
	}
	return effective, nil
}

func ApplyPackToLaunch(cfg *config.Config, o *RunOpts, env hostenv.Env, warn io.Writer) (string, error) {
	packRoot := pack.ActivePackRoot(cfg.Pack, o.Pack)
	if packRoot == "" {
		return "", nil // no active pack (detached or never created)
	}
	p, err := pack.LoadPack(packRoot)
	if err != nil {
		if strings.TrimSpace(o.Pack) != "" {
			return "", fmt.Errorf("--pack %s: %v", o.Pack, err)
		}
		if errors.Is(err, pack.ErrNotAPack) {
			fmt.Fprintf(warn, "pix: active pack unavailable (%v); launching without it — `pix pack use <path>` to re-point it or `pix pack rm` to detach\n", err)
			return "", nil
		}
		return "", fmt.Errorf("active pack %s: %v (refusing to launch without the pack's declared context; fix the pack or `pix pack rm` to detach it)", packRoot, err)
	}
	if err := pack.VerifyPackInferenceTrust(p, cfg.GogAccount, env); err != nil {
		return "", err
	}
	// Apply the exact manifest snapshot whose trust surface was just verified.
	if err := pack.ApplyPackInference(cfg, p.Manifest.Inference, p.Root); err != nil {
		return "", err
	}
	if p.SkillsDir != "" && !slices.Contains(o.Skills, p.SkillsDir) {
		o.Skills = append(o.Skills, p.SkillsDir)
	}
	if m := strings.TrimSpace(p.Manifest.OllamaBridgeModel); m != "" {
		cfg.OllamaBridgeModel = m
	}
	// The ephemeral mixin kit carrying this pack's non-host bin/ wrappers goes
	// in the DEDICATED PackKits field, never o.Kits (see the field doc: folding
	// it into the --kit escape hatch would silently drop the base image kit).
	kit, kerr := pack.SynthesizePackKit(p)
	if kerr != nil {
		return "", fmt.Errorf("pack %s: %v (refusing to launch a sandbox missing a declared wrapper; fix the pack's bin/ or drop the [[proxy]] entry)", p.Manifest.Name, kerr)
	}
	if kit != "" && !slices.Contains(o.PackKits, kit) {
		o.PackKits = append(o.PackKits, kit)
	}
	for _, ig := range p.Manifest.Integrations {
		// CONTAINER integrations (Manifest set) get their credentials
		// Docker-side, so an op-ref warning would be misleading noise. Only
		// op-run-wrapped (host-provided/remote) integrations warn.
		if ig.Env != "" && ig.Manifest == "" && !secret.OpRefFilled(env, ig.Env) {
			if ig.Setup != "" {
				fmt.Fprintf(warn, "pix: pack integration %q is not connected; run: pix setup --pack %s --with %s\n", ig.Name, sys.ShellQuote(p.Root), sys.ShellQuote(ig.Setup))
			} else {
				fmt.Fprintf(warn, "pix: pack integration %q needs a credential; set it: pix secret set %s op://vault/item/field\n", ig.Name, ig.Env)
			}
		}
	}
	// Every pack integration's MCP server is in the preload set. `pack use`
	// already persists them into cfg.MCP, so this only matters for a TRANSIENT
	// --pack override: fold its names in IN MEMORY for this launch. Never
	// Save()d — run/task never call Save() on this cfg — so an override never
	// leaks into the persisted config.
	for _, n := range pack.McpNames(p) {
		if !slices.Contains(cfg.MCP, n) {
			cfg.MCP = append(cfg.MCP, n)
		}
	}
	return packRoot, nil
}

func WritePackContextFiles(cfg *config.Config, o RunOpts, effectivePack string, warn io.Writer) {
	if _, err := workspace.EnsureGitExclude(o.Workspace); err != nil {
		fmt.Fprintf(warn, "pix: could not add .pix ws state to git excludes: %v\n", err)
	}
	WriteOllamaBridgeFile(o.Workspace, cfg.OllamaBridgeModel)
	var activePack *pack.Info
	if effectivePack != "" {
		if lp, lerr := pack.LoadPack(effectivePack); lerr == nil {
			activePack = lp
		}
	}
	pack.WriteMemoryScope(o.Workspace, activePack)
}
