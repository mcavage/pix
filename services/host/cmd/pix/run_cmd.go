// run_cmd.go — `pix run` as a typed root child. The two things the launch
// package deliberately does not know — the verb table (for the "did you mean"
// hint) and the real env — are passed to it from here at each call, rather than
// installed into package vars by an init().
//
// One shape the grammar cannot express stays in FRONT of the parser: the `--` pi
// passthrough. kong consumes `--` as an end-of-flags marker and would feed the
// first pi arg to the DIR positional, so the tail is rewritten into repeated
// `--pi-arg=` values (rewriteRunPassthrough) — an argv REWRITE, not a parser.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/health"
	"pix/host/inference"
	"pix/host/launcher"
	"pix/host/mcp"
	"pix/host/packinfo"
	"pix/host/sandbox"
	"pix/host/secret"
	"pix/host/service"
	"pix/host/uat"
	"pix/host/workflow/doctor"
	nativeenv "pix/host/workflow/env"
	"pix/host/workflow/launch"
	"pix/host/workflow/pack"
	"pix/host/workflow/provision"
	"pix/host/workspace"
)

// knownVerb is the verb table launch asks about to turn `pix run doctro` into
// a suggestion. L4 owns the table, so L4 hands it over per call.
func knownVerb(v string) bool { return knownVerbs()[v] }

// runDescription is run's long help: the lifecycle and released-vs-local rules
// generated usage cannot infer. The flag list is NOT here — the fields are.
const runDescription = `Everything after -- is passed to pi. Set PIX_DEBUG=1 to print the composed sbx
command.

lifecycle (matches sbx's own re-attach model):
  no sandbox named N          -> create it (the flags below apply).
  a sandbox named N exists    -> ATTACH to it as-is (running or stopped); sbx
                                 reads the agent from its own spec, so
                                 --kit/--mcp/--template and create-only skill
                                 flags are NOT re-sent. --dev attaches only when
                                 the sandbox has its create-time session UAT
                                 record; otherwise it refuses with the recreate
                                 command, because static MCP cannot be added
                                 later. --model/--intent are NOT create-only:
                                 they are pi runtime args, so they still reach
                                 the pi session on an attach too.
                                 An attach whose create-time MCP set or image
                                 no longer matches is REFUSED, not silently
                                 attached; to recreate, remove it first:
                                 pix rm <box> && pix run.

  the last shell to leave a sandbox tears it down (pix run -k keeps it).

released vs local:
  A RELEASED launcher (a clean version like 0.0.16) tracks the LATEST STABLE
  release: it resolves the newest published tag (cached 24h) and pins that, so
  a launcher installed months ago still boots today's kit + image. If that
  lookup cannot run — offline, GitHub unreachable — it falls back to this
  build's own version; a run is never blocked by it. Precedence: --kit-ref,
  version_pin in config.toml, latest stable, this build's version.

  An UNRELEASED/local build never pins a nonexistent v<version> tag: it uses
  your local checkout kit when one is resolvable (pinning the locally loaded
  image via --template), else falls back to #ref=main with a warning.`

func (c *runCmd) Help() string { return runDescription }

