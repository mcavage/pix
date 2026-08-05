package launch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/mcp"
	"pix/host/inference"
	"pix/host/sys"
	"pix/host/workflow/doctor"
	"pix/host/workflow/pack"
	"pix/host/workspace"
)

// ApplyConfiguredSessionModel gives every interactive entry point (`run` and
// `task new`) the same top-level model. Before this seam existed task sandboxes
// skipped run_intent entirely and silently inherited Pi's provider default.
// The bool reports whether a configured intent (including the explicit
// none/off opt-out) applied; callers decide how to present resolver errors.
func ApplyConfiguredSessionModel(o *RunOpts, cfg *config.Config) (bool, error) {
	if o == nil || cfg == nil || o.Model != "" || o.Intent != "" {
		return false, nil
	}
	intent := strings.TrimSpace(cfg.RunIntent)
	if intent == "" {
		return false, nil
	}
	if strings.EqualFold(intent, "none") || strings.EqualFold(intent, "off") {
		return true, nil
	}
	model, err := inference.ResolveSessionModel(intent)
	if err != nil {
		return true, err
	}
	o.Intent = intent
	o.Model = model
	return true, nil
}

// Creation-evidence poll seams. After `sbx run` is STARTED (not waited), the
// create path polls for the named sandbox to become visible through
// SandboxAppearProbeFn, records the create receipt the moment it is, and only
// then settles into Wait — so status/doctor can render preload provenance
// WHILE the interactive session is alive, not hours later when it exits.
// Injectable so tests never shell out or sleep for real; production polls
// `sbx ls` via ProbeTaskSandbox. The timeout is deliberately generous: a
// first create may pull the image for minutes before the sandbox exists, and
// the poll only runs while `sbx run` itself is still alive, so a large bound
// costs the happy path nothing.
var (
	SandboxAppearProbeFn = func(name string) SbxState {
		return ProbeTaskSandbox(DefaultEnv(), name)
	}
	SandboxAppearPollInterval = 500 * time.Millisecond
	SandboxAppearPollTimeout  = 15 * time.Minute
)

// sandboxAppeared reports whether st is POSITIVE existence evidence: the name
// is present in `sbx ls`, running or not. Absent keeps polling; unknown (a
// failed probe) proves nothing and also keeps polling — never record a create
// receipt on an indeterminate read.
func sandboxAppeared(st SbxState) bool { return st == SbxRunning || st == SbxStopped }

// RecordCreateReceipt commits the create receipt for sandbox — called ONLY by
// ExecSbxRunAndRecordCreate, once its creation-evidence poll has positively
// seen run.go's OWN `sbx run` create appear. preloaded is the EXACT
// --static-mcp set that launch emitted (o.StaticMCP:
// mcp.AllPreloadedMCP(cfg.MCP+o.MCP), which already folds in every
// active/transient pack integration's MCP server — ApplyPackToLaunch runs
// before this set is computed), so a receipt read later never disagrees with
// what create actually requested. merge=true (the normal path: the
// pre-create clear succeeded) preserves loads a concurrent `pix mcp
// load` appended during the create window; merge=false (the clear could not
// be proven) replaces outright so a prior lifetime's loads can never survive.
// workspace is the CANONICAL workspace path the create was for
// (workspace.CanonicalPath) — the receipt's workspace->sandbox identity that
// workspace.ResolveSandbox reads back for custom-named sandboxes.
func RecordCreateReceipt(sandbox, ws string, preloaded []string, merge bool) error {
	dir, err := workspace.MCPStateDirFn()
	if err != nil {
		return &workspace.ReceiptRecordError{Op: "create", Sandbox: sandbox, Err: fmt.Errorf("resolving pix state dir: %w", err)}
	}
	var werr error
	if merge {
		werr = workspace.CommitCreateReceipt(dir, sandbox, ws, preloaded, nil)
	} else {
		werr = workspace.WriteCreateReceipt(dir, sandbox, ws, preloaded, nil)
	}
	if werr != nil {
		return &workspace.ReceiptRecordError{Op: "create", Sandbox: sandbox, Err: werr}
	}
	return nil
}

