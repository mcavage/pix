// run_cmd.go — `pix run` as a typed root child. The two things the launch
// package deliberately does not know — the verb table (for the "did you mean"
// hint) and the real env — are passed to it from here, at each call, rather
// than installed into package vars by an init().
//
// One shape the grammar cannot express stays in FRONT of the parser: the `--`
// pi passthrough. kong consumes `--` as an end-of-flags marker and would feed
// the first pi arg to the DIR positional, so the tail is rewritten into
// repeated `--pi-arg=` values (rewriteRunPassthrough) — an argv REWRITE, like
// the `task NAME path` one, not a second parser.
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
	"pix/host/sandbox"
	"pix/host/secret"
	"pix/host/service"
	"pix/host/workflow/doctor"
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
                                 --kit/--mcp/--template, --dev, and the
                                 create-only skill flags are NOT re-sent (--dev
                                 is create-only and is ignored, with a note, on
                                 an attach). --model/--intent are NOT create-
                                 only: they are pi runtime args, so they still
                                 reach the pi session on an attach too.
                                 An attach whose create-time MCP set or image
                                 no longer matches is REFUSED, not silently
                                 attached; to recreate, remove it first:
                                 pix rm <box> && pix run. There is no
                                 --replace: a forced removal that races another
                                 shell's live session is exactly what the
                                 proof-gated 'pix rm' exists to prevent.

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
	Kit      []string `help:"Override the KIT — image + entrypoint + creds + egress + skills — instead of the auto git/local pin (repeatable; path or git+URL)." placeholder:"K"`
	KitRef   string   `help:"Pin the auto kit to a git ref (v0.1.0, main) instead of the latest stable release." placeholder:"REF"`
	Template string   `help:"Override only the IMAGE sbx boots (the ref 'make load' prints). Orthogonal to --kit." placeholder:"REF"`
	Mcp      []string `help:"Attach an MCP server at creation (repeatable)." placeholder:"M"`
	Pack     string   `help:"Active pack for this run (path or git-url); overrides the configured one." placeholder:"P"`
	Name     string   `help:"Sandbox name." placeholder:"N"`
	Model    string   `help:"Active pi model (passed through to pi)." placeholder:"M"`
	Intent   string   `help:"Resolve the session model via the router; --model overrides it. Intents: pix models show." placeholder:"NAME"`
	// Replace is RETIRED (U04e). It is still parsed, hidden, so typing it
	// answers with the standard PIX_RETIRED notice and the two-step
	// replacement instead of kong's "unknown flag" — a stale script or shell
	// history gets a recovery path, not a syntax error.
	Replace bool   `hidden:"" help:"Retired: remove the sandbox explicitly (pix rm BOX), then run."`
	Task    string `help:"Launch an existing task's sandbox (same as 'pix task run NAME')." placeholder:"NAME"`
	Keep    bool   `short:"k" help:"Keep the sandbox when the last shell exits: a sticky, identity-bound marker the teardown/orphan reaper refuses on (an explicit 'pix rm' still removes it)."`

	// PiArg is the `--` tail, rewritten by rewriteRunPassthrough. Hidden
	// because a user never types it: they type `-- <pi args>`, which the
	// description above documents.
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
	// `--task NAME` is the `pix task run NAME` shorthand: task.Resolve (L1) does
	// the work and this only fills the ordinary DIR + --name shape, so no
	// sandbox-lifecycle code is duplicated here. An explicit --name still wins.
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
	// The retired flag answers before ANY resolution, probe or mutation: the
	// same inert contract every other retired surface holds (retired.go).
	if c.Replace {
		return retiredFlag(d.Err, "run", "--replace")
	}
	o, err := c.opts()
	if err != nil {
		return err
	}
	return runLaunch(d, o)
}