// runCmd is the launcher's main verb.
type runCmd struct {
	Dir string `arg:"" optional:"" default:"." help:"Workspace to launch in (default: the current directory)."`

	Dev      bool     `help:"Mode B: use the local checkout kit + load skills live from it (needs a checkout)."`
	Skills   []string `help:"Mount an extra skill tree and load it live (repeatable)." placeholder:"DIR"`
	Kit      []string `help:"Override the KIT (image + entrypoint + creds + egress + skills) instead of the auto git/local pin (repeatable; path or git+URL)." placeholder:"K"`
	KitRef   string   `help:"Pin the auto kit to a git ref (v0.1.0, main) instead of the latest stable release." placeholder:"REF"`
	Template string   `help:"Override only the IMAGE sbx boots (the ref 'make load' prints). Orthogonal to --kit." placeholder:"REF"`
	Mcp      []string `help:"Attach an MCP server at creation (repeatable)." placeholder:"M"`
	Pack     string   `help:"Active pack for this run (path or git-url); overrides the configured one." placeholder:"P"`
	Name     string   `help:"Sandbox name." placeholder:"N"`
	Model    string   `help:"Active pi model (passed through to pi)." placeholder:"M"`
	Intent   string   `help:"Resolve the session model via the router; --model overrides it. Intents: pix models show." placeholder:"NAME"`
	Task     string   `help:"Launch an existing task's sandbox (same as 'pix task run NAME')." placeholder:"NAME"`
	Keep     bool     `short:"k" help:"Keep the sandbox when the last shell exits: a sticky, identity-bound marker the teardown/orphan reaper refuses on (an explicit 'pix rm' still removes it)."`

	// PiArg is the `--` tail, rewritten by rewriteRunPassthrough. Hidden because a
	// user never types it: they type `-- <pi args>`, documented above.
	PiArg []string `name:"pi-arg" hidden:""`
}

// rewriteRunPassthrough turns `run ... -- a b` into `run ... --pi-arg=a
// --pi-arg=b`. The `=value` form is deliberate: a pi arg starting with `-` can
// never be re-read as a flag of ours. Everything before the first `--` is kept.
func rewriteRunPassthrough(argv []string) []string {
	for i, a := range argv {
		if a != "--" {
			continue
		}
		out := append([]string(nil), argv[:i]...)
		for _, t := range argv[i+1:] {
			out = append(out, "--pi-arg="+t)
		}
		return out
	}
	return argv
}

// opts turns the parsed command into the launch options, resolving the two
// inputs that are not literal flag values: `--task NAME` and the workspace.
func (c *runCmd) opts() (launch.RunOpts, error) {
	o := launch.RunOpts{
		Workspace:   c.Dir,
		Dev:         c.Dev,
		Skills:      c.Skills,
		Kits:        c.Kit,
		Template:    c.Template,
		MCP:         c.Mcp,
		Name:        c.Name,
		Model:       c.Model,
		Intent:      c.Intent,
		Pack:        c.Pack,
		Passthrough: c.PiArg,
		Keep:        c.Keep,
	}
	if c.KitRef != "" {
		o.KitRef = launch.NormalizeKitRef(c.KitRef)
	}
	// `--task NAME` is the `pix task run NAME` shorthand: task.Resolve does the work
	// and this fills the ordinary DIR + --name shape, so no sandbox-lifecycle code is
	// duplicated here. An explicit --name still wins.
	if c.Task != "" {
		dir, sandboxName, err := resolveTaskTarget(c.Task)
		if err != nil {
			return o, err
		}
		o.Workspace = dir
		if o.Name == "" {
			o.Name = sandboxName
		}
	}
	// A non-"." workspace MUST exist and be a directory, or a mistyped verb
	// (`pix run doctro`) silently boots a junk sandbox named after the typo.
	if err := launch.ValidateRunWorkspace(o.Workspace, knownVerb); err != nil {
		return o, cli.UsageError{Err: err}
	}
	return o, nil
}

func (c *runCmd) Run(d *cli.Deps) error {
	o, err := c.opts()
	if err != nil {
		return err
	}
	return runLaunch(d, o)
}

// composeStaticMCP folds this launch's already-resolved StaticMCP — e.g. a pack
// contribution ApplyPackContribution folded in earlier — together with the
// configured (cfg.MCP) and flag-requested (o.MCP) servers into the final
// create-time preloaded set. It reads all three inputs but never mutates or
// aliases any of their backing arrays: the result is always a freshly made
// slice, so a caller's cfg or o is safe to keep using afterward.
func composeStaticMCP(existing, cfgMCP, oMCP []string) []string {
	merged := make([]string, 0, len(existing)+len(cfgMCP)+len(oMCP))
	merged = append(merged, existing...)
	merged = append(merged, cfgMCP...)
	merged = append(merged, oMCP...)
	return mcp.AllPreloadedMCP(merged)
}

