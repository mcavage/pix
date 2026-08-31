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
	"time"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/health"
	"pix/host/inference"
	"pix/host/launcher"
	"pix/host/mcp"
	"pix/host/sandbox"
	"pix/host/secret"
	"pix/host/workflow/doctor"
	nativeenv "pix/host/workflow/env"
	"pix/host/workflow/launch"
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
                                 later. --model is NOT create-only: it is a pi
                                 runtime arg, so it still reaches the pi
                                 session on an attach too.
                                 An attach whose create-time declaration no
                                 longer matches is REFUSED, not silently
                                 attached; to recreate, remove it first:
                                 pix rm <box> && pix run. The ONE exception is
                                 a drift that is only Pix's own pinned build
                                 (image, pull policy, kits) — an ordinary Pix
                                 upgrade — which is removed and recreated
                                 automatically when the sandbox is provably
                                 idle: fresh listing, zero holders, no keep,
                                 the recorded instance, a direct host mount,
                                 and a still-reviewed environment. Any missing
                                 proof refuses and names it.

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

	Dev    bool   `help:"Mode B: use the local checkout kit + load skills live from it (needs a checkout)."`
	Name   string `help:"Sandbox name." placeholder:"N"`
	Env    string `help:"Launch under a named environment (an exact ~/.pix/envs/<name> directory, never a prefix); overrides the machine default for this run only." placeholder:"NAME"`
	Model  string `help:"Active pi model (passed through to pi)." placeholder:"M"`
	Resume string `help:"Resume this pi session (passed through to pi on every attach or create)." placeholder:"SESSION"`
	Task   string `help:"Launch an existing task's sandbox." placeholder:"NAME"`
	Keep   bool   `short:"k" help:"Keep the sandbox when the last shell exits: a sticky, identity-bound marker the teardown/orphan reaper refuses on (an explicit 'pix rm' still removes it)."`

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
		Name:        c.Name,
		Env:         c.Env,
		Model:       c.Model,
		Resume:      c.Resume,
		Passthrough: c.PiArg,
		Keep:        c.Keep,
	}
	// `--task NAME` resolves an existing task checkout by name: task.Resolve does the work
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

// attachGateFor completes the attach gate with the two facts only this
// layer holds: the sbx row it just read, and the RecreateProof built from
// this host's real lease state for that sandbox.
//
// It is a named function, not an inline struct literal, because the proof
// is the field whose ZERO VALUE silently authorizes nothing: the first
// cutover commit shipped the whole recreation decision layer and never
// filled it, so every pinned-image upgrade still refused with the manual
// `pix rm && pix run`. Filling it in one testable place is what keeps that
// from happening again.
func attachGateFor(sessionKey, workspace string, entry *sandbox.Entry, g launch.AttachGate) launch.AttachGate {
	g.Entry = entry
	// entry != nil is this launch's own fresh, schema-verified listing;
	// nothing here reads a recorded one.
	g.Proof = launch.RecreateProofFor(sessionKey, workspace, entry != nil)
	return g
}

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
// runLaunch is the entry point every caller uses. It keeps one pristine copy
// of the resolved options so that a SAFE AUTOMATIC RECREATE (a drift whose
// every facet is a Pix-owned construction pin, behind a full proof set) can
// re-enter the ordinary create path with exactly the options the user asked
// for, rather than with the half-resolved attach-path values of the first
// attempt. Exactly one recreate is allowed per invocation: the second attempt
// finds no sandbox at all, so it creates, and a drift refusal there would be
// a real refusal rather than a loop.
func runLaunch(d *cli.Deps, o launch.RunOpts) error {
	return runLaunchAttempt(d, cloneRunOpts(o), cloneRunOpts(o))
}

// cloneRunOpts copies the option struct AND its slices, so an attempt that
// appends to StaticMCP/PackKits cannot reach the retry's copy through a
// shared backing array.
func cloneRunOpts(o launch.RunOpts) launch.RunOpts {
	cp := func(s []string) []string {
		if s == nil {
			return nil
		}
		return append([]string(nil), s...)
	}
	o.Skills, o.Kits, o.MCP = cp(o.Skills), cp(o.Kits), cp(o.MCP)
	o.StaticMCP, o.PackKits, o.Passthrough = cp(o.StaticMCP), cp(o.PackKits), cp(o.Passthrough)
	return o
}

