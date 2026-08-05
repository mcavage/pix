// run_cmd.go — `pix run` as a typed root child, plus the two things the launch
// package deliberately does not know: the verb table (for the "did you mean"
// hint) and the real env.
//
// The flags are struct fields, so the usage a user reads is generated from the
// declaration that parses them. One shape the grammar cannot express stays in
// front of the parser: the `--` pi passthrough. kong consumes `--` as an
// end-of-flags marker and would then feed the first pi arg to the DIR
// positional, so the root rewrites the tail into repeated `--pi-arg=` values
// (rewriteRunPassthrough) before parsing — an argv REWRITE, exactly like the
// `task NAME path` one, not a second parser.
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
	"pix/host/secret"
	"pix/host/service"
	"pix/host/workflow/doctor"
	"pix/host/workflow/launch"
	"pix/host/workflow/onboard"
	"pix/host/workflow/pack"
	"pix/host/workspace"
)

func init() {
	launch.IsKnownVerb = func(v string) bool { return knownVerbs()[v] }
	launch.DefaultEnv = defaultShellEnv
}

// runDescription is run's long help: the lifecycle and released-vs-local
// rules, which generated usage cannot infer from a struct tag. The flag list
// is NOT here — the fields below are the flag list.
const runDescription = `Everything after -- is passed to pi. Set PIX_DEBUG=1 to print the composed sbx
command.

lifecycle (matches sbx's own re-attach model):
  no sandbox named N          -> create it (the flags below apply).
  a sandbox named N exists    -> RE-ATTACH to it as-is (running or stopped);
                                 sbx reads the agent from its own spec, so
                                 --kit/--mcp/--template, --dev, and the
                                 create-only skill flags are NOT re-sent (--dev
                                 is create/replace-only and is ignored, with a
                                 note, on a plain re-attach). Use --replace to
                                 recreate with the current flags instead.
                                 --model/--intent are NOT create-only: they are
                                 pi runtime args, so they still reach the pi
                                 session on a re-attach too.

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
	Kit      []string `help:"Override the KIT — image + entrypoint + creds + egress + skills — instead of the auto git/local pin (repeatable; path or git+URL)." placeholder:"K"`
	KitRef   string   `help:"Pin the auto kit to a git ref (v0.1.0, main) instead of the latest stable release." placeholder:"REF"`
	Template string   `help:"Override only the IMAGE sbx boots (the ref 'make load' prints). Orthogonal to --kit." placeholder:"REF"`
	Mcp      []string `help:"Attach an MCP server at creation (repeatable)." placeholder:"M"`
	Pack     string   `help:"Active pack for this run (path or git-url); overrides the configured one." placeholder:"P"`
	Name     string   `help:"Sandbox name." placeholder:"N"`
	Model    string   `help:"Active pi model (passed through to pi)." placeholder:"M"`
	Intent   string   `help:"Resolve the session model via the router; --model overrides it. Intents: pix models show." placeholder:"NAME"`
	Replace  bool     `help:"Recreate the sandbox (sbx rm -f, then create) instead of re-attaching; picks up changed create-only flags."`
	Task     string   `help:"Launch an existing task's sandbox (same as 'pix task run NAME')." placeholder:"NAME"`

	// PiArg is the `--` tail, rewritten by rewriteRunPassthrough. Hidden
	// because a user never types it: they type `-- <pi args>`, which the
	// description above documents.
	PiArg []string `name:"pi-arg" hidden:""`
}

// rewriteRunPassthrough turns `run ... -- a b` into `run ... --pi-arg=a
// --pi-arg=b`, so the pi tail survives kong's end-of-flags handling intact.
// The `=value` form is deliberate: a pi arg starting with `-` can never be
// re-read as a flag of ours. Everything before the first `--` is untouched.
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
// inputs that are not literal flag values: `--task NAME` (an existing task
// checkout, resolved to its dir + sandbox name) and the workspace, which must
// be a real directory or this is a mistyped verb rather than a launch.
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
		Replace:     c.Replace,
		Pack:        c.Pack,
		Passthrough: c.PiArg,
	}
	if c.KitRef != "" {
		o.KitRef = launch.NormalizeKitRef(c.KitRef)
	}
	// `--task NAME` is the `pix task run NAME` shorthand: task.Resolve (L1) does
	// the resolution and this only fills the ordinary DIR + --name shape run
	// already understands, so no sandbox-lifecycle code is duplicated here. An
	// explicit --name still wins.
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
	// A non-"." workspace MUST be an existing directory. Otherwise a mistyped
	// verb (`pix run doctro`) would silently boot a junk sandbox named after the
	// typo.
	if err := launch.ValidateRunWorkspace(o.Workspace); err != nil {
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

// runFail reports a launch failure in run's own words and hands the root the
// exit code to use. The message is already complete, so it travels as a
// SilentError rather than being re-prefixed by the root's renderer.
func runFail(d *cli.Deps, code int, format string, a ...any) error {
	fmt.Fprintf(d.Err, "pix run: "+format+"\n", a...)
	return cli.SilentError{Code: code}
}

// runLaunch reads the config, resolves the run options (including a repo
// checkout for --dev), composes the sbx argv, and execs it with stdio
// inherited.
//
// The default path forwards NO credential bearer into the sandbox: Google
// Workspace rides the sbx gateway as the host-side `gog` MCP server (the
// `slack` pattern), authed entirely on the host — there is nothing to inject.
func runLaunch(d *cli.Deps, o launch.RunOpts) (err error) {
	var generatedKitDirs []string
	defer func() {
		if cerr := launch.CleanupGeneratedKitDirs(generatedKitDirs); cerr != nil {
			fmt.Fprintf(d.Err, "pix: warning: %v\n", cerr)
		}
	}()

	// Default the session intent from config (run_intent, the "overlord") when the
	// user pinned neither --model nor --intent. This is what flips the top-level
	// interactive orchestrator to its configured vendor (the stack ships
	// run_intent=overlord -> GPT-5.6 Sol). Track that it came from config, not a
	// flag: a bad config-sourced intent must NOT brick the launch the way an
	// explicit --intent typo does.
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

	// "none"/"off" is the explicit opt-out: use pi's own default model, no router
	// (honored for both --intent and run_intent). Kept here so a user who does not
	// want the overlord default has a first-class, documented escape.
	if o.Model == "" && (strings.EqualFold(o.Intent, "none") || strings.EqualFold(o.Intent, "off")) {
		o.Intent = ""
	}

	// Resolve --intent to a concrete session model via the router (unless --model
	// already pinned one, which wins). This makes the INTERACTIVE session use the
	// same cost/latency/accuracy routing the subagent crew uses.
	if o.Intent != "" && o.Model == "" {
		m, rerr := inference.ResolveSessionModel(o.Intent)
		if rerr != nil {
			return runFail(d, 2, "--intent %q: %v", o.Intent, rerr)
		}
		o.Model = m
		fmt.Fprintf(d.Err, "pix: intent %q -> model %s\n", o.Intent, m)
	}

	// Bare-minimum key bootstrap: a pi session needs at least one provider key,
	// so resolve the 1Password refs into sbx (a cheap no-op when a key is
	// already there), then refuse ONLY on a POSITIVE "no key" answer — sbx
	// absent or unprobeable means can't verify, which proceeds. keyResult is
	// KEPT for the readiness snapshot, so run pays for one `sbx secret ls`.
	var keyResult health.Result
	if _, lerr := defaultShellEnv().LookPath("sbx"); lerr == nil && !inference.ConfiguredKeylessInference() {
		env := defaultShellEnv()
		launch.BootstrapProviderKeys(env, os.Stdin, d.Err, d.Interactive)
		keyResult = launch.ProbeModelKeys(context.Background(), "")
		if launch.RefusesLaunch(keyResult) {
			fmt.Fprint(d.Err, secret.ModelKeyMissingMessage(env))
			return cli.SilentError{Code: 1}
		}
	}

	// Reconcile any control-plane proposal a prior in-session onboarding wrote
	// (<workspace>/.pix/onboarding.json), BEFORE LoadResolvedConfig so a fresh
	// create picks it up. Best-effort; a non-TTY just leaves the file.
	onboard.ReconcileOnboarding(o.Workspace, defaultShellEnv(), os.Stdin, os.Stdout, false, d.Interactive, onboardDeps())

	// Load the config for the rest of run (kits, mcp, gog, pack).
	cfg, _, err := workspace.LoadResolvedConfig()
	if err != nil {
		return runFail(d, 1, "%v", err)
	}
	if !inference.AllowsModel(cfg, o.Model) {
		return runFail(d, 2, "model %q is not available through the configured inference backends", o.Model)
	}

	// Own the sandbox name so we can manage its lifecycle. sbx would otherwise
	// auto-derive `pix-<dir>`. Resolved (and the sandbox state probed) BEFORE
	// any create-only input resolution below — a plain re-attach must never fail on
	// a --dev/checkout or --kit problem it doesn't even need.
	if o.Name == "" {
		o.Name = workspace.DeriveSandboxName(o.Workspace)
	}
	state := launch.ProbeTaskSandbox(defaultShellEnv(), o.Name)

	// Mirror sbx's own model: an existing sandbox (running OR stopped)
	// RE-ATTACHES rather than being recreated, so the create-only flags are not
	// even RESOLVED here. --replace forces rm -f + create for either state.
	if launch.WillCreate(state, o.Replace) {
		// Kit selection. A CLEAN released version (e.g. "0.0.16") pins the matching
		// git tag; anything else — an unstamped "dev" build, a "0.0.16+local" local
		// build, or non-semver — is UNRELEASED, its tag does not exist, so we never
		// pin v<version>. --dev forces the local checkout kit; an unreleased build
		// uses it too when a checkout is resolvable, else falls back to #ref=main.
		released := launcher.IsReleased(version)
		kitOverride := len(o.Kits) > 0

		// Released launchers pin the kit and image to their own stamped version.
		// Only explicit --kit-ref and version_pin overrides move that pin.
		if !o.Dev && !kitOverride {
			ref, src := launch.ResolveKitRef(version, o.KitRef, cfg.VersionPin)
			o.KitRef = ref
			if msg := launch.KitRefNotice(version, ref, src); msg != "" {
				fmt.Fprintln(d.Err, msg)
			}
		}

		if o.Dev {
			// --dev needs a resolvable repo checkout; fail loud otherwise. --dev is
			// create/replace-only (this branch), so it is a no-op on a plain re-attach.
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
	} else if o.Dev {
		fmt.Fprintln(d.Err, "pix: --dev is create/replace-only; re-attaching to the existing sandbox as-is (use --replace to recreate with --dev)")
	}

	// Active pack: mount its skills/ + knowledge/ so the pack's context loads in
	// this sandbox. --pack overrides config.Pack; with neither set, no pack is
	// active. Create-time only (skills + knowledge are create-time mounts; a
	// re-attach keeps what it was made with).
	// effectivePack is the pack root that ACTUALLY loaded, as opposed to the
	// merely CONFIGURED one: it is the configured root on a re-attach, and
	// ApplyPackStackToLaunch's honest return ("" when the pack degraded) on a
	// create. That is what keeps the sandbox.pack marker and the memory scope
	// from disagreeing about what loaded.
	effectivePack := pack.ActivePackRoot(cfg.Pack, o.Pack)
	if launch.WillCreate(state, o.Replace) {
		// Fatal on error (explicit --pack that doesn't load, or a declared
		// sandbox proxy whose kit can't be built — round-4 F2 fail-closed):
		// never create a sandbox missing context the pack declared.
		root, perr := launch.ApplyPackStackToLaunch(cfg, &o, defaultShellEnv())
		if perr != nil {
			fmt.Fprintf(d.Err, "pix: %v\n", perr)
			return cli.SilentError{Code: 1}
		}
		effectivePack = root
		// Inference is a generated create-time facet just like pack wrappers: the
		// sandbox receives only probed models, compiled routes, and public endpoint
		// metadata. Credential values never enter this kit.
		inferenceKit, ierr := inference.SynthesizeInferenceKit(cfg)
		if ierr != nil {
			fmt.Fprintf(d.Err, "pix: inference: %v\n", ierr)
			return cli.SilentError{Code: 1}
		}
		if inferenceKit != "" {
			o.PackKits = append(o.PackKits, inferenceKit)
			generatedKitDirs = append(generatedKitDirs, inferenceKit)
		}
		o.Models, ierr = inference.CallableRuntimeModels(cfg)
		if ierr != nil {
			fmt.Fprintf(d.Err, "pix: inference models: %v\n", ierr)
			return cli.SilentError{Code: 1}
		}
		contextKit, cerr := launch.SynthesizePersonalContextKit()
		if cerr != nil {
			fmt.Fprintf(d.Err, "pix: personal context: %v\n", cerr)
			return cli.SilentError{Code: 1}
		}
		if contextKit != "" {
			o.PackKits = append(o.PackKits, contextKit)
			generatedKitDirs = append(generatedKitDirs, contextKit)
		}
	}

	// Local-image preflight: when we're about to pin --template to a locally loaded
	// tag (a --dev / unreleased build) and that tag is NOT in sbx's image store, sbx
	// would try to PULL it from the registry — but local-* tags are never published,
	// so the user gets a confusing interactive "pull? use cached?" prompt and a slow
	// hang. Refuse fast with the real fix instead. (Only on create; a re-attach
	// reads the sandbox's own spec and doesn't re-pin --template.)
	if launch.WillCreate(state, o.Replace) && o.LocalImageTag != "" && len(o.Kits) == 0 && o.LocalKit != "" && o.Template == "" {
		if !launch.LocalImageLoaded(defaultShellEnv(), o.LocalImageTag) {
			fmt.Fprintf(d.Err, "pix: local image %s:%s is not loaded in sbx.\n", launch.DockerImageRepo, o.LocalImageTag)
			fmt.Fprintln(d.Err, "It's a local build (never published), so sbx would try to pull it and stall on a prompt.")
			fmt.Fprintln(d.Err, "Load this build into sbx first, from your pix checkout:")
			fmt.Fprintln(d.Err, "  make load")
			return cli.SilentError{Code: 1}
		}
	}

	// Same preflight for an explicit --template that names a local-* build: it's
	// never published, so an unloaded ref would make sbx stall on a pull prompt.
	// Only local-* tags get this guard — a published ref is legitimately pullable.
	if launch.WillCreate(state, o.Replace) && o.Template != "" {
		if tag := launch.TemplateTag(o.Template); strings.HasPrefix(tag, "local-") && !launch.LocalImageLoaded(defaultShellEnv(), tag) {
			fmt.Fprintf(d.Err, "pix: --template %s is not loaded in sbx.\n", o.Template)
			fmt.Fprintln(d.Err, "It's a local build (never published), so sbx would try to pull it and stall on a prompt.")
			fmt.Fprintln(d.Err, "Load it first, from the checkout that built it:  make load")
			return cli.SilentError{Code: 1}
		}
	}

	// Resolve every configured MCP server to attach at create (--static-mcp).
	// S01: all of them preload — no more eager/lazy split. Only needed on a
	// create — a re-attach never sends --static-mcp.
	if launch.WillCreate(state, o.Replace) {
		o.StaticMCP = mcp.AllPreloadedMCP(append(append([]string(nil), cfg.MCP...), o.MCP...))
	}

	plan := launch.PlanSandboxLaunch(state, o.Replace, cfg, o, version)
	if plan.Err != nil {
		// Fail closed BEFORE any output claims a replace/create/reattach is
		// happening, and before RmFirst or exec — see launch.PlanSandboxLaunch's
		// launch.SbxUnknown+replace case.
		return runFail(d, 1, "%v", plan.Err)
	}
	if !plan.Reattach {
		if verr := launch.ValidateCreateKits(plan.Args, launch.ValidateSbxKit); verr != nil {
			return runFail(d, 1, "%v", verr)
		}
	}
	switch {
	case o.Replace:
		fmt.Fprintf(d.Err, "pix run: replacing sandbox %q\n", o.Name)
	case plan.Reattach && state == launch.SbxRunning:
		fmt.Fprintf(d.Err, "pix run: re-attaching to running sandbox %q\n", o.Name)
	case plan.Reattach:
		fmt.Fprintf(d.Err, "pix run: starting + attaching existing sandbox %q (use --replace to recreate with current kit/mcp/flags)\n", o.Name)
	}
	// finding #8: a reattach is honest about live-vs-recreate for MOST facets
	// above, but says nothing about a pack switched since this sandbox was
	// created — its mcp/bin/skills are create-only and won't attach without
	// --replace. Surface that explicitly rather than silently reattaching to a
	// stale facet set.
	if msg := launch.StalePackReattachWarning(cfg, o, plan.Reattach); msg != "" {
		fmt.Fprintln(d.Err, msg)
	}
	// Product gap #2: reattach honesty. Separate from launch.StalePackReattachWarning
	// (which only speaks to skills/bin drift from a pack switch). This checks
	// MCP attachment PRECISELY, via the launcher's own receipt, regardless of
	// WHY a desired server might not be attached (config change, pack change,
	// explicit --mcp, or no receipt at all). Never auto-loads, only reports.
	if msg := launch.McpReattachWarning(cfg, o, plan.Reattach); msg != "" {
		fmt.Fprintln(d.Err, msg)
	}
	// Lazy auto-start: make the configured host services (memory) reachable
	// before the sandbox tries them, with a SHORT budget — the launch waits AT
	// MOST service.EnsureRunTimeout (8s), covering spawn-lock acquisition AND
	// the health poll under one deadline (M2), then proceeds regardless (recall
	// degrades in-VM exactly as before). service.Ensure prints its own
	// progress/failure lines.
	service.EnsureUp(nil, service.EnsureRunTimeout)

	// Readiness, reusing the key evidence the launch gate above already paid
	// for. AT MOST launch.WarningLimit rows (a wall of text here is how a user
	// learns to skip readiness output), and it NEVER blocks: the missing
	// provider key handled above is the only launch-stopping gap.
	launch.RenderWarnings(d.Err, launch.FastSnapshot(context.Background(), cfg, keyResult), launch.WarningLimit)

	// Local model + memory scope, handed to the in-VM ollama-bridge and
	// recall/capture extensions via per-run workspace files. Best-effort: an
	// unloadable pack degrades to unscoped rather than failing run. Shared with
	// `task new` so both launch paths write the SAME pack context.
	launch.WritePackContextFiles(cfg, o, effectivePack)

	// Trusted host state travels ONLY inside the launcher-generated initial
	// prompt, never as a workspace file a cloned repo could plant. It is a
	// no-op (and probes nothing) without such a prompt. The --pack override
	// only takes effect on a CREATE, so a re-attach must not claim it.
	packForState := ""
	if launch.WillCreate(state, o.Replace) {
		packForState = o.Pack
	}
	// A HARD contract: when a generated prompt IS present it is the fenced
	// agent's ONLY trusted host truth, so a launch that cannot build it ABORTS
	// before exec. Destructive replacement is deliberately last — every
	// fallible read-only preflight must pass before the old sandbox is removed.
	var args []string
	if perr := launch.PreflightBeforeReplace(func() error {
		var preflightErr error
		args, preflightErr = launch.InjectTrustedHostState(plan.Args, cfg, defaultShellEnv(), packForState)
		if preflightErr != nil {
			return fmt.Errorf("could not build trusted host state: %w", preflightErr)
		}
		return nil
	}, func() error {
		return launch.ApplyReplaceRm(defaultShellEnv(), plan, o.Name)
	}); perr != nil {
		return runFail(d, 1, "%v", perr)
	}
	// Record the pack only after every hard-fail preflight and any replacement
	// removal succeeded. A failed trusted-state preflight must leave both the old
	// sandbox and its create-time marker untouched.
	if launch.DefinitelyCreating(state, o.Replace) {
		launch.WriteSandboxPackMarker(o.Workspace, effectivePack)
	}

	if os.Getenv("PIX_DEBUG") != "" {
		fmt.Fprintln(d.Err, "+ sbx "+strings.Join(args, " "))
	}

	cmd := exec.Command("sbx", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// The default path injects no credential bearer: gog authenticates on the host
	// inside the gateway-spawned MCP server, so the sandbox never sees a Google
	// token. A future external credential broker plugin would set its own bearer through
	// the retained generic seam and own that plumbing itself.
	cmd.Env = os.Environ()
	// S03: on a DEFINITE create (launch.DefinitelyCreating — the same predicate that
	// gates the sandbox.pack marker above; a plain re-attach must never write a
	// fresh create receipt over the existing lifetime), record the
	// create receipt ONLY after this exact `sbx run` exec has itself succeeded
	// — never before, never on failure, never on reattach.
	if xerr := launch.ExecSbxRunAndRecordCreate(cmd, launch.DefinitelyCreating(state, o.Replace), o.Name, workspace.CanonicalPath(o.Workspace), o.StaticMCP); xerr != nil {
		var rerr *workspace.ReceiptRecordError
		if errors.As(xerr, &rerr) {
			// The sandbox itself WAS created successfully — only the local
			// receipt failed. Say so honestly rather than implying the launch
			// failed, but still exit non-zero: doctor/status must not be told
			// this sandbox's MCP set is recorded when it isn't.
			fmt.Fprintf(d.Err, "pix run: %v\n", rerr)
			fmt.Fprintln(d.Err, "the sandbox itself launched fine; only pix's local record of its preloaded MCP set failed to write. check state-dir permissions and re-run `pix doctor`.")
			return cli.SilentError{Code: 1}
		}
		var exitErr *exec.ExitError
		if errors.As(xerr, &exitErr) {
			// If we pinned a git #ref kit and sbx bailed (classically git exit 128
			// "Remote branch not found"), the raw error is opaque — replace it with
			// an actionable note instead of leaking the git 128.
			if msg := launch.KitResolveFailureMsg(launch.PinnedGitKit(args)); msg != "" {
				fmt.Fprintln(d.Err, msg)
			}
			// A re-attach exec can fail on an sbx version that won't reattach a
			// kit-created sandbox; don't leave the user stuck without a next step.
			if plan.Reattach {
				fmt.Fprintf(d.Err, "pix run: re-attach failed; recreate it with: %s\n", launch.RunReplaceCommand(o.Workspace))
			}
			return cli.SilentError{Code: exitErr.ExitCode()}
		}
		fmt.Fprintf(d.Err, "pix run: exec sbx: %v\n", xerr)
		if errors.Is(xerr, exec.ErrNotFound) {
			fmt.Fprintln(d.Err, "install sbx with: "+doctor.SbxInstallHint)
		}
		if plan.Reattach {
			fmt.Fprintf(d.Err, "pix run: re-attach failed; recreate it with: %s\n", launch.RunReplaceCommand(o.Workspace))
		}
		return cli.SilentError{Code: 1}
	}
	return nil
}
