// run_cmd.go — the argv seam for `pix run`, plus the two things the launch
// package deliberately does not know: the verb table (for the "did you mean"
// hint) and the real env.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"pix/host/cli"
	"pix/host/config"
	"pix/host/inference"
	"pix/host/launcher"
	"pix/host/mcp"
	"pix/host/readiness/axis"
	"pix/host/secret"
	"pix/host/service"
	"pix/host/workflow/doctor"
	"pix/host/workflow/launch"
	"pix/host/workflow/onboard"
	"pix/host/workflow/pack"
	"pix/host/workspace"
	"strings"
)

func init() {
	launch.IsKnownVerb = func(v string) bool { return knownVerbs[v] }
	launch.DefaultEnv = defaultShellEnv
}

// runRun implements bare `pix [DIR]` and `pix run ...`. It reads the
// config, resolves the run options (including a repo checkout for --dev),
// composes the sbx argv, and execs it with stdio inherited.
//
// The default path forwards NO credential bearer into the sandbox: Google
// Workspace now rides the sbx gateway as the host-side `gog` MCP server (the
// `slack` pattern), authed entirely on the host — there is nothing to inject.
func runRun(argv []string) {
	// `--task NAME` shorthand: equivalent to `pix task run NAME`, expanded
	// BEFORE ParseRunArgs sees it — task.Path/Resolve (L1) do the resolution;
	// this file only rewrites argv into the ordinary DIR + --name shape `run`
	// already understands, so no sandbox-lifecycle code is duplicated here.
	if expanded, matched, terr := expandTaskFlag(argv); matched {
		if terr != nil {
			fmt.Fprintf(os.Stderr, "pix run --task: %v\n", terr)
			os.Exit(1)
		}
		argv = expanded
	}

	var generatedKitDirs []string
	cleanupGeneratedKits := func() {
		if err := launch.CleanupGeneratedKitDirs(generatedKitDirs); err != nil {
			fmt.Fprintf(os.Stderr, "pix: warning: %v\n", err)
		}
	}
	defer cleanupGeneratedKits()
	exit := func(code int) {
		cleanupGeneratedKits()
		os.Exit(code)
	}

	o, err := launch.ParseRunArgs(argv)
	if err != nil {
		if err == cli.ErrHelpRequested {
			// -h/--help: usage to STDOUT, exit 0 (a help request, not an error).
			fmt.Print(runUsage)
			return
		}
		fmt.Fprintf(os.Stderr, "pix run: %v\n\n", err)
		fmt.Fprint(os.Stderr, runUsage)
		exit(2)
	}

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
				fmt.Fprintf(os.Stderr, "pix: run_intent %q did not resolve (%v); using pi's default model. Fix with `pix config set run_intent <intent>`.\n", strings.TrimSpace(cfg.RunIntent), rerr)
			} else if applied && o.Model != "" {
				fmt.Fprintf(os.Stderr, "pix: intent %q -> model %s\n", o.Intent, o.Model)
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
		m, rerr := axis.ResolveSessionModel(o.Intent)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "pix run: --intent %q: %v\n", o.Intent, rerr)
			exit(2)
		} else {
			o.Model = m
			fmt.Fprintf(os.Stderr, "pix: intent %q -> model %s\n", o.Intent, m)
		}
	}

	// Bare-minimum key bootstrap: a pi session needs at least one model provider
	// key. `run` otherwise stays out of the way (no onboarding, no nags), but with
	// NO usable key it can't launch — so auto-run the key flow (resolve your
	// 1Password refs into sbx; on a TTY, steer you to 1Password). When a key is
	// already present this is a cheap no-op. Then refuse ONLY when we can POSITIVELY
	// confirm no key (tri-state): sbx absent OR its control plane unprobeable =
	// can't verify = proceed, never a false refusal on a transient failure.
	//
	// The probe result is KEPT (keyEvidence) and handed to the readiness
	// snapshot further down, so rendering readiness costs `run` no second
	// `sbx secret ls`.
	var keyEvidence axis.SbxKeyEvidence
	if _, err := defaultShellEnv().LookPath("sbx"); err == nil && !inference.ConfiguredKeylessInference() {
		env := defaultShellEnv()
		launch.BootstrapProviderKeys(env, os.Stdin, os.Stderr, cli.IsTTY(os.Stdin))
		keyEvidence = axis.ProbeSbxKeyEvidence(env)
		if keyEvidence.Ok() && !axis.AnyModelKeyInOutput(keyEvidence.Out) {
			fmt.Fprint(os.Stderr, secret.ModelKeyMissingMessage(env))
			exit(1)
		}
	}

	// Reconcile any control-plane proposal a prior in-session onboarding wrote
	// (<workspace>/.pix/onboarding.json): validate it, show the diff, apply
	// under a [Y/n] gate, register newly-enabled MCP servers, delete the file. This
	// runs BEFORE workspace.LoadResolvedConfig so a fresh create picks up the applied config.
	// Best-effort and non-blocking on a non-TTY (it just leaves the file).
	onboard.ReconcileOnboarding(o.Workspace, defaultShellEnv(), os.Stdin, os.Stdout, false, cli.IsTTY(os.Stdin), onboardDeps())

	// Load the config for the rest of run (kits, mcp, gog, pack). The
	cfg, _, err := workspace.LoadResolvedConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix run: %v\n", err)
		exit(1)
	}
	if !inference.AllowsModel(cfg, o.Model) {
		fmt.Fprintf(os.Stderr, "pix run: model %q is not available through the configured inference backends\n", o.Model)
		exit(2)
	}

	// Own the sandbox name so we can manage its lifecycle. sbx would otherwise
	// auto-derive `pix-<dir>`. Resolved (and the sandbox state probed) BEFORE
	// any create-only input resolution below — a plain re-attach must never fail on
	// a --dev/checkout or --kit problem it doesn't even need.
	if o.Name == "" {
		o.Name = workspace.DeriveSandboxName(o.Workspace)
	}
	state := launch.ProbeTaskSandbox(defaultShellEnv(), o.Name)

	// Mirror sbx's own model: an existing sandbox (running OR stopped) RE-ATTACHES
	// instead of refusing/recreating — the create-only flags (--kit/--template/
	// --mcp/config-stacked-kits/--dev/dev-skills) only apply to a fresh create, so they are
	// simply not sent (and, per launch.WillCreate below, not even RESOLVED) on re-attach.
	// --replace forces the old implicit-recreate behavior (rm -f then create) for
	// either state, so changed kit/mcp/create-only flags take effect.
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
				fmt.Fprintln(os.Stderr, msg)
			}
		}

		if o.Dev {
			// --dev needs a resolvable repo checkout; fail loud otherwise. --dev is
			// create/replace-only (this branch), so it is a no-op on a plain re-attach.
			root, err := launch.ResolveRepoRoot()
			if err != nil {
				fmt.Fprintf(os.Stderr, "pix run --dev: %v\n", err)
				exit(1)
			}
			o.DevRoot = root
			o.LocalKit = filepath.Join(root, "pi-kit")
			o.LocalImageTag = launch.ReadLocalImageTag(root)
		} else if !released && !kitOverride {
			if root, err := launch.ResolveRepoRoot(); err == nil {
				o.LocalKit = filepath.Join(root, "pi-kit")
				o.LocalImageTag = launch.ReadLocalImageTag(root)
				note := ""
				if o.LocalImageTag != "" {
					note = " (local image :" + o.LocalImageTag + ")"
				}
				fmt.Fprintf(os.Stderr, "pix: unreleased build %q — using local checkout kit %s%s\n", version, o.LocalKit, note)
			} else {
				fmt.Fprintf(os.Stderr, "pix: unreleased build %q and no pix checkout found — "+
					"kit tracks #ref=main (may not match this binary). Use `pix run --dev` from a "+
					"checkout or `pix run --kit <path-or-git-url>` to override.\n", version)
			}
		}
	} else if o.Dev {
		fmt.Fprintln(os.Stderr, "pix: --dev is create/replace-only; re-attaching to the existing sandbox as-is (use --replace to recreate with --dev)")
	}

	// Active pack: mount its skills/ + knowledge/ so the pack's context loads in
	// this sandbox. --pack overrides config.Pack; with neither set, no pack is
	// active. Create-time only (skills + knowledge are create-time mounts; a
	// re-attach keeps what it was made with).
	// effectivePack is the pack root that ACTUALLY loaded and applied, as
	// opposed to pack.ActivePackRoot(cfg.Pack, o.Pack) (the merely CONFIGURED one).
	// It defaults to the configured root for a re-attach (no launch.ApplyPackToLaunch
	// call — launch.WritePackContextFiles below still attempts its own load and
	// degrades to unscoped on failure), and is overwritten with
	// launch.ApplyPackToLaunch's honest return on a create, where it becomes "" if
	// the pack degraded via pack.ErrNotAPack. This is what keeps the sandbox.pack
	// marker and memory scope from disagreeing about what actually loaded.
	effectivePack := pack.ActivePackRoot(cfg.Pack, o.Pack)
	if launch.WillCreate(state, o.Replace) {
		// Fatal on error (explicit --pack that doesn't load, or a declared
		// sandbox proxy whose kit can't be built — round-4 F2 fail-closed):
		// never create a sandbox missing context the pack declared.
		root, err := launch.ApplyPackStackToLaunch(cfg, &o, defaultShellEnv())
		if err != nil {
			fmt.Fprintf(os.Stderr, "pix: %v\n", err)
			exit(1)
		}
		effectivePack = root
		// Inference is a generated create-time facet just like pack wrappers: the
		// sandbox receives only probed models, compiled routes, and public endpoint
		// metadata. Credential values never enter this kit.
		inferenceKit, ierr := inference.SynthesizeInferenceKit(cfg)
		if ierr != nil {
			fmt.Fprintf(os.Stderr, "pix: inference: %v\n", ierr)
			exit(1)
		}
		if inferenceKit != "" {
			o.PackKits = append(o.PackKits, inferenceKit)
			generatedKitDirs = append(generatedKitDirs, inferenceKit)
		}
		o.Models, ierr = inference.CallableRuntimeModels(cfg)
		if ierr != nil {
			fmt.Fprintf(os.Stderr, "pix: inference models: %v\n", ierr)
			exit(1)
		}
		contextKit, cerr := launch.SynthesizePersonalContextKit()
		if cerr != nil {
			fmt.Fprintf(os.Stderr, "pix: personal context: %v\n", cerr)
			exit(1)
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
			fmt.Fprintf(os.Stderr, "pix: local image %s:%s is not loaded in sbx.\n", launch.DockerImageRepo, o.LocalImageTag)
			fmt.Fprintln(os.Stderr, "It's a local build (never published), so sbx would try to pull it and stall on a prompt.")
			fmt.Fprintln(os.Stderr, "Load this build into sbx first, from your pix checkout:")
			fmt.Fprintln(os.Stderr, "  make load")
			exit(1)
		}
	}

	// Same preflight for an explicit --template that names a local-* build: it's
	// never published, so an unloaded ref would make sbx stall on a pull prompt.
	// Only local-* tags get this guard — a published ref is legitimately pullable.
	if launch.WillCreate(state, o.Replace) && o.Template != "" {
		if tag := launch.TemplateTag(o.Template); strings.HasPrefix(tag, "local-") && !launch.LocalImageLoaded(defaultShellEnv(), tag) {
			fmt.Fprintf(os.Stderr, "pix: --template %s is not loaded in sbx.\n", o.Template)
			fmt.Fprintln(os.Stderr, "It's a local build (never published), so sbx would try to pull it and stall on a prompt.")
			fmt.Fprintln(os.Stderr, "Load it first, from the checkout that built it:  make load")
			exit(1)
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
		fmt.Fprintf(os.Stderr, "pix run: %v\n", plan.Err)
		exit(1)
	}
	if !plan.Reattach {
		if err := launch.ValidateCreateKits(plan.Args, launch.ValidateSbxKit); err != nil {
			fmt.Fprintf(os.Stderr, "pix run: %v\n", err)
			exit(1)
		}
	}
	switch {
	case o.Replace:
		fmt.Fprintf(os.Stderr, "pix run: replacing sandbox %q\n", o.Name)
	case plan.Reattach && state == launch.SbxRunning:
		fmt.Fprintf(os.Stderr, "pix run: re-attaching to running sandbox %q\n", o.Name)
	case plan.Reattach:
		fmt.Fprintf(os.Stderr, "pix run: starting + attaching existing sandbox %q (use --replace to recreate with current kit/mcp/flags)\n", o.Name)
	}
	// finding #8: a reattach is honest about live-vs-recreate for MOST facets
	// above, but says nothing about a pack switched since this sandbox was
	// created — its mcp/bin/skills are create-only and won't attach without
	// --replace. Surface that explicitly rather than silently reattaching to a
	// stale facet set.
	if msg := launch.StalePackReattachWarning(cfg, o, plan.Reattach); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
	}
	// Product gap #2: reattach honesty. Separate from launch.StalePackReattachWarning
	// (which only speaks to skills/bin drift from a pack switch). This checks
	// MCP attachment PRECISELY, via the launcher's own receipt, regardless of
	// WHY a desired server might not be attached (config change, pack change,
	// explicit --mcp, or no receipt at all). Never auto-loads, only reports.
	if msg := launch.McpReattachWarning(cfg, o, plan.Reattach); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
	}
	// Lazy auto-start: make the configured host services (memory/knowledge)
	// reachable before the sandbox tries them, with a SHORT budget — the launch
	// waits AT MOST service.EnsureRunTimeout (8s), covering spawn-lock acquisition
	// AND the health poll under one deadline (M2), then proceeds regardless
	// (recall/knowledge degrade in-VM exactly as before). service.Ensure prints its
	// own progress/failure lines.
	service.EnsureUp(nil, service.EnsureRunTimeout)

	// Readiness, rendered from the SHARED lazy snapshot (readiness_launch.go)
	// and reusing the key evidence the launch gate above already paid for. Two
	// rules, both deliberate:
	//   - AT MOST axis.LaunchWarningLimit rows, then a single count pointing at
	//     doctor. `run` is the daily command; a wall of readiness text here is
	//     how a user learns to skip readiness output entirely.
	//   - It NEVER blocks. The only launch-stopping condition is the missing
	//     provider key handled above, because that is the only gap that makes
	//     the session useless rather than degraded.
	axis.RenderReadinessWarnings(os.Stderr, axis.FastReadinessSnapshot(cfg, defaultShellEnv(), keyEvidence), axis.LaunchWarningLimit)

	// Local model + memory scope: hand the configured ollama_bridge_model to the
	// in-VM ollama-bridge, and the active pack's memory_scope (default: the pack
	// name; "default" is the shared/unscoped tag) to the in-VM recall/capture
	// extensions, via per-run workspace files. No active pack -> pack.WriteMemoryScope
	// removes any stale file (unscoped recall). Best-effort throughout: an
	// unloadable pack degrades to unscoped rather than failing run. Shared with
	// `task new` via launch.WritePackContextFiles (pack.go) so both launch paths write
	// the SAME pack context.
	launch.WritePackContextFiles(cfg, o, effectivePack)

	// Trusted host state: the host-visible facts the fenced agent can't see for
	// itself (keys/services/knowledge/gog/mcp/models/pack/identity). This
	// travels ONLY inside the launcher-generated initial prompt (the pi
	// passthrough arg carrying launch.GeneratedInputMarker, e.g. setup.OnboardingKickoff) —
	// never as a workspace file, which a cloned repo could plant or leave stale.
	// launch.InjectTrustedHostState is a no-op (and probes nothing) when no such arg is
	// present, so a normal run never pays for or produces onboarding truth.
	//
	// The --pack override only takes effect on a CREATE (packs mount at creation);
	// on a re-attach the sandbox keeps whatever pack it was made with, so don't
	// claim the override in the payload — fall back to the persisted active pack.
	packForState := ""
	if launch.WillCreate(state, o.Replace) {
		packForState = o.Pack
	}
	// This is a HARD contract, not best-effort: when a generated prompt IS
	// present, it is the fenced in-VM agent's ONLY source of trusted host-visible
	// truth, so a launch that can't build/encode it must ABORT before exec'ing
	// sbx rather than hand the agent a generated prompt with no trusted payload.
	// Destructive replacement is deliberately last: all fallible, read-only
	// kit/readiness/trusted-state preflights above must succeed before the old
	// sandbox is removed. A failed generated onboarding payload must never leave
	// the user with neither the old sandbox nor a replacement.
	var args []string
	err = launch.PreflightBeforeReplace(func() error {
		var preflightErr error
		args, preflightErr = launch.InjectTrustedHostState(plan.Args, cfg, defaultShellEnv(), packForState)
		if preflightErr != nil {
			return fmt.Errorf("could not build trusted host state: %w", preflightErr)
		}
		return nil
	}, func() error {
		return launch.ApplyReplaceRm(defaultShellEnv(), plan, o.Name)
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix run: %v\n", err)
		exit(1)
	}
	// Record the pack only after every hard-fail preflight and any replacement
	// removal succeeded. A failed trusted-state preflight must leave both the old
	// sandbox and its create-time marker untouched.
	if launch.DefinitelyCreating(state, o.Replace) {
		launch.WriteSandboxPackMarker(o.Workspace, effectivePack)
	}

	if os.Getenv("PIX_DEBUG") != "" {
		fmt.Fprintln(os.Stderr, "+ sbx "+strings.Join(args, " "))
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
	if err := launch.ExecSbxRunAndRecordCreate(cmd, launch.DefinitelyCreating(state, o.Replace), o.Name, workspace.CanonicalPath(o.Workspace), o.StaticMCP); err != nil {
		var rerr *workspace.ReceiptRecordError
		if errors.As(err, &rerr) {
			// The sandbox itself WAS created successfully — only the local
			// receipt failed. Say so honestly rather than implying the launch
			// failed, but still exit non-zero: doctor/status must not be told
			// this sandbox's MCP set is recorded when it isn't.
			fmt.Fprintf(os.Stderr, "pix run: %v\n", rerr)
			fmt.Fprintln(os.Stderr, "the sandbox itself launched fine; only pix's local record of its preloaded MCP set failed to write. check state-dir permissions and re-run `pix doctor`.")
			exit(1)
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			// If we pinned a git #ref kit and sbx bailed (classically git exit 128
			// "Remote branch not found"), the raw error is opaque — replace it with
			// an actionable note instead of leaking the git 128.
			if msg := launch.KitResolveFailureMsg(launch.PinnedGitKit(args)); msg != "" {
				fmt.Fprintln(os.Stderr, msg)
			}
			// A re-attach exec can fail on an sbx version that won't reattach a
			// kit-created sandbox; don't leave the user stuck without a next step.
			if plan.Reattach {
				fmt.Fprintf(os.Stderr, "pix run: re-attach failed; recreate it with: %s\n", launch.RunReplaceCommand(o.Workspace))
			}
			exit(exitErr.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "pix run: exec sbx: %v\n", err)
		if errors.Is(err, exec.ErrNotFound) {
			fmt.Fprintln(os.Stderr, "install sbx with: "+doctor.SbxInstallHint)
		}
		if plan.Reattach {
			fmt.Fprintf(os.Stderr, "pix run: re-attach failed; recreate it with: %s\n", launch.RunReplaceCommand(o.Workspace))
		}
		exit(1)
	}
}