// resolveSandboxName is the sandbox `pix run` actually targets: an explicit
// --name travels verbatim (a user-owned display name, never reinterpreted).
// Absent that, the DEFAULT is the deterministic, digest-suffixed
// sandbox.Name(workspace) — and lease state is keyed by whichever of those two
// this function returns (see sessionKey below), so two workspaces sharing a
// basename can never alias one sandbox or one lease directory in the default
// case (the digest covers the full canonical path), and an explicit --name
// reusing a workspace directory another sandbox already used never aliases
// that sandbox's lease directory either.
func resolveSandboxName(explicit, workspace string) string {
	if explicit != "" {
		return explicit
	}
	return sandbox.Name(workspace)
}

// sessionKeyFor is the lease identity for a resolved run: the FINAL sandbox
// name (o.Name, already run through resolveSandboxName by the caller), never
// the raw workspace path. Extracted as its own function so the collision this
// guards against is unit-testable without exercising the rest of runCmd.Run:
// two DIFFERENT explicit --name runs sharing one workspace directory (the
// exact shape section [3]/[4] of the host UAT script exercises, reusing a
// workspace across sandboxes) must never resolve to the same lease directory.
func sessionKeyFor(o launch.RunOpts) string { return o.Name }

// runFail reports a launch failure in run's own words and hands the root the
// exit code to use. The message is already complete, so it travels as a
// SilentError rather than being re-prefixed by the root's renderer.
func uatSmokeSkipsProviderKeyGate() bool {
	return os.Getenv("PIX_UAT_SMOKE") == "1"
}

func runFail(d *cli.Deps, code int, format string, a ...any) error {
	fmt.Fprintf(d.Err, "pix run: "+format+"\n", a...)
	return cli.SilentError{Code: code}
}

// gateSbxVersion refuses `pix run` before ANY sandbox side effect when the
// installed sbx cannot do what native environments will require: older than
// health.SbxMinVersion, or so unparsable a version cannot be read from it at
// all (docs/design/environments.md section 5.6, AC-20). bin is the SbxProbe
// seam a test points at a fixture; production always passes the real "sbx"
// resolved off PATH.
//
// Missing, denied, or otherwise unreachable sbx is a DIFFERENT, already
// honest gap — unloadedLocalImage and the "exec sbx" failure further down
// this file both name doctor.SbxInstallHint for it — so health.SbxVersionGate
// deliberately fires ONLY on a positive read; this gate never turns "could
// not check" into a version refusal.
func gateSbxVersion(d *cli.Deps, bin string) error {
	ctx, cancel := context.WithTimeout(context.Background(), health.StatusBudget)
	defer cancel()
	r := health.SbxProbe{Bin: bin}.Check(ctx)
	if blocked, found := health.SbxVersionGate(r); blocked {
		fmt.Fprint(d.Err, health.SbxVersionGateMessage(found))
		return cli.SilentError{Code: 1}
	}
	return nil
}

// unloadedLocalImage is the refusal both local-image preflight arms share: what
// was pinned, why sbx would stall on it, and the one command that fixes it.
func unloadedLocalImage(d *cli.Deps, what string) error {
	fmt.Fprintf(d.Err, "pix: %s is not loaded in sbx.\n", what)
	fmt.Fprintln(d.Err, "It's a local build (never published), so sbx would try to pull it and stall on a prompt.")
	fmt.Fprintln(d.Err, "Load it into sbx first, from the checkout that built it:  make load")
	return cli.SilentError{Code: 1}
}

