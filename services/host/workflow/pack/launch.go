// launch.go — what the active pack stack contributes to a LAUNCH, and the trust
// gate that must pass before any of it counts.
//
// This is the pack side of a deliberate split. workflow/launch owns the launch
// options and folds the RESULT in (its launchpack.go); it may not ask a sibling
// workflow for the pack, and it must never be the thing that decides a pack's
// declared inference endpoint is still the accepted one. So verification and
// projection live here, next to the trust store they answer to, and the
// composition root hands the resulting packinfo.LaunchContribution to launch.
package pack

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/packinfo"
	"pix/host/secret"
	"pix/host/sys"
)

// ResolveLaunchContribution verifies and projects each active pack in stack
// order, folding every applied pack's MCP servers into the create-time preload
// set. c.Root is the last effectively-applied root, with the same "" contract as
// ApplyToLaunch. warn carries the degrade notes through, unchanged.
func ResolveLaunchContribution(cfg *config.Config, override string, env hostenv.Env, warn io.Writer) (packinfo.LaunchContribution, error) {
	var c packinfo.LaunchContribution
	roots := packinfo.ActivePackRoots(cfg, override)
	if len(roots) == 0 {
		return c, nil
	}
	originalPack := cfg.Pack
	defer func() { cfg.Pack = originalPack }()
	for _, root := range roots {
		cfg.Pack = root
		// override travels INTO the per-pack call: an explicit `--pack` that does
		// not load is fatal, which is what run.go promises and what blanking it
		// here used to quietly downgrade to a warn-and-degrade.
		applied, err := ApplyToLaunch(cfg, override, env, warn, &c)
		if err != nil {
			return packinfo.LaunchContribution{}, err
		}
		if applied == "" {
			continue
		}
		c.Root = applied
		p, err := packinfo.LoadPack(applied)
		if err != nil {
			return packinfo.LaunchContribution{}, err
		}
		for _, name := range packinfo.McpNames(p) {
			if !slices.Contains(c.MCPNames, name) {
				c.MCPNames = append(c.MCPNames, name)
			}
		}
	}
	return c, nil
}

// ApplyToLaunch verifies ONE pack's trust surface and appends what it
// contributes to c, returning the root it applied ("" when there is no active
// pack, or the active one is genuinely absent and the launch may degrade).
// override is the explicit `--pack` value, which sharpens the load error.
func ApplyToLaunch(cfg *config.Config, override string, env hostenv.Env, warn io.Writer, c *packinfo.LaunchContribution) (string, error) {
	packRoot := packinfo.ActivePackRoot(cfg.Pack, override)
	if packRoot == "" {
		return "", nil // no active pack (detached or never created)
	}
	p, err := packinfo.LoadPack(packRoot)
	if err != nil {
		if strings.TrimSpace(override) != "" {
			return "", fmt.Errorf("--pack %s: %v", override, err)
		}
		if errors.Is(err, packinfo.ErrNotAPack) {
			fmt.Fprintf(warn, "pix: active pack unavailable (%v); launching without it — `pix pack use <path>` to re-point it or `pix pack rm` to detach\n", err)
			return "", nil
		}
		return "", fmt.Errorf("active pack %s: %v (refusing to launch without the pack's declared context; fix the pack or `pix pack rm` to detach it)", packRoot, err)
	}
	if err := VerifyPackLaunchTrust(p, env); err != nil {
		return "", err
	}
	// Apply the exact manifest snapshot whose trust surface was just verified.
	if err := ApplyPackInference(cfg, p.Manifest.Inference, p.Root); err != nil {
		return "", err
	}
	if p.SkillsDir != "" && !slices.Contains(c.Skills, p.SkillsDir) {
		c.Skills = append(c.Skills, p.SkillsDir)
	}
	if m := strings.TrimSpace(p.Manifest.OllamaBridgeModel); m != "" {
		cfg.OllamaBridgeModel = m
	}
	kit, kerr := SynthesizePackKit(p)
	if kerr != nil {
		return "", fmt.Errorf("pack %s: %v (refusing to launch a sandbox missing a declared wrapper; fix the pack's bin/ or drop the [[proxy]] entry)", p.Manifest.Name, kerr)
	}
	if kit != "" && !slices.Contains(c.Kits, kit) {
		c.Kits = append(c.Kits, kit)
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
	for _, n := range packinfo.McpNames(p) {
		if !slices.Contains(cfg.MCP, n) {
			cfg.MCP = append(cfg.MCP, n)
		}
	}
	return packRoot, nil
}
