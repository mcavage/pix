// launchpack.go — applying the active pack to a LAUNCH.
//
// These live on the launch side, not in pack.go, because that is whose question
// they answer: they take run's RunOpts, write into the workspace a sandbox is
// about to start in, and are called only from `pix run` and `pix task new`.
// Filing them under pack made pack look coupled to run when what is true is
// that run is coupled to pack.
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

// ApplyPackToLaunch mounts the active pack into a launch (run OR task): it
// appends the pack's skills dir to o.Skills, applies its ollama model pref,
// synthesizes + stacks its sandbox bin/ mixin kit, folds its integration MCP
// servers into cfg.MCP, and warns about a missing integration credential.
//
// Failure posture. An EXPLICIT --pack that fails to load is fatal. The
// CONFIGURED active pack fails CLOSED too when it exists but won't load — a
// symlink rejection, facet-validation failure or parse error means a broken or
// TAMPERED pack, and launching without its declared wrappers/skills would be a
// silent downgrade. The ONLY degradable case is pack.ErrNotAPack (the dir or
// its pack.toml is genuinely gone), which warns once and proceeds pack-less. A
// declared-but-unbuildable sandbox proxy is ALSO fatal: never create a sandbox
// missing a wrapper the pack promised.
//
// It returns the EFFECTIVE root it actually applied: "" when there is no active
// pack OR when it degraded via pack.ErrNotAPack, the real root otherwise.
// Callers MUST write the sandbox.pack marker and scope memory from THIS value,
// never from pack.ActivePackRoot directly — the configured path can name a pack
// that degraded to pack-less this launch, and recording it anyway would make a
// later StalePackReattachWarning wrongly stay silent.
//
// warn receives the degrade + unconnected-integration notes. It is the
// caller's stream: this package never writes to a stream of its own choosing,
// so a test reads these notes instead of racing the process's stderr.
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
	// Reloading before projection would reopen a verify-then-use window for
	// credential endpoint metadata.
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
	// SynthesizePackKit distinguishes "no proxies declared" (("", nil): fine)
	// from "declared but unbuildable" (("", err)), which aborts the launch.
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

// WritePackContextFiles writes the two per-launch, pack-scoped workspace files
// that carry the active pack's context into a sandbox: the ollama-bridge model
// and the memory scope. Shared by `pix run` and `pix task new` so a task
// sandbox gets the SAME pack context as a normal run. Best-effort throughout:
// an unloadable pack degrades to unscoped memory rather than failing the
// launch.
//
// effectivePack is the root ApplyPackToLaunch actually applied, NOT
// pack.ActivePackRoot — that keeps memory scoping honest with the sandbox.pack
// marker when the pack degraded. warn takes the one note it can emit.
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
