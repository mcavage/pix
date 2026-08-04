// launchpack.go — applying the active pack to a LAUNCH.
//
// These live on the launch side, not in pack.go, because that is whose
// question they answer: they take run's RunOpts, write into the workspace a
// sandbox is about to start in, and are called only from `pix run` and
// `pix task new`. Filing them under pack made pack look coupled to run when
// what was actually true is that run is coupled to pack, which is the correct
// direction and the one the layering allows.
package launch

import (
	"errors"
	"fmt"
	"os"
	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/secret"
	"pix/host/sys"
	"pix/host/workflow/pack"
	"pix/host/workspace"
	"slices"
	"strings"
)

func ApplyPackStackToLaunch(cfg *config.Config, o *RunOpts, env hostenv.Env) (string, error) {
	roots := pack.ActivePackRoots(cfg, o.Pack)
	if len(roots) == 0 {
		return "", nil
	}
	originalPack, originalOverride := cfg.Pack, o.Pack
	defer func() { cfg.Pack, o.Pack = originalPack, originalOverride }()
	var effective string
	for _, root := range roots {
		cfg.Pack, o.Pack = root, ""
		applied, err := ApplyPackToLaunch(cfg, o, env)
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
// appends the pack's skills dir to o.Skills, applies the pack's ollama model
// pref, synthesizes + stacks the pack's sandbox bin/ mixin kit (F2), and warns
// about a missing integration credential. A pack's `integration.mcp` is NOT
// warned about here (v1 behavior): F1 enables it into cfg.MCP at `pack use`
// time, so BuildSbxArgs' existing --mcp loop already attaches it on the next
// create — nothing new needed in the arg builder, and warning here would be
// stale noise for an already-attached pack. Knowledge is NOT handled here
// either: a pack's embedded knowledge/ dir (p.KnowledgeDir) is INERT —
// mounted like skills/, but nothing indexes it (the built-in OKF knowledge
// service was retired, W2 U03A) — so there is no bundle state to scope for a
// transient --pack override. An EXPLICIT
// --pack that fails to load is fatal (a non-nil return the caller must treat
// as launch-aborting). The CONFIGURED active pack (cfg.Pack, or the default
// pack fallback) fails CLOSED too when it exists but won't load — a symlink
// rejection, facet-validation failure, or parse error means a broken or
// TAMPERED pack, and launching without its declared wrappers/skills would be
// a silent downgrade. The ONLY degradable case is pack.ErrNotAPack ("genuinely
// absent": the dir or its pack.toml is gone), which warns once and proceeds
// as if no pack were active. A declared-but-unbuildable sandbox proxy is ALSO
// fatal (round-4 F2): the launch fails CLOSED rather than creating a sandbox
// missing a declared wrapper.
//
// It returns the EFFECTIVE active-pack root it actually applied: "" when there
// is no active pack, OR when it degraded via pack.ErrNotAPack (genuinely absent —
// see below); the real packRoot when a pack loaded and its context was mounted
// onto o/cfg. Callers (run.go, task.go) MUST write the sandbox.pack marker and
// scope memory from this returned root, never from pack.ActivePackRoot(cfg.Pack,
// o.Pack) directly — the configured path can name a pack that degraded to
// pack-less this launch, and recording it anyway would make a later
// StalePackReattachWarning wrongly stay silent (marker == active) even though
// the sandbox never got the pack's create-time facets.
func ApplyPackToLaunch(cfg *config.Config, o *RunOpts, env hostenv.Env) (string, error) {
	packRoot := pack.ActivePackRoot(cfg.Pack, o.Pack)
	if packRoot == "" {
		return "", nil // no active pack; nothing to mount (detached or never created)
	}
	p, err := pack.LoadPack(packRoot)
	if err != nil {
		if strings.TrimSpace(o.Pack) != "" {
			return "", fmt.Errorf("--pack %s: %v", o.Pack, err)
		}
		if errors.Is(err, pack.ErrNotAPack) {
			// Genuinely absent (deleted dir / no pack.toml): warn and launch
			// without it, as if no pack were active. Not fatal — a stale
			// cfg.Pack must not brick every launch. The pack did NOT apply, so
			// the effective root is "" — the caller must not mark this launch
			// as having this pack.
			fmt.Fprintf(os.Stderr, "pix: active pack unavailable (%v); launching without it — `pix pack use <path>` to re-point it or `pix pack rm` to detach\n", err)
			return "", nil
		}
		// The pack EXISTS but won't load (symlink injected, validation/parse
		// failure): fail the launch closed. Creating a sandbox from a broken or
		// tampered active pack would silently drop its declared context.
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
	// F2: synthesize (or refresh) the ephemeral mixin kit carrying this pack's
	// non-host bin/ wrappers, and stack it via the DEDICATED PackKits field (never
	// o.Kits — see the PackKits field doc for why folding it into the --kit
	// escape hatch would silently drop the base image kit). FAIL CLOSED at the
	// launch boundary (round-4 F2): a pack that DECLARES a sandbox proxy whose
	// kit can't be built must abort the launch — pack.SynthesizePackKit distinguishes
	// "no proxies declared" (("", nil): fine, no kit) from "declared but
	// unbuildable" (("", err)), so a sandbox is never created silently missing a
	// wrapper the pack promised it.
	kit, kerr := pack.SynthesizePackKit(p)
	if kerr != nil {
		return "", fmt.Errorf("pack %s: %v (refusing to launch a sandbox missing a declared wrapper; fix the pack's bin/ or drop the [[proxy]] entry)", p.Manifest.Name, kerr)
	}
	if kit != "" && !slices.Contains(o.PackKits, kit) {
		o.PackKits = append(o.PackKits, kit)
	}
	for _, ig := range p.Manifest.Integrations {
		// CONTAINER integrations (Manifest set) get their credentials Docker-side,
		// not from op-refs — so an op-ref warning would be misleading noise. Only
		// warn for op-run-wrapped (host-provided/remote) integrations.
		if ig.Env != "" && ig.Manifest == "" && !secret.OpRefFilled(env, ig.Env) {
			if ig.Setup != "" {
				fmt.Fprintf(os.Stderr, "pix: pack integration %q is not connected; run: pix setup --pack %s --with %s\n", ig.Name, sys.ShellQuote(p.Root), sys.ShellQuote(ig.Setup))
			} else {
				fmt.Fprintf(os.Stderr, "pix: pack integration %q needs a credential; set it: pix secret set %s op://vault/item/field\n", ig.Name, ig.Env)
			}
		}
	}
	// S01: every pack integration's MCP server is in the preload set — no more
	// eager/lazy split. `pack use` already persists each integration's server
	// into cfg.MCP (pack.McpNames/F1, above), so this only matters for a
	// TRANSIENT --pack override that was never `pack use`d: fold its
	// integration names into cfg.MCP IN MEMORY for this launch only. Never
	// Save()d — run.go/task.go never call Save() on this cfg after
	// ApplyPackToLaunch, so a --pack override never leaks into the persisted
	// config.
	for _, n := range pack.McpNames(p) {
		if !slices.Contains(cfg.MCP, n) {
			cfg.MCP = append(cfg.MCP, n)
		}
	}
	return packRoot, nil
}

// WritePackContextFiles writes the two per-launch, pack-scoped workspace files
// that carry the active pack's context into a sandbox: the ollama-bridge model
// (.pix/ollama-bridge.model, via WriteOllamaBridgeFile) and the memory
// scope (.pix/profile, via pack.WriteMemoryScope, resolved from the active
// pack). Shared by `pix run` and `pix task new` so a task sandbox
// gets the SAME pack context as a normal run — packs-v2 Phase 1 had `run`
// write these but not `task new`. Best-effort throughout: an unloadable pack
// degrades to unscoped memory rather than failing the launch (mirrors
// pack.WriteMemoryScope's own contract).
//
// effectivePack is the root ApplyPackToLaunch actually applied (its returned
// value) — NOT pack.ActivePackRoot(cfg.Pack, o.Pack) directly. This keeps memory
// scoping honest with the sandbox.pack marker: when ApplyPackToLaunch
// degraded (pack.ErrNotAPack), effectivePack is "" and memory stays unscoped, even
// though cfg.Pack/o.Pack still name the (unavailable) configured pack.
func WritePackContextFiles(cfg *config.Config, o RunOpts, effectivePack string) {
	if _, err := workspace.EnsureGitExclude(o.Workspace); err != nil {
		fmt.Fprintf(os.Stderr, "pix: could not add .pix ws state to git excludes: %v\n", err)
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