// runLaunch reads the config, resolves the run options (including a repo checkout
// for --dev), composes the sbx argv, and execs it with stdio inherited. It
// forwards NO credential bearer into the sandbox: host MCP servers authenticate on
// the host, so there is nothing to inject.
func runLaunch(d *cli.Deps, o launch.RunOpts) (err error) {
	// Fail closed before anything else: an sbx that cannot run native
	// environments must not get far enough to attempt a create or attach.
	if verr := gateSbxVersion(d, "sbx"); verr != nil {
		return verr
	}
	var generatedKitDirs []string
	defer func() {
		if cerr := launch.CleanupGeneratedKitDirs(generatedKitDirs); cerr != nil {
			fmt.Fprintf(d.Err, "pix: warning: %v\n", cerr)
		}
	}()

	// Default the session intent from config (run_intent, the "overlord") when the
	// user pinned neither --model nor --intent.
	if o.Intent == "" && o.Model == "" {
		if cfg, cerr := config.Load(); cerr == nil {
			if applied, rerr := launch.ApplyConfiguredSessionModel(&o, cfg); rerr != nil {
				// A bad config value must not brick launch, but it must be loud. The
				// explicit --intent path below remains a hard usage error.
				fmt.Fprintf(d.Err, "pix: run_intent %q did not resolve (%v); using pi's default model. Fix with `pix config set run_intent <intent>`.\n", strings.TrimSpace(cfg.RunIntent), rerr)
			} else if applied && o.Model != "" {
				fmt.Fprintf(d.Err, "pix: intent %q -> model %s\n", o.Intent, o.Model)
			}
		}
	}

	// "none"/"off" is the documented opt-out (for both --intent and run_intent):
	// pi's own default model, no router.
	if o.Model == "" && (strings.EqualFold(o.Intent, "none") || strings.EqualFold(o.Intent, "off")) {
		o.Intent = ""
	}

	// Resolve --intent through the router (--model wins), so the INTERACTIVE
	// session routes the same way the subagent crew does.
	if o.Intent != "" && o.Model == "" {
		m, rerr := inference.ResolveSessionModel(o.Intent)
		if rerr != nil {
			return runFail(d, 2, "--intent %q: %v", o.Intent, rerr)
		}
		o.Model = m
		fmt.Fprintf(d.Err, "pix: intent %q -> model %s\n", o.Intent, m)
	}

	// A pi session needs at least one provider key: resolve the 1Password refs into
	// sbx (a no-op when a key is there), then refuse ONLY on a POSITIVE "no key"
	// answer — unprobeable means cannot verify, which proceeds. keyResult is kept for
	// the readiness snapshot, so run pays for one `sbx secret ls`.
	var keyResult health.Result
	if _, lerr := defaultShellEnv().LookPath("sbx"); lerr == nil && !inference.ConfiguredKeylessInference() && !uatSmokeSkipsProviderKeyGate() {
		env := defaultShellEnv()
		launch.BootstrapProviderKeys(env, d.In, d.Err, d.Interactive)
		keyResult = launch.ProbeModelKeys(context.Background(), "")
		if launch.RefusesLaunch(keyResult) {
			fmt.Fprint(d.Err, secret.ModelKeyMissingMessage(env))
			return cli.SilentError{Code: 1}
		}
	}

	// Reconcile a prior in-session onboarding proposal (<workspace>/.pix/
	// onboarding.json) BEFORE LoadResolvedConfig so a fresh create picks it up.
	// Best-effort; a non-TTY just leaves the file.
	provision.ReconcileOnboarding(o.Workspace, defaultShellEnv(), d.In, d.Out, false, d.Interactive)

	// Load the config for the rest of run (kits, mcp, gog, pack).
	cfg, _, err := workspace.LoadResolvedConfig()
	if err != nil {
		return runFail(d, 1, "%v", err)
	}
	// D13/AC-59: the one quiet, negative-first nudge about an unregistered
	// workspace `.sbxenv.yaml` — read-only, no prompt, no config mutation, at
	// most once per canonical workspace ever (workflow/env.RunHint owns every
	// suppression rule: a registered environment anywhere, an already-shown
	// durable marker, or no file at all all resolve to "").
	if hint := nativeenv.RunHint(cfg, o.Workspace); hint != "" {
		fmt.Fprint(d.Err, hint)
	}
	if !inference.AllowsModel(cfg, o.Model) {
		return runFail(d, 2, "model %q is not available through the configured inference backends", o.Model)
	}

	// Own the sandbox name (sbx would auto-derive `pix-<dir>`) and probe its state
	// BEFORE any create-only resolution below, so a plain re-attach never fails on a
	// --dev/checkout or --kit problem it does not need.
	o.Name = resolveSandboxName(o.Name, o.Workspace)
	state := launch.ProbeTaskSandbox(defaultShellEnv(), o.Name)

	// Mirror sbx's own model: an existing sandbox (running OR stopped) is ATTACHED
	// to rather than recreated, so NOTHING in the create-only block below — kit
	// selection, the pack stack, the local-image preflight, the MCP set — is even
	// RESOLVED on an attach. ONE predicate answers it, for this gate and the plan.
	creating := launch.WillCreate(state)
	// effectivePack is the pack that ACTUALLY loaded, which keeps the sandbox.pack
	// marker and the memory scope from disagreeing. --pack applies at create only,
	// since a re-attach keeps what it was made with.
	effectivePack := packinfo.ActivePackRoot(cfg.Pack, o.Pack)
	if creating {
		// Kit selection. A CLEAN released version pins the matching git tag; anything
		// else (unstamped "dev", "0.0.16+local", non-semver) is UNRELEASED and its tag
		// does not exist, so v<version> is never pinned. --dev forces the local
		// checkout kit; an unreleased build uses it too when a checkout resolves, else
		// falls back to #ref=main.
		released := launcher.IsReleased(version)
		kitOverride := len(o.Kits) > 0

		// Released launchers pin kit + image to their stamped version; only
		// --kit-ref and version_pin move that pin.
		if !o.Dev && !kitOverride {
			ref, src := launch.ResolveKitRef(version, o.KitRef, cfg.VersionPin)
			o.KitRef = ref
			if msg := launch.KitRefNotice(version, ref, src); msg != "" {
				fmt.Fprintln(d.Err, msg)
			}
		}

		if o.Dev {
			// --dev needs a resolvable repo checkout; fail loud otherwise.
			root, rerr := launch.ResolveRepoRoot()
			if rerr != nil {
				return runFail(d, 1, "--dev: %v", rerr)
			}
			o.DevRoot = root
			o.LocalKit = filepath.Join(root, "pi-kit")
			o.LocalImageTag = launch.ReadLocalImageTag(root)
		} else if !released && !kitOverride {
			if root, rerr := launch.ResolveRepoRoot(); rerr == nil {
				o.LocalKit = filepath.Join(root, "pi-kit")
				o.LocalImageTag = launch.ReadLocalImageTag(root)
				note := ""
				if o.LocalImageTag != "" {
					note = " (local image :" + o.LocalImageTag + ")"
				}
				fmt.Fprintf(d.Err, "pix: unreleased build %q — using local checkout kit %s%s\n", version, o.LocalKit, note)
			} else {
				fmt.Fprintf(d.Err, "pix: unreleased build %q and no pix checkout found — "+
					"kit tracks #ref=main (may not match this binary). Use `pix run --dev` from a "+
					"checkout or `pix run --kit <path-or-git-url>` to override.\n", version)
			}
		}

		// The active pack's skills/ + knowledge/ mount into this sandbox: workflow/pack
		// verifies the trust surface and fails closed; launch folds the verified value in.
		contributed, perr := pack.ResolveLaunchContribution(cfg, o.Pack, defaultShellEnv(), d.Err)
		if perr != nil {
			return runFail(d, 1, "%v", perr)
		}
		effectivePack = o.ApplyPackContribution(contributed)
		// Inference is a generated create-time facet like pack wrappers: probed models,
		// compiled routes, public endpoint metadata. No credential value enters it.
		inferenceKit, ierr := inference.SynthesizeInferenceKit(cfg)
		if ierr != nil {
			return runFail(d, 1, "inference: %v", ierr)
		}
		if inferenceKit != "" {
			o.PackKits = append(o.PackKits, inferenceKit)
			generatedKitDirs = append(generatedKitDirs, inferenceKit)
		}
		o.Models, ierr = inference.CallableRuntimeModels(cfg)
		if ierr != nil {
			return runFail(d, 1, "inference models: %v", ierr)
		}
		// Create the personal context tree before the mount list is built, so a
		// first-ever run mounts it and a session can write its first skill (and
		// its AGENTS.md) without going back to the host first.
		launch.EnsurePersonalContextDir()
		contextKit, cerr := launch.SynthesizePersonalContextKit()
		if cerr != nil {
			return runFail(d, 1, "personal context: %v", cerr)
		}
		if contextKit != "" {
			o.PackKits = append(o.PackKits, contextKit)
			generatedKitDirs = append(generatedKitDirs, contextKit)
		}

		// Local-image preflight: a local-* tag is NEVER published, so pinning it when
		// sbx has not loaded it makes sbx try to pull and stall on an interactive
		// prompt. Refuse fast with the real fix instead.
		switch {
		case o.Template != "":
			if tag := launch.TemplateTag(o.Template); strings.HasPrefix(tag, "local-") && !launch.LocalImageLoaded(defaultShellEnv(), tag) {
				return unloadedLocalImage(d, "--template "+o.Template)
			}
		case o.LocalImageTag != "" && o.LocalKit != "" && len(o.Kits) == 0:
			if !launch.LocalImageLoaded(defaultShellEnv(), o.LocalImageTag) {
				return unloadedLocalImage(d, "local image "+launch.DockerImageRepo+":"+o.LocalImageTag)
			}
		}

		// Every configured MCP server attaches at create (--static-mcp), on top of
		// whatever a verified pack already contributed above (ApplyPackContribution) —
		// composeStaticMCP folds all three in without discarding or aliasing any of them.
		o.StaticMCP = composeStaticMCP(o.StaticMCP, cfg.MCP, o.MCP)
	}

	var uatRec *uat.Registration
	if creating && o.Dev {
		id, err := uat.GenerateSessionID()
		if err != nil {
			return err
		}
		// Make MCPName strictly pix-uat-ID to avoid length issues if o.Name is long
		uatRec = &uat.Registration{
			SessionID: id,
			MCPName:   "pix-uat-" + id,
		}
		o.StaticMCP = composeStaticMCP(o.StaticMCP, nil, []string{uatRec.MCPName})
	} else {
		// Attach reconstructs without mutation. An explicit --dev request is a
		// hard requirement, not a hint: without this create-time record the
		// sandbox cannot have the ephemeral static MCP, and attaching anyway is
		// exactly how a session claimed dev mode while uat_capabilities was absent.
		rec, err := uat.ResolveAttachRegistration(defaultShellEnv(), o.Name, o.Dev)
		if err != nil {
			return runFail(d, 1, "%v", err)
		}
		if rec != nil {
			o.StaticMCP = composeStaticMCP(o.StaticMCP, nil, []string{rec.MCPName})
			uatRec = rec
			if o.Dev {
				// A kept sandbox reattached after its original launcher exited may
				// have no live uat-worker at all (the owner stopped it on its own
				// exit) or a stale one; EnsureWorker dials first so a worker that
				// IS still live (another concurrent attach) is adopted, never
				// unlinked or replaced. Must happen before session attach below.
				repoRoot, rerr := launch.ResolveRepoRoot()
				if rerr != nil {
					return runFail(d, 1, "--dev attach: %v", rerr)
				}
				uatState, _ := defaultShellEnv().StateDir()
				if werr := ensureUatWorkerOrFail(defaultShellEnv(), repoRoot, filepath.Join(uatState, "uat"), rec); werr != nil {
					return runFail(d, 1, "failed to start UAT worker: %v", werr)
				}
			}
		}
	}

	plan := launch.PlanSandboxLaunch(state, cfg, o, version)
	if plan.Err != nil {
		// Fail closed BEFORE any output claims a create or attach is happening,
		// and before exec (SbxUnknown).
		return runFail(d, 1, "%v", plan.Err)
	}
	// sessionKey is the lease identity for THIS sandbox: the FINAL resolved name
	// (o.Name, already resolved above by resolveSandboxName), never the workspace
	// path directly. Keying by workspace instead would alias two DIFFERENT named
	// sandboxes onto one lease directory the moment they share a workspace (e.g.
	// an explicit --name reusing a directory pix already digest-named for another
	// sandbox) — keying by the name sbx itself treats as the unique identity avoids
	// that, and still lands on the same digest form in the default (unnamed) case,
	// since resolveSandboxName's default IS sandbox.Name(workspace). fp is what a
	// later attach must match. The exec-attach DECISION is probed here read-only
	// and re-validated under the lifecycle lock by launch.RunSession, which is
	// also where a fingerprint divergence refuses.
	sessionKey := sessionKeyFor(o)
	fp := launch.SessionFingerprint(cfg, o)
	attachExec := false
	if plan.Reattach {
		_, attachExec = launch.FindPositivelyIdentifiedRunning(defaultShellEnv(), o.Name)
	} else if verr := launch.ValidateCreateKits(plan.Args, launch.ValidateSbxKit); verr != nil {
		return runFail(d, 1, "%v", verr)
	}
	// What this launch is DOING, in one line. Nothing here warns about create-only
	// drift (a stale pack, a changed MCP set): the recorded create-time FINGERPRINT
	// decides that under the lifecycle lock, and a divergent attach is REFUSED with
	// RecreateGuidance rather than attached with a warning nobody can verify.
	switch {
	case plan.Reattach && state == launch.SbxRunning:
		fmt.Fprintf(d.Err, "pix run: attaching to running sandbox %q\n", o.Name)
	case plan.Reattach:
		fmt.Fprintf(d.Err, "pix run: starting + attaching existing sandbox %q\n", o.Name)
	}
	// Lazy auto-start of the configured host services under ONE short deadline
	// (spawn lock + health poll). The launch proceeds regardless; recall degrades
	// in-VM. service.Ensure prints its own lines.
	service.EnsureUp(d.Err, nil, service.EnsureRunTimeout)

	// Readiness, reusing the key evidence the launch gate already paid for. AT
	// MOST launch.WarningLimit rows, and it NEVER blocks: the missing provider
	// key handled above is the only launch-stopping gap.
	launch.RenderWarnings(d.Err, launch.FastSnapshot(context.Background(), cfg, keyResult), launch.WarningLimit)

	// Local model + memory scope for the in-VM ollama-bridge and recall/capture
	// extensions. Best-effort: an unloadable pack degrades to unscoped.
	launch.WritePackContextFiles(cfg, o, effectivePack, d.Err)

	// Trusted host state travels ONLY inside the launcher-generated initial prompt,
	// never as a workspace file a cloned repo could plant. --pack applies on CREATE
	// only, so a re-attach must not claim it.
	packForState := ""
	if creating {
		packForState = o.Pack
	}
	// A HARD contract: a generated prompt is the fenced agent's ONLY trusted host
	// truth, so a launch that cannot build it ABORTS before exec.
	args, perr := launch.InjectTrustedHostState(plan.Args, cfg, defaultShellEnv(), packForState)
	if perr != nil {
		return runFail(d, 1, "could not build trusted host state: %v", perr)
	}

	if creating && o.Dev && uatRec != nil {
		if err := uat.WriteRegistration(defaultShellEnv(), o.Name, uatRec); err != nil {
			return runFail(d, 1, "failed to record UAT session: %v", err)
		}
		uatState, _ := defaultShellEnv().StateDir()
		if err := uat.RegisterMCP(defaultShellEnv(), uatRec, o.DevRoot, filepath.Join(uatState, "uat")); err != nil {
			_ = uat.DeleteRegistration(defaultShellEnv(), o.Name)
			return runFail(d, 1, "failed to register UAT MCP: %v", err)
		}
		// Started AFTER the secure per-session runner state directory exists
		// (RegisterMCP just created it) and BEFORE anything execs sbx below, so
		// the gateway relay can never need it before it is listening.
		if werr := ensureUatWorkerOrFail(defaultShellEnv(), o.DevRoot, filepath.Join(uatState, "uat"), uatRec); werr != nil {
			_ = uat.UnregisterMCP(defaultShellEnv(), uatRec.MCPName)
			_ = uat.DeleteRegistration(defaultShellEnv(), o.Name)
			return runFail(d, 1, "failed to start UAT worker: %v", werr)
		}
	}

	launched := false
	defer func() {
		if !launched && creating && o.Dev && uatRec != nil {
			_ = uat.UnregisterMCP(defaultShellEnv(), uatRec.MCPName)
			_ = uat.DeleteRegistration(defaultShellEnv(), o.Name)
		}
	}()

	if os.Getenv("PIX_DEBUG") != "" {
		fmt.Fprintln(d.Err, "+ sbx "+strings.Join(args, " "))
	}

	// launch.RunSession OWNS the create/attach ordering: lifecycle lock EX, a fresh
	// probe under it, the child started, the create-time facts recorded, the refs
	// SHARED reference taken while lifecycle is still held, lifecycle released, and
	// only THEN the session waited out. This layer owns stdio wiring, the exit code
	// and the words, never the ordering. ONE invocation builder serves both the argv
	// this create records and the default an attach falls back to, so they cannot
	// drift.
	invocation := launch.BuildPiInvocation(launch.LiveSkillDirs(cfg, o), o)
	spec := launch.SessionSpec{
		Key:               sessionKey,
		Name:              o.Name,
		Workspace:         o.Workspace,
		Creating:          creating,
		Keep:              o.Keep,
		CreateArgs:        args,
		AttachTTY:         d.Interactive,
		AttachExec:        attachExec,
		Fingerprint:       fp,
		Invocation:        invocation,
		DefaultInvocation: invocation,
	}
	deps := launch.SessionDeps{
		Env:  defaultShellEnv(),
		Poll: launch.SbxCreatePoll(defaultShellEnv()),
		Warn: d.Err,
		Spawn: func(argv []string) *exec.Cmd {
			cmd := exec.Command("sbx", argv...)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			// No credential bearer: host MCP servers authenticate on the host, so the
			// sandbox never sees a token.
			cmd.Env = os.Environ()
			return cmd
		},
	}
	if xerr := launch.RunSession(spec, deps); xerr != nil {
		if creating && o.Dev && uatRec != nil {
			_ = uat.UnregisterMCP(defaultShellEnv(), uatRec.MCPName)
			_ = uat.DeleteRegistration(defaultShellEnv(), o.Name)
		}
		var refused *launch.SessionRefused
		if errors.As(xerr, &refused) {
			// Decided under the lifecycle lock, before anything started: no create, no
			// attach, no removal. run's own complete message.
			return runFail(d, 1, "%v", refused)
		}
		code := 1
		var exitErr *exec.ExitError
		if errors.As(xerr, &exitErr) {
			code = exitErr.ExitCode()
			// A pinned git #ref kit that sbx could not resolve fails with an opaque
			// git 128; replace it with an actionable note.
			if msg := launch.KitResolveFailureMsg(launch.PinnedGitKit(args)); msg != "" {
				fmt.Fprintln(d.Err, msg)
			}
		} else {
			fmt.Fprintf(d.Err, "pix run: exec sbx: %v\n", xerr)
			if errors.Is(xerr, exec.ErrNotFound) {
				fmt.Fprintln(d.Err, "install sbx with: "+doctor.SbxInstallHint)
			}
		}
		// A re-attach can fail on an sbx that won't reattach a kit-created sandbox;
		// never leave the user without a next step.
		if plan.Reattach {
			fmt.Fprintf(d.Err, "pix run: attach failed; %s\n", launch.RecreateGuidance(o.Name))
		}
		return cli.SilentError{Code: code}
	}
	// RunSession now owns the sandbox lifecycle. A normal last-shell teardown
	// already removed the registration; a kept sandbox must retain it for the
	// next attachment. The fallback defer is only for failures before handoff.
	launched = true
	return nil
}