// resolveSandboxName is the sandbox `pix run` actually targets: an explicit
// --name travels verbatim, unchanged — it is a user-owned display name, never
// mangled or reinterpreted. Absent that, the DEFAULT is the deterministic,
// digest-suffixed sandbox.Name(workspace) — the SAME identity
// launch.SessionName keys lease state by — not workspace.DeriveSandboxName's
// bare "pix-<basename>" (still used, unchanged, by the separate MCP-receipt
// lattice). Two workspaces that happen to share a basename get two DIFFERENT
// default sandbox names, because the digest is computed over the full
// canonical path, not the basename alone (see sandbox.Name's own doc).
func resolveSandboxName(explicit, workspace string) string {
	if explicit != "" {
		return explicit
	}
	return sandbox.Name(workspace)
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
// inherited. It forwards NO credential bearer into the sandbox: host MCP
// servers authenticate on the host, so there is nothing to inject.
func runLaunch(d *cli.Deps, o launch.RunOpts) (err error) {
	var generatedKitDirs []string
	defer func() {
		if cerr := launch.CleanupGeneratedKitDirs(generatedKitDirs); cerr != nil {
			fmt.Fprintf(d.Err, "pix: warning: %v\n", cerr)
		}
	}()

	// Default the session intent from config (run_intent, the "overlord") when
	// the user pinned neither --model nor --intent — this is what flips the
	// top-level orchestrator to its configured vendor.
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

	// A pi session needs at least one provider key: resolve the 1Password refs
	// into sbx (a no-op when a key is there), then refuse ONLY on a POSITIVE "no
	// key" answer — unprobeable means can't verify, which proceeds. keyResult is
	// kept for the readiness snapshot, so run pays for one `sbx secret ls`.
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

	// Reconcile a prior in-session onboarding proposal (<workspace>/.pix/
	// onboarding.json) BEFORE LoadResolvedConfig so a fresh create picks it up.
	// Best-effort; a non-TTY just leaves the file.
	provision.ReconcileOnboarding(o.Workspace, defaultShellEnv(), os.Stdin, os.Stdout, false, d.Interactive)

	// Load the config for the rest of run (kits, mcp, gog, pack).
	cfg, _, err := workspace.LoadResolvedConfig()
	if err != nil {
		return runFail(d, 1, "%v", err)
	}
	if !inference.AllowsModel(cfg, o.Model) {
		return runFail(d, 2, "model %q is not available through the configured inference backends", o.Model)
	}

	// Own the sandbox name (sbx would auto-derive `pix-<dir>`) and probe its
	// state BEFORE any create-only resolution below, so a plain re-attach never
	// fails on a --dev/checkout or --kit problem it does not even need.
	o.Name = resolveSandboxName(o.Name, o.Workspace)
	state := launch.ProbeTaskSandbox(defaultShellEnv(), o.Name)

	// Mirror sbx's own model: an existing sandbox (running OR stopped) is
	// ATTACHED to rather than recreated, so the create-only flags are not even
	// RESOLVED here. ONE predicate answers it, for this gate and for the plan.
	creating := launch.WillCreate(state)
	if creating {
		// Kit selection. A CLEAN released version pins the matching git tag;
		// anything else (unstamped "dev", "0.0.16+local", non-semver) is
		// UNRELEASED and its tag does not exist, so v<version> is never pinned.
		// --dev forces the local checkout kit; an unreleased build uses it too
		// when a checkout is resolvable, else falls back to #ref=main.
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
	} else if o.Dev {
		fmt.Fprintf(d.Err, "pix: --dev is create-only; attaching to the existing sandbox as-is (to get --dev, %s)\n", launch.RecreateGuidance(o.Name))
	}

	// Active pack: mount its skills/ + knowledge/ into this sandbox. --pack
	// overrides config.Pack; create-time only, since a re-attach keeps what it
	// was made with. effectivePack is the pack that ACTUALLY loaded (not merely
	// the configured one), which is what keeps the sandbox.pack marker and the
	// memory scope from disagreeing.
	effectivePack := pack.ActivePackRoot(cfg.Pack, o.Pack)
	if creating {
		// Fail closed on an explicit --pack that doesn't load, or a declared
		// sandbox proxy whose kit can't be built: never create a sandbox missing
		// context the pack declared.
		root, perr := launch.ApplyPackStackToLaunch(cfg, &o, defaultShellEnv(), d.Err)
		if perr != nil {
			return runFail(d, 1, "%v", perr)
		}
		effectivePack = root
		// Inference is a generated create-time facet like pack wrappers: probed
		// models, compiled routes, public endpoint metadata. No credential value
		// ever enters this kit.
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
		contextKit, cerr := launch.SynthesizePersonalContextKit()
		if cerr != nil {
			return runFail(d, 1, "personal context: %v", cerr)
		}
		if contextKit != "" {
			o.PackKits = append(o.PackKits, contextKit)
			generatedKitDirs = append(generatedKitDirs, contextKit)
		}
	}

	// Local-image preflight: a local-* tag is NEVER published, so pinning
	// --template to one sbx has not loaded makes sbx try to pull it and stall on
	// an interactive prompt. Refuse fast with the real fix instead. Only on a
	// create: a re-attach reads the sandbox's own spec and re-pins nothing.
	if creating {
		switch {
		case o.Template != "":
			if tag := launch.TemplateTag(o.Template); strings.HasPrefix(tag, "local-") && !launch.LocalImageLoaded(defaultShellEnv(), tag) {
				fmt.Fprintf(d.Err, "pix: --template %s is not loaded in sbx.\n", o.Template)
				fmt.Fprintln(d.Err, "It's a local build (never published), so sbx would try to pull it and stall on a prompt.")
				fmt.Fprintln(d.Err, "Load it first, from the checkout that built it:  make load")
				return cli.SilentError{Code: 1}
			}
		case o.LocalImageTag != "" && o.LocalKit != "" && len(o.Kits) == 0:
			if !launch.LocalImageLoaded(defaultShellEnv(), o.LocalImageTag) {
				fmt.Fprintf(d.Err, "pix: local image %s:%s is not loaded in sbx.\n", launch.DockerImageRepo, o.LocalImageTag)
				fmt.Fprintln(d.Err, "It's a local build (never published), so sbx would try to pull it and stall on a prompt.")
				fmt.Fprintln(d.Err, "Load this build into sbx first, from your pix checkout:")
				fmt.Fprintln(d.Err, "  make load")
				return cli.SilentError{Code: 1}
			}
		}
	}

	// Every configured MCP server attaches at create (--static-mcp); a re-attach
	// never sends it.
	if creating {
		o.StaticMCP = mcp.AllPreloadedMCP(append(append([]string(nil), cfg.MCP...), o.MCP...))
	}

	plan := launch.PlanSandboxLaunch(state, cfg, o, version)
	if plan.Err != nil {
		// Fail closed BEFORE any output claims a create or attach is happening,
		// and before exec (SbxUnknown).
		return runFail(d, 1, "%v", plan.Err)
	}
	// U04c2: sessionKey is the lease identity for THIS workspace (the same
	// digest name resolveSandboxName defaults the sandbox to), and fp is what
	// a later attach must match. The DECISION to exec-attach is probed here,
	// read-only, and re-validated under the lifecycle lock by launch.RunSession
	// — which is also where a fingerprint divergence refuses.
	sessionKey := launch.SessionName(o.Workspace)
	fp := launch.SessionFingerprint(cfg, o)
	attachExec := false
	if plan.Reattach {
		_, attachExec = launch.FindPositivelyIdentifiedRunning(defaultShellEnv(), o.Name)
	}
	if !plan.Reattach {
		if verr := launch.ValidateCreateKits(plan.Args, launch.ValidateSbxKit); verr != nil {
			return runFail(d, 1, "%v", verr)
		}
	}
	// What this launch is DOING, in one line. Nothing here warns about
	// create-only drift (a stale pack, a changed MCP set): those claims used to
	// be assembled from a workspace marker and a launcher receipt, and now the
	// recorded create-time FINGERPRINT decides them under the lifecycle lock —
	// an attach that no longer matches is refused with RecreateGuidance rather
	// than attached with a warning nobody can verify.
	switch {
	case plan.Reattach && state == launch.SbxRunning:
		fmt.Fprintf(d.Err, "pix run: attaching to running sandbox %q\n", o.Name)
	case plan.Reattach:
		fmt.Fprintf(d.Err, "pix run: starting + attaching existing sandbox %q\n", o.Name)
	}
	// Lazy auto-start of the configured host services, under ONE short deadline
	// (spawn lock + health poll); the launch proceeds regardless, and recall
	// degrades in-VM as before. service.Ensure prints its own lines.
	service.EnsureUp(nil, service.EnsureRunTimeout)

	// Readiness, reusing the key evidence the launch gate already paid for. AT
	// MOST launch.WarningLimit rows, and it NEVER blocks: the missing provider
	// key handled above is the only launch-stopping gap.
	launch.RenderWarnings(d.Err, launch.FastSnapshot(context.Background(), cfg, keyResult), launch.WarningLimit)

	// Local model + memory scope for the in-VM ollama-bridge and recall/capture
	// extensions. Best-effort: an unloadable pack degrades to unscoped.
	launch.WritePackContextFiles(cfg, o, effectivePack, d.Err)

	// Trusted host state travels ONLY inside the launcher-generated initial
	// prompt, never as a workspace file a cloned repo could plant, and probes
	// nothing without one. --pack applies on CREATE only, so a re-attach must
	// not claim it.
	packForState := ""
	if creating {
		packForState = o.Pack
	}
	// A HARD contract: a generated prompt is the fenced agent's ONLY trusted
	// host truth, so a launch that cannot build it ABORTS before exec. Nothing
	// destructive follows it any more — with --replace retired, this launch
	// removes nothing, so there is no ordering left to get wrong.
	args, perr := launch.InjectTrustedHostState(plan.Args, cfg, defaultShellEnv(), packForState)
	if perr != nil {
		return runFail(d, 1, "could not build trusted host state: %v", perr)
	}

	if os.Getenv("PIX_DEBUG") != "" {
		fmt.Fprintln(d.Err, "+ sbx "+strings.Join(args, " "))
	}

	// U04c2: the sandbox's whole create/attach lifecycle runs through
	// launch.RunSession, which owns the ordering: lifecycle lock EX, a FRESH
	// probe under it, the child started, the create-time facts (instance id,
	// fingerprint, exact pi invocation, MCP receipt) all recorded, the refs
	// SHARED reference taken while lifecycle is still held, lifecycle
	// released, and only THEN the session waited out. This command layer owns
	// stdio wiring, the exit code, and the words — never the ordering.
	// ONE invocation builder, used for both roles it can play: the argv this
	// create records, and the safe recomputed default an attach falls back to
	// when nothing was ever recorded — so "default" can never drift from what
	// a create would have sent.
	invocation := launch.BuildPiInvocation(launch.LiveSkillDirs(cfg, o), o)
	spec := launch.SessionSpec{
		Key:               sessionKey,
		Name:              o.Name,
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
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			// No credential bearer: host MCP servers authenticate on the host,
			// so the sandbox never sees a token.
			cmd.Env = os.Environ()
			return cmd
		},
	}
	if xerr := launch.RunSession(spec, deps); xerr != nil {
		var refused *launch.SessionRefused
		if errors.As(xerr, &refused) {
			// Decided under the lifecycle lock, before anything started: no
			// create, no attach, no removal. run's own complete message.
			return runFail(d, 1, "%v", refused)
		}
		var exitErr *exec.ExitError
		if errors.As(xerr, &exitErr) {
			// A pinned git #ref kit that sbx could not resolve fails with an opaque
			// git 128; replace it with an actionable note.
			if msg := launch.KitResolveFailureMsg(launch.PinnedGitKit(args)); msg != "" {
				fmt.Fprintln(d.Err, msg)
			}
			// A re-attach can fail on an sbx that won't reattach a kit-created
			// sandbox; never leave the user without a next step.
			if plan.Reattach {
				fmt.Fprintf(d.Err, "pix run: attach failed; %s\n", launch.RecreateGuidance(o.Name))
			}
			return cli.SilentError{Code: exitErr.ExitCode()}
		}
		fmt.Fprintf(d.Err, "pix run: exec sbx: %v\n", xerr)
		if errors.Is(xerr, exec.ErrNotFound) {
			fmt.Fprintln(d.Err, "install sbx with: "+doctor.SbxInstallHint)
		}
		if plan.Reattach {
			fmt.Fprintf(d.Err, "pix run: attach failed; %s\n", launch.RecreateGuidance(o.Name))
		}
		return cli.SilentError{Code: 1}
	}
	return nil
}