// ExecSbxRunAndRecordCreate runs cmd (the already-composed `sbx run ...`
// invocation, stdio already wired by the caller — Start/Wait preserve it) and
// owns the create-receipt lifecycle around it:
//
//   - writeReceipt=false (a plain re-attach, or an inconclusive SbxUnknown
//     probe — see DefinitelyCreating): cmd.Run() and nothing else. A re-attach
//     writes nothing, clears nothing.
//   - writeReceipt=true (a definite create/replace): any stale receipt from a
//     prior same-name lifetime is CLEARED under the per-sandbox lock BEFORE
//     the create starts; then cmd is STARTED and the sandbox's appearance is
//     polled (SandboxAppearProbeFn, bounded by SandboxAppearPollTimeout).
//     The moment it appears the receipt is committed — while the interactive
//     session is still alive — merging any loads recorded since the clear;
//     then we Wait for the session.
//
// Outcome contract: if the process exits BEFORE creation evidence, its error
// is returned and no receipt is written (a final probe on a CLEAN exit still
// records — evidence found at exit is evidence). If the receipt cannot be
// recorded after the sandbox positively appeared (or the poll timed out with
// the session still running), the session is still waited to completion and
// the failure surfaces as *workspace.ReceiptRecordError — the caller reports
// "launched/attached, but state unrecorded" and exits non-zero, never a
// silent success and never confused with a launch failure. The Wait goroutine
// always terminates when the process exits and its result is always drained
// — no goroutine leaks on any path.
func ExecSbxRunAndRecordCreate(cmd *exec.Cmd, writeReceipt bool, sandbox, ws string, preloaded []string) error {
	if !writeReceipt {
		return cmd.Run()
	}

	// Pre-create clear (B): under the same per-sandbox lock the writers use,
	// drop any receipt from a previous incarnation of this name so its load
	// history can never leak into the new lifetime. merge stays false unless
	// the clear POSITIVELY succeeded — the commit then merges only loads
	// appended after this point; an unproven clear degrades to a plain
	// replace, which cannot resurrect old loads.
	merge := false
	if stateDir, err := workspace.MCPStateDirFn(); err == nil {
		if err := workspace.ClearMCPReceipt(stateDir, sandbox); err == nil {
			merge = true
		}
	}

	if err := cmd.Start(); err != nil {
		return err
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	var recErr error
	deadline := time.Now().Add(SandboxAppearPollTimeout)
	ticker := time.NewTicker(SandboxAppearPollInterval)
	defer ticker.Stop()
poll:
	for {
		if sandboxAppeared(SandboxAppearProbeFn(sandbox)) {
			recErr = RecordCreateReceipt(sandbox, ws, preloaded, merge)
			break poll
		}
		if time.Now().After(deadline) {
			recErr = &workspace.ReceiptRecordError{Op: "create", Sandbox: sandbox,
				Err: fmt.Errorf("timed out after %s waiting for the sandbox to appear in `sbx ls`; its preloaded MCP set was not recorded", SandboxAppearPollTimeout)}
			break poll
		}
		select {
		case werr := <-waitCh:
			// The process exited before creation evidence. A failed exec
			// surfaces its OWN error, receiptless. A clean exit gets ONE final
			// probe (the sandbox may have appeared exactly as it exited, e.g. a
			// detached create); still no evidence means honestly no receipt.
			if werr != nil {
				return werr
			}
			if sandboxAppeared(SandboxAppearProbeFn(sandbox)) {
				return RecordCreateReceipt(sandbox, ws, preloaded, merge)
			}
			return nil
		case <-ticker.C:
		}
	}
	// Receipt outcome decided (recorded, failed, or timed out) — now hand the
	// terminal back to the session and wait it out. Its own failure dominates
	// the report; a receipt failure surfaces only on a clean session exit.
	if werr := <-waitCh; werr != nil {
		return werr
	}
	return recErr
}

// ApplyReplaceRm runs the plan's RmFirst step (`sbx rm -f <name>`) via env when
// required, and MUST be checked by the caller: a failed rm means the old
// sandbox may still exist under that name, and proceeding to create against it
// anyway is undefined (sbx may error, or silently reattach to a sandbox with
// stale kit/mcp/create-only flags — exactly what --replace was trying to avoid).
// A no-op (nil) when the plan doesn't call for it.
func ApplyReplaceRm(env hostenv.Env, plan RunLaunchPlan, name string) error {
	if !plan.RmFirst {
		return nil
	}
	if _, err := env.Run("sbx", "rm", "-f", name); err != nil {
		return fmt.Errorf("could not remove existing sandbox %q to replace it: %w", name, err)
	}
	// The launcher itself removed this sandbox, so its MCP receipt describes a
	// dead lifetime — clear it (E). Best-effort with a warning: the pre-create
	// clear in ExecSbxRunAndRecordCreate is the correctness backstop.
	if err := workspace.ClearRemovedReceipt(name); err != nil {
		fmt.Fprintf(os.Stderr, "pix: warning: removed sandbox %q but could not clear its mcp receipt: %v\n", name, err)
	}
	return nil
}

// SandboxPackMarkerPath is <workspace>/.pix/sandbox.pack: the pack root
// this workspace's sandbox was CREATED with (finding G). Written on every
// create (removed when created pack-less), never on a re-attach, so a later
// re-attach compares create-time truth against the CURRENT active pack instead
// of guessing from the active pack alone.
func SandboxPackMarkerPath(ws string) string {
	return filepath.Join(ws, ".pix", "sandbox.pack")
}

// WriteSandboxPackMarker records the pack root a sandbox is being created with
// (or removes the marker when creating pack-less). Best-effort: a failed write
// only costs a future stale-pack reminder, never the launch. Symlink-safe via
// workspace.WriteStateFile (a cloned repo can ship .pix/sandbox.pack as a
// tracked symlink) and workspace.RemoveStateFile (a cloned repo can ship
// .pix ITSELF as a symlink to another repo's .pix, which a plain
// os.Remove would traverse and delete through).
func WriteSandboxPackMarker(ws, packRoot string) {
	if strings.TrimSpace(packRoot) == "" {
		_ = workspace.RemoveStateFile(ws, "sandbox.pack")
		return
	}
	_ = workspace.WriteStateFile(ws, "sandbox.pack", []byte(pack.CanonicalizePackRoot(packRoot)+"\n"), 0o644)
}

// ReadSandboxPackMarker returns the create-time pack root recorded for this
// workspace's sandbox, or "" when no marker exists (a sandbox created before
// markers existed, or created pack-less).
func ReadSandboxPackMarker(ws string) string {
	b, err := os.ReadFile(SandboxPackMarkerPath(ws))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// StalePackReattachWarning returns the "stale pack" reminder `runRun` prints
// when RE-ATTACHING (not creating, not --replace) to a sandbox whose
// CREATE-TIME pack differs from the current active pack (finding G). The
// create-time pack comes from the workspace marker written at create
// (WriteSandboxPackMarker); comparing marker vs active is what makes the
// message honest in BOTH directions:
//   - no false warning when the sandbox already carries the current pack
//     (marker == active pack), and
//   - a warning after `pack rm` (marker set, active empty): the old sandbox
//     still has the removed pack's create-time bin/skills baked in.
//
// No marker => no warning: a sandbox created before markers existed (or
// pack-less) gives us nothing to compare, and guessing from the active pack
// alone is exactly what produced the old false positives.
//
// Deliberately says nothing about MCP: McpReattachWarning (product gap #2)
// owns that claim PRECISELY, via the launcher's own receipt, for every
// desired server regardless of whether a pack changed. Folding a vaguer
// "mcp may be stale" guess in here would duplicate that check and could
// contradict it (e.g. this warning firing on pack drift while the receipt
// proves every MCP server is in fact already attached).
func StalePackReattachWarning(cfg *config.Config, o RunOpts, reattaching bool) string {
	if !reattaching || o.Replace {
		return ""
	}
	created := ReadSandboxPackMarker(o.Workspace)
	if created == "" {
		return ""
	}
	active := ""
	if root := pack.ActivePackRoot(cfg.Pack, o.Pack); root != "" {
		active = pack.CanonicalizePackRoot(root)
	}
	if created == active {
		return ""
	}
	if active == "" {
		return fmt.Sprintf("pix: re-attaching without --replace — this sandbox was created with pack %q (since detached); its bin/skills are still attached until you recreate: %s", created, RunReplaceCommand(o.Workspace))
	}
	return fmt.Sprintf("pix: re-attaching without --replace — this sandbox was created with pack %q, not the active pack %q; the active pack's bin/skills won't attach until you recreate: %s", created, active, RunReplaceCommand(o.Workspace))
}

// desiredMCPUniverse computes the FULL set of MCP server names this
// invocation would preload at CREATE: cfg.MCP, the active/transient pack's
// integration servers (pack.McpNames), and any explicit --mcp, deduped via
// mcp.AllPreloadedMCP. It is the read-only twin of ApplyPackToLaunch's pack-fold
// step (pack.go) used ONLY for this comparison: a re-attach never mounts a
// pack (skills/bin/knowledge are create-time only) and must never trigger
// ApplyPackToLaunch's mount/kit-synthesis side effects just to answer "what
// would this invocation want". A pack that fails to load degrades to
// cfg.MCP+o.MCP alone here (the same as it always did before packs existed)
// rather than blocking a reattach comparison on a broken pack.
func desiredMCPUniverse(cfg *config.Config, o RunOpts) []string {
	names := append([]string(nil), cfg.MCP...)
	if root := pack.ActivePackRoot(cfg.Pack, o.Pack); root != "" {
		if p, err := pack.LoadPack(root); err == nil {
			names = append(names, pack.McpNames(p)...)
		}
	}
	names = append(names, o.MCP...)
	return mcp.AllPreloadedMCP(names)
}

// mcpLoadHints joins one doctor.McpLoadCommand per name (mcp load only ever attaches
// one server at a time, so N missing names need N commands).
func mcpLoadHints(names []string, ws string) string {
	cmds := make([]string, 0, len(names))
	for _, n := range names {
		cmds = append(cmds, doctor.McpLoadCommand(n, ws))
	}
	return strings.Join(cmds, "; ")
}

// McpReattachWarning is `pix run`'s reattach honesty check (product gap
// #2): on a RE-ATTACH (not a create, not --replace) it compares the DESIRED
// MCP universe for THIS invocation (desiredMCPUniverse) against the
// sandbox's own launcher receipt (sandboxmcpstate.go) and warns, BEFORE
// reattaching, about any desired name the receipt cannot PROVE is attached
// (a positive preloaded/loaded claim in a valid receipt is proof, anything
// else is a gap). It never auto-loads, only reports, and always offers
// BOTH exact remediation paths: a live `pix mcp load NAME <workspace>`
// per missing name, or `pix run <workspace> --replace` to recreate with
// the current context. A receipt entry for a name that is no longer desired
// (dropped from config since create) is legitimate history and is never
// mentioned; only desired names are ever checked.
//
// No desired servers at all -> nothing to check, silent. An unresolvable
// state dir, an absent receipt, or an unverifiable one (corrupt / wrong
// schema / wrong sandbox identity) all mean the SAME honest thing for every
// desired name: attachment cannot be verified from here.
func McpReattachWarning(cfg *config.Config, o RunOpts, reattaching bool) string {
	if !reattaching || o.Replace {
		return ""
	}
	desired := desiredMCPUniverse(cfg, o)
	if len(desired) == 0 {
		return ""
	}
	stateDir, err := workspace.MCPStateDirFn()
	if err != nil {
		return fmt.Sprintf("pix: re-attaching without --replace: could not resolve local state (%v), so attachment for %s cannot be verified. Attach live: %s. Or recreate with current context: %s",
			err, strings.Join(desired, ", "), mcpLoadHints(desired, o.Workspace), RunReplaceCommand(o.Workspace))
	}
	receipt, rstatus, _ := workspace.ReadMCPReceipt(stateDir, o.Name)
	if rstatus.Unverifiable() {
		return fmt.Sprintf("pix: re-attaching without --replace: this sandbox's MCP receipt is %s, so attachment for %s cannot be verified. Attach live: %s. Or recreate with current context: %s",
			rstatus.String(), strings.Join(desired, ", "), mcpLoadHints(desired, o.Workspace), RunReplaceCommand(o.Workspace))
	}
	if rstatus == workspace.MCPStateAbsent {
		return fmt.Sprintf("pix: re-attaching without --replace: no MCP receipt for this sandbox, so attachment for %s cannot be verified. Attach live: %s. Or recreate with current context: %s",
			strings.Join(desired, ", "), mcpLoadHints(desired, o.Workspace), RunReplaceCommand(o.Workspace))
	}
	var missing []string
	for _, name := range desired {
		if mcp.ReceiptClaim(receipt, rstatus, name) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return fmt.Sprintf("pix: re-attaching without --replace: %s not proven attached to this sandbox (no receipted preload or load). Attach live: %s. Or recreate with current context: %s",
		strings.Join(missing, ", "), mcpLoadHints(missing, o.Workspace), RunReplaceCommand(o.Workspace))
}

// RunReplaceCommand returns the exact `pix run [WORKSPACE] --replace`
// recovery command to print for workspace, POSIX-shell-safe via
// sys.ShellQuote. Bare "pix run --replace" is only correct for the "."
// default (the sandbox name derives from cwd, so a bare re-run from the SAME
// cwd targets the same sandbox); an EXPLICIT workspace must be echoed back
// verbatim (quoted) — omitting it would target whatever sandbox the CURRENT
// cwd derives, which can be a completely different sandbox than the one that
// just failed to reattach or is carrying a stale pack. Printing the wrong
// recovery command is worse than a slightly longer one.
func RunReplaceCommand(ws string) string {
	if ws == "" || ws == "." {
		return "pix run --replace"
	}
	return "pix run " + sys.ShellQuote(ws) + " --replace"
}

// ParseRunArgs is a small hand-rolled parser (no cobra, no third-party flags) so
// DIR can appear before or after the flags, matching the flexibility of the old
// bin/pix shell launcher. Everything after `--` is pi passthrough.
func ParseRunArgs(argv []string) (RunOpts, error) {
	// -h/--help anywhere before `--` is a help request, not a parse error.
	if cli.WantsHelp(argv) {
		return RunOpts{}, cli.ErrHelpRequested
	}
	o := RunOpts{Workspace: "."}
	wsSet := false

	// Split off the `--` passthrough first.
	pre := argv
	for i, a := range argv {
		if a == "--" {
			pre = argv[:i]
			o.Passthrough = append([]string(nil), argv[i+1:]...)
			break
		}
	}

	valueOf := func(a string, i *int) (string, error) {
		if eq := strings.IndexByte(a, '='); eq >= 0 {
			return a[eq+1:], nil
		}
		if *i+1 >= len(pre) {
			return "", fmt.Errorf("flag %s needs a value", a)
		}
		*i++
		return pre[*i], nil
	}

	for i := 0; i < len(pre); i++ {
		a := pre[i]
		name := a
		if eq := strings.IndexByte(a, '='); eq >= 0 {
			name = a[:eq]
		}
		switch {
		case a == "--dev":
			o.Dev = true
		case a == "--replace":
			o.Replace = true
		case name == "--name":
			v, err := valueOf(a, &i)
			if err != nil {
				return o, err
			}
			o.Name = v
		case name == "--model":
			v, err := valueOf(a, &i)
			if err != nil {
				return o, err
			}
			o.Model = v
		case name == "--intent":
			v, err := valueOf(a, &i)
			if err != nil {
				return o, err
			}
			o.Intent = v
		case name == "--skills":
			v, err := valueOf(a, &i)
			if err != nil {
				return o, err
			}
			o.Skills = append(o.Skills, v)
		case name == "--kit":
			v, err := valueOf(a, &i)
			if err != nil {
				return o, err
			}
			o.Kits = append(o.Kits, v)
		case name == "--kit-ref":
			v, err := valueOf(a, &i)
			if err != nil {
				return o, err
			}
			o.KitRef = normalizeKitRef(v)
		case name == "--template":
			v, err := valueOf(a, &i)
			if err != nil {
				return o, err
			}
			o.Template = v
		case name == "--pack":
			v, err := valueOf(a, &i)
			if err != nil {
				return o, err
			}
			o.Pack = v
		case name == "--mcp":
			v, err := valueOf(a, &i)
			if err != nil {
				return o, err
			}
			o.MCP = append(o.MCP, v)
		case strings.HasPrefix(a, "-"):
			return o, fmt.Errorf("unknown flag %q", a)
		default:
			if wsSet {
				return o, fmt.Errorf("unexpected extra argument %q (only one DIR allowed; use -- for pi args)", a)
			}
			o.Workspace = a
			wsSet = true
		}
	}
	// A non-"." workspace MUST be an existing directory. Otherwise a mistyped verb
	// (`pix run help`, `run doctro`) would silently boot a junk sandbox named
	// after the typo. Reject it, suggesting the verb when the token matches one.
	if err := ValidateRunWorkspace(o.Workspace); err != nil {
		return o, err
	}
	return o, nil
}

// ValidateRunWorkspace verifies a resolved run workspace is launchable: the cwd
// default (".") always is; any other value must name an existing directory. A
// non-directory token that matches a known verb gets a "did you mean" hint.
// DefaultEnv builds the real hostenv.Env. Only the composition root can — it
// alone knows which capability supplies each probe — so it injects this here.
// The default panics rather than returning a half-wired env: a launch that
// silently probes nothing is the failure mode this whole refactor exists to
// delete.
var DefaultEnv = func() hostenv.Env {
	panic("launch: DefaultEnv not wired — the composition root must set it")
}

// IsKnownVerb reports whether a bare positional is actually a mistyped verb,
// so "pix statuss" can suggest "pix status". Only cmd/pix has the verb table,
// so it supplies this; the default answers no, which loses the hint and nothing
// else. Same shape as pack.PackLocalMCP.
var IsKnownVerb = func(string) bool { return false }

func ValidateRunWorkspace(ws string) error {
	err := workspace.Validate(ws)
	var nd workspace.ErrNotDirectory
	if errors.As(err, &nd) && IsKnownVerb(ws) {
		return fmt.Errorf("%q is not a directory. Did you mean `pix %s`?", ws, ws)
	}
	return err
}

// ResolveRepoRoot finds a pix repo checkout for the local kit path, in
// order: $PIX_DEV_ROOT if set, else walking up from the current working
// directory, else the launcher binary's own location (make install symlinks
// ~/.local/bin/pix -> <repo>/out/pix, so the repo is two levels up
// from the resolved binary). Fails when none resolves.
func ResolveRepoRoot() (string, error) {
	if r := strings.TrimSpace(os.Getenv("PIX_DEV_ROOT")); r != "" {
		if isRepoRoot(r) {
			return r, nil
		}
		return "", fmt.Errorf("$PIX_DEV_ROOT=%q is not a pix checkout (no pi-kit/spec.yaml)", r)
	}
	// Walk up from cwd looking for a repo root.
	if cwd, err := os.Getwd(); err == nil {
		dir := cwd
		for {
			if isRepoRoot(dir) {
				return dir, nil
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	// The launcher binary's own location (symlink-resolved).
	if repo, ok := repoFromBinary(); ok {
		return repo, nil
	}
	return "", fmt.Errorf("no pix checkout found (set $PIX_DEV_ROOT or run from inside a checkout)")
}

// repoFromBinary resolves the launcher binary (following symlinks) and reports
// the repo root two levels up (<repo>/out/pix -> <repo>) when it looks
// like a checkout.
func repoFromBinary() (string, bool) {
	self, err := os.Executable()
	if err != nil {
		return "", false
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	repo := filepath.Dir(filepath.Dir(self))
	if isRepoRoot(repo) {
		return repo, true
	}
	return "", false
}

// LocalImageLoaded reports whether sbx's template store carries the local image
// tag. Used to refuse a launch that would otherwise make sbx PULL a
// never-published local-* image (the confusing "pull? use cached?" prompt/stall).
//
// It matches the tag as a SUBSTRING anywhere in `sbx template ls` output, which
// is both format-independent (works for `repo tag id`, a combined `repo:tag id`,
// headers, warnings) and catches the fully-pruned case (no matching line at all
// -> not loaded -> refuse). The tag is a unique local-<unixts>, so a substring
// match can't collide with anything else. It fails OPEN (returns true) only when
// there's NO signal to judge from: no sbx, an ls error, or empty output.
func LocalImageLoaded(env hostenv.Env, tag string) bool {
	if tag == "" || false {
		return true
	}
	if _, err := env.LookPath("sbx"); err != nil {
		return true
	}
	// BOUNDED (probeRun): a hung `sbx template ls` is a timeout, which is the
	// same "no signal" as an error — fail open, never wedge the launch.
	out, timedOut, err := env.RunTimed("sbx", "template", "ls")
	if timedOut || err != nil || strings.TrimSpace(out) == "" {
		return true // no signal -> don't block
	}
	return strings.Contains(out, tag)
}

// ReadLocalImageTag returns the trimmed contents of <root>/out/.local-image-tag
// (written by `make load`), or "" when absent — in which case the caller skips
// the --template pin.
func ReadLocalImageTag(root string) string {
	b, err := os.ReadFile(filepath.Join(root, "out", ".local-image-tag"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// isRepoRoot reports whether dir looks like a pix repo checkout.
func isRepoRoot(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "pi-kit", "spec.yaml"))
	return err == nil
}

// WriteOllamaBridgeFile writes <workspace>/.pix/ollama-bridge.model: the
// local model tag the in-VM ollama-bridge should expose (interactive cycle + the
// router's local option). Configured on the host with `pix config set
// ollama_bridge_model`; the bridge reads it (env var still overrides). Per-run,
// gitignored, best-effort — an absent file just means the bridge uses its default.
// Symlink-safe via workspace.WriteStateFile.
func WriteOllamaBridgeFile(ws, model string) {
	model = strings.TrimSpace(model)
	if model == "" {
		model = config.DefaultOllamaBridgeModel
	}
	_ = workspace.WriteStateFile(ws, "ollama-bridge.model", []byte(model+"\n"), 0o644)
}

// ToStringSlice coerces a decoded JSON array (any of []any / []string) to
// []string, dropping non-strings.
func ToStringSlice(v any) []string {
	switch xs := v.(type) {
	case []string:
		return xs
	case []any:
		out := make([]string, 0, len(xs))
		for _, x := range xs {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// The tri-state sandbox probe (running/stopped/absent/unknown) that drives the
// create-vs-reattach-vs-replace decision lives in task.go as ProbeTaskSandbox +
// SbxState (an alias for the canonical sandbox.State) — run.go reuses it
// rather than duplicating the `sbx ls` parse.

func PrintJSONLauncher(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}

// GeneratedInputMarker prefixes any user-role message that `pix` itself
// synthesizes and hands to the agent as if typed by the user (currently just
// onboardingKickoff). It is NOT the user talking, so extensions that observe
// user turns (memory-capture.ts) must recognize it and skip capture — without
// this, the watcher model treats the kickoff line as a real user statement and
// invents facts/events from it (the bug this constant fixes). Keep this string
// and extensions/memory-capture.ts's prefix check in sync.
//
// The marker is NOT user-visible: the kickoff travels as pi's initial CLI
// prompt argument, and observed session transcripts do not render that
// initial prompt as a chat message, so the bracketed prefix never shows up
// in the UI. No stripping/beautifying is needed for display.
const GeneratedInputMarker = "[pix-generated:onboarding] "