func runLaunchAttempt(d *cli.Deps, o launch.RunOpts, retry launch.RunOpts) (err error) {
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

	// Resolve the environment BEFORE any model defaulting, any probe, and any
	// sandbox side effect (§6.1, AC-21): an explicit `--env` names an EXACT
	// registered environment, never a prefix and never a fuzzy match, and an
	// unknown one exits non-zero having created nothing and having fallen back
	// to no default — a typo silently launching the wrong credential set is the
	// worst outcome this feature can produce. Nothing on this path writes
	// config: `--env` selects for THIS run only (AC-22).
	selection, trustSnap, serr := resolveRunEnvironment(o.Env)
	if serr != nil {
		fmt.Fprintln(d.Err, strings.TrimRight(serr.Error(), "\n"))
		return cli.SilentError{Code: 2}
	}
	o.EnvName = selection.Name

	// CRITICAL (security re-review): an environment this run actually
	// selected must be REVIEWED before anything past this point runs a
	// host command, resolves a credential, or composes a mount — fail
	// closed on a non-interactive terminal, first-use BOM/default-No on an
	// interactive one (run_trust.go). This is the FIRST of two checks; the
	// second, immediately before the actual sbx mutation, closes the TOCTOU
	// window between resolving the environment and using it. Both calls
	// bind to trustSnap, the ONE in-memory snapshot resolveRunEnvironment
	// resolved above (M1, security re-review: trust TOCTOU) — never a
	// fresh, independent re-read of the environment by name.
	if terr := runTrustGate(d, trustSnap, false); terr != nil {
		return terr
	}

	// §6.3's model precedence, in one place: --model > the selected
	// environment's [models].main > pi's own default.
	if model, source := launch.SelectSessionModel(o.Model, selection.Sidecar); model != "" {
		o.Model = model
		if source == "[models].main" {
			fmt.Fprintf(d.Err, "pix: environment %q -> model %s\n", selection.Name, model)
		}
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

		// Inference is a generated create-time facet: probed models, compiled
		// routes, public endpoint metadata. No credential value enters it. The
		// selected environment's authored roster (E3.1) travels into the
		// generated mixin kit, and is validated (E3.3) against the SAME model
		// set the manifest ships before anything is created.
		shipped, _, _ := listAgents()
		roster := launch.RosterInputFor(selection.Sidecar, shipped)
		if verr := validateRunRoster(cfg, selection, shipped); verr != nil {
			return runFail(d, 2, "%v", verr)
		}
		inferenceKit, ierr := inference.SynthesizeInferenceKit(cfg, roster)
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

		// Every configured MCP server attaches at create (--static-mcp).
		o.StaticMCP = composeStaticMCP(o.StaticMCP, cfg.MCP, o.MCP)
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
	// root is this launch's interactive-root reference (session.go/session_root.go,
	// architecture §7.2): acquired synchronously below once the attach path has a
	// positive instance receipt, or awaited concurrently with a fresh create
	// further down. Released on every path out of runLaunchAttempt via releaseRoot.
	var root *interactiveRoot
	releaseRoot := func(failed bool) { root.release(failed) }
	if plan.Reattach {
		// FindPositivelyIdentified accepts a RUNNING-OR-STOPPED row: a stopped
		// sandbox is still a legitimate reattach target (docs/getting-started.md:
		// "A sandbox already exists -> reattach, running or stopped, as-is") and
		// must not be refused outright the way a running-only gate would.
		entry, _ := launch.FindPositivelyIdentified(defaultShellEnv(), o.Name)
		recordedInstanceID := launch.ReadRecordedInstanceID(sessionKey)

		// §10.2's third condition: the RECREATE-ONLY creation fingerprint,
		// recomputed from the environment as it is now under the STORED
		// launcher key (never a freshly generated one — a missing key is the
		// post-reset state and attributes as exactly one drift).
		stored, storedFound := launch.ReadCreationFingerprint(sessionKey)
		current, resetInvalidated, cerr := currentCreationFingerprint(cfg, o, selection, version)
		if cerr != nil {
			return runFail(d, 1, "environment: %v", cerr)
		}
		rec, _ := launch.ReadSessionEnvironment(sessionKey)
		decision := launch.DecideEnvAttach(attachGateFor(sessionKey, o.Workspace, entry, launch.AttachGate{
			RecordedInstanceID: recordedInstanceID,
			Stored:             stored,
			StoredFound:        storedFound,
			Current:            current,
			ResetInvalidated:   resetInvalidated && storedFound,
			Reviewed:           !selection.Selected() || selection.Reviewed,
			Tree:               selection.Tree,
		}), o.Name, rec.Name)
		// A recreation-safe drift with a complete proof set: remove the
		// sandbox through the ordinary proof-gated teardown, then re-enter
		// the ordinary create path with the user's original options. No
		// forced removal, no second create path, and one attempt only.
		if decision.Recreate != nil {
			if retry.Recreated {
				return runFail(d, 1, "%q still drifts after one automatic recreate; %s",
					o.Name, launch.EnvRecreateGuidance(o.Name, rec.Name))
			}
			fmt.Fprintf(d.Err, "pix run: %s\n", decision.Recreate.Reason)
			if rerr := launch.ExecuteRecreate(defaultShellEnv(), decision.Recreate, sessionKey, launch.TeardownOptions{
				Planner: launch.EnvTeardownPlanner(o.Name),
			}); rerr != nil {
				fmt.Fprintf(d.Err, "pix run: %v\n", rerr)
				fmt.Fprintf(d.Err, "pix run: recreate it yourself: %s\n", launch.EnvRecreateGuidance(o.Name, rec.Name))
				return cli.SilentError{Code: 1}
			}
			fmt.Fprintf(d.Err, "pix run: removed %q; recreating it\n", o.Name)
			retry.Recreated = true
			return runLaunchAttempt(d, cloneRunOpts(retry), cloneRunOpts(retry))
		}
		if !decision.Attach {
			// E1.6/E2.6's I4 diagnostic: a creation-fingerprint drift refusal
			// appends one bounded recreatelog record; every other refusal
			// reason (RecordAttachRefusal's own guard) appends nothing. This
			// is diagnostic only — a write failure here is swallowed rather
			// than layered onto the refusal it is describing.
			if sdir, serr := config.StateDir(); serr == nil {
				_ = launch.RecordAttachRefusal(sdir, rec.Name, decision)
			}
			return runFail(d, 1, "%s", decision.Refusal)
		}
		// exec has no "start" of its own and fails outright against a stopped
		// sandbox — only a RUNNING, positively identified entry may exec. A
		// stopped one still attaches (decision.Attach is true above), but via
		// spec.AttachArgs, the legacy `sbx run --name` reattach that actually
		// starts it, honoring THIS launch's current --model/--resume exactly
		// like BuildReattachArgs already does for a fresh create.
		attachExec = entry != nil && entry.State == sandbox.StateRunning

		// The interactive-root Hold: this attach has a POSITIVE instance receipt
		// right here (a fresh probe's entry.InstanceID, else the recorded one
		// this same decision already trusted), so a second live interactive root
		// for the SAME sandbox is refused BEFORE anything execs, not discovered
		// after two terminals are already sharing one pi session.
		attachInstanceID := recordedInstanceID
		if entry != nil && entry.InstanceID != nil && *entry.InstanceID != "" {
			attachInstanceID = *entry.InstanceID
		}
		if attachInstanceID != "" {
			r, herr := holdInteractiveRootNow(sessionKey, o.Name, o.Workspace, selection.Name, o.Model, attachInstanceID)
			if herr != nil {
				return runFail(d, 1, "%v", herr)
			}
			root = r
		}
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
	// Readiness, reusing the key evidence the launch gate already paid for. AT
	// MOST launch.WarningLimit rows, and it NEVER blocks: the missing provider
	// key handled above is the only launch-stopping gap.
	launch.RenderWarnings(d.Err, launch.FastSnapshot(context.Background(), cfg, keyResult), launch.WarningLimit)

	// Local model for the in-VM ollama-bridge and recall/capture extensions.
	launch.WriteWorkspaceContextFiles(cfg, o, d.Err)

	// A HARD contract: a generated prompt is the fenced agent's ONLY trusted host
	// truth, so a launch that cannot build it ABORTS before exec. After the
	// cutover the payload is injected into the pi INVOCATION this launch
	// actually execs (`sbx exec -it <name> -- pi <invocation>`), not into an
	// `sbx run` argv that is no longer spawned.
	invocation := launch.BuildPiInvocation(launch.LiveSkillDirs(cfg, o), o)
	invocation, perr := launch.InjectTrustedHostState(invocation, cfg, defaultShellEnv())
	if perr != nil {
		return runFail(d, 1, "could not build trusted host state: %v", perr)
	}

	if os.Getenv("PIX_DEBUG") != "" {
		fmt.Fprintln(d.Err, "+ sbx "+strings.Join(plan.Args, " "))
	}

	// launch.RunSession OWNS the create/attach ordering: lifecycle lock EX, a fresh
	// probe under it, the child started, the create-time facts recorded, the refs
	// SHARED reference taken while lifecycle is still held, lifecycle released, and
	// only THEN the session waited out. This layer owns stdio wiring, the exit code
	// and the words, never the ordering. ONE invocation builder serves both the argv
	// this create records and the default an attach falls back to, so they cannot
	// drift.
	// E2.5's ONE create path: compose the real RuntimeFacts once, render them
	// through the single effective-document producer, persist the EXACT bytes
	// this create is about to use, and create through `sbx env create` — the
	// same stable path removal recomposes. No parallel old create path stays
	// selectable.
	var envCreateArgs []string
	if creating {
		in, ierr := runEffectiveInput(cfg, o, selection, version)
		if ierr != nil {
			return runFail(d, 1, "environment: %v", ierr)
		}
		// The launcher's creation-fingerprint key is ESTABLISHED here, once,
		// before the first fingerprint and before anything is created. A
		// fresh host's first interpolated create is an ordinary create, not a
		// reset-invalidated one.
		resolver, kerr := launch.CreateHMACResolver(configDirOrEmpty(), nil)
		if kerr != nil {
			return runFail(d, 1, "environment: %v", kerr)
		}
		eff, eerr := launch.RenderEffectiveEnvironment(in, resolver)
		if eerr != nil {
			return runFail(d, 1, "environment: %v", eerr)
		}
		envCreateArgs = launch.EnvCreateArgs(eff.Path)
		if rerr := launch.RecordCreationFingerprint(sessionKey, eff.Fingerprint); rerr != nil {
			return runFail(d, 1, "environment: %v", rerr)
		}
		if ierr := launch.WriteCreateIntentFor(sessionKey, selection.Root, selection.Name, o.Name, eff.Fingerprint); ierr != nil {
			return runFail(d, 1, "environment: %v", ierr)
		}
		if rerr := launch.RecordSessionEnvironment(sessionKey, launch.SessionEnvironment{
			Name: selection.Name, Root: selection.Root, SandboxName: o.Name, EffectivePath: eff.Path,
		}); rerr != nil {
			fmt.Fprintf(d.Err, "pix: warning: %v\n", rerr)
		}
	}

	// The interactive-root Hold, CREATE half: no instance exists yet, so the
	// positive receipt is awaited CONCURRENTLY with RunSession's own create,
	// below, rather than blocking it. The deferred cancel fires on every path
	// out of this function, which bounds the poll to this launch's own
	// lifetime even if RunSession itself never returns a usable receipt.
	var rootWait <-chan *interactiveRoot
	if creating {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch := make(chan *interactiveRoot, 1)
		go func() {
			r, herr := awaitInteractiveRootHold(ctx, defaultShellEnv(), sessionKey, o.Name, o.Workspace, selection.Name, o.Model)
			warnInteractiveRootFailure(d.Err, o.Name, herr)
			ch <- r
		}()
		rootWait = ch
	}
	spec := launch.SessionSpec{
		Key:               sessionKey,
		Name:              o.Name,
		EnvCreateArgs:     envCreateArgs,
		Workspace:         o.Workspace,
		Creating:          creating,
		Keep:              o.Keep,
		AttachArgs:        plan.Args,
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
		// Teardown routes through the environment-scoped planner (E2.4), so
		// the stable effective file this launch created is what removal names
		// — inside the SAME proof chain, never beside it.
		Teardown: launch.TeardownOptions{Planner: launch.EnvTeardownPlanner(o.Name)},
		Spawn: func(argv []string) *exec.Cmd {
			cmd := exec.Command("sbx", argv...)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			// No credential bearer: host MCP servers authenticate on the host, so the
			// sandbox never sees a token.
			cmd.Env = os.Environ()
			return cmd
		},
	}
	// CRITICAL (security re-review), SECOND check: bound to the SAME
	// trustSnap the first gate checked, immediately before the one call
	// that actually mutates sbx (RunSession: `sbx env create`/`sbx exec`).
	// checkDrift=true re-reads the environment directory ONE more time —
	// the one legitimate remaining disk read — but only to compare its
	// fingerprint against trustSnap.fingerprint (identity/digest, never a
	// substitute independent read): unchanged costs nothing here and
	// proceeds on the SAME acceptance the first gate already confirmed;
	// anything that changed since resolution (a symlink swap, a concurrent
	// edit) is refused outright, regardless of whether the new content
	// would itself be independently trusted (M1, security re-review: trust
	// TOCTOU).
	if terr := runTrustGate(d, trustSnap, true); terr != nil {
		releaseRoot(true)
		return terr
	}
	xerr := launch.RunSession(spec, deps)
	// The create-path Hold, awaited only now: RunSession has returned (created
	// and run to completion, or refused before anything started), so the
	// sandbox's fate is already decided and the poll above has nothing left to
	// wait for. A short bound, not the unbounded creating poll: by this point
	// either the sandbox positively appeared (the common case; the goroutine
	// already returned or is finishing its last probe) or RunSession itself
	// refused, in which case FindPositivelyIdentifiedRunning will keep saying
	// no and the deferred cancel above ends the wait.
	if rootWait != nil {
		select {
		case r := <-rootWait:
			root = r
		case <-time.After(5 * time.Second):
		}
	}
	releaseRoot(xerr != nil)
	if xerr != nil {
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
			// The pinned git kit now lives in the effective document's kit
			// list, composed by EnvExtraKits from the same inputs.
			if msg := launch.KitResolveFailureMsg(launch.PinnedGitKit(launch.EnvExtraKits(cfg, o, version))); msg != "" {
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
	// RunSession now owns the sandbox lifecycle from here.
	return nil
}
