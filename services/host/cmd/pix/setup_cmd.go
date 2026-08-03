// setup_cmd.go — the argv seam for `pix setup`, plus the composition setup
// cannot do for itself. setup sequences nearly every other workflow, so this is
// the widest Deps in the tree; that is what a top-level guided flow looks like.
package main

import (
	"errors"
	"fmt"
	"os"
	"pix/host/cli"
	"pix/host/config"
	"pix/host/workflow/launch"
	"pix/host/workflow/onboard"
	"pix/host/workflow/pack"
	"pix/host/workflow/setup"
	"slices"
	"strings"
)

// runSetupCmd is the `pix setup` entry. It accepts the same host-config
// flags as `onboard` plus an optional DIR (default "."), runs the host phase,
// prints the handoff, then execs the run with the onboarding kickoff message.
func runSetupCmd(argv []string) {
	if cli.WantsHelp(argv) {
		fmt.Print(setup.Usage)
		return
	}

	// Split an optional positional DIR from the onboard-style flags. DIR is the
	// single non-flag token; everything else is forwarded to the host phase.
	// --replace is SETUP'S OWN flag (recreate an existing sandbox and hand it
	// the tour): consumed here, never forwarded to onboard.ParseOnboardArgs — it is not
	// host config.
	dir := "."
	dirSet := false
	replace := false
	noAgent := false
	verbose := false
	var hostArgs []string
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--replace" {
			replace = true
			continue
		}
		if a == "--verbose" {
			verbose = true
			continue
		}
		// --no-agent is SETUP'S OWN flag (AC-P0-308): run the host phase and
		// stop there — no sandbox, no handoff. It replaces the deleted
		// `pix onboard` verb, so it is consumed here and never forwarded
		// to the host-config parser.
		if a == "--no-agent" {
			noAgent = true
			continue
		}
		if len(a) > 0 && a[0] != '-' {
			if dirSet {
				fmt.Fprintf(os.Stderr, "pix setup: too many directories (%q and %q); pass at most one DIR\n", dir, a)
				os.Exit(2)
			}
			dir, dirSet = a, true
			continue
		}
		hostArgs = append(hostArgs, a)
		if setup.FlagTakesValue(a) && i+1 < len(argv) {
			i++
			hostArgs = append(hostArgs, argv[i])
		}
	}

	env := defaultShellEnv()
	env.Quiet = !verbose
	if verbose {
		_ = os.Setenv("PIX_SETUP_VERBOSE", "1")
	}
	parsed, parseErr := onboard.ParseOnboardArgs(hostArgs)
	if parseErr != nil {
		fmt.Fprintf(os.Stderr, "pix setup: %v\n\n%s", parseErr, setup.Usage)
		os.Exit(2)
	}
	// Load + validate every built-in semantic flag/value before prerequisites,
	// pack adoption, setup hooks, or browser-capable authorization. The later
	// host phase repeats the same pure validator for direct/test callers.
	preflightCfg, cfgErr := config.Load()
	if cfgErr != nil {
		fmt.Fprintf(os.Stderr, "pix setup: loading config: %v\n", cfgErr)
		os.Exit(1)
	}
	if err := setup.ValidateSetupSemantics(parsed, preflightCfg, env, hostBinaryResolver); err != nil {
		fmt.Fprintf(os.Stderr, "pix setup: %v\n\n%s", err, setup.Usage)
		os.Exit(2)
	}

	// `--apply` is the surviving half of the deleted `pix onboard`: reconcile
	// a pending <DIR>/.pix/onboarding.json (the control-plane proposal an
	// in-sandbox onboarding agent wrote) and stop. It is deliberately NOT part of
	// the phase machine — it applies a proposal the user already reviewed rather
	// than provisioning a host — so it validates DIR, reconciles, and returns
	// without touching keys, packs, or the sandbox.
	if slices.Contains(hostArgs, "--apply") {
		if err := launch.ValidateRunWorkspace(dir); err != nil {
			fmt.Fprintf(os.Stderr, "pix setup: %v\n", err)
			os.Exit(2)
		}
		opts, perr := onboard.ParseOnboardArgs(hostArgs)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "pix setup: %v\n\n%s", perr, setup.Usage)
			os.Exit(2)
		}
		onboard.ReconcileOnboarding(dir, env, os.Stdin, os.Stdout, opts.AssumeYes, cli.IsTTY(os.Stdin), onboardDeps())
		return
	}
	if err := launch.ValidateRunWorkspace(dir); err != nil {
		fmt.Fprintf(os.Stderr, "pix setup: %v\n", err)
		os.Exit(2)
	}
	// sbx is universally required. 1Password is conditional and is decided only
	// AFTER explicit packs have contributed inference; a keyless work gateway
	// must never trigger an irrelevant op installation/login flow.
	if err := setup.EnsureSetupPrereqsFor(env, os.Stdin, os.Stdout, cli.IsTTY(os.Stdin) && !parsed.AssumeYes, false); err != nil {
		fmt.Fprintf(os.Stderr, "pix setup: %v\n", err)
		os.Exit(1)
	}
	if err := setup.EnsureSetupSbxSession(env, os.Stdout, cli.IsTTY(os.Stdin) && !parsed.AssumeYes); err != nil {
		fmt.Fprintf(os.Stderr, "pix setup: %v\n", err)
		os.Exit(1)
	}
	// Pix's published base kit comes from GitHub, while a fresh sbx install only
	// trusts docker.io kit sources. Fill that one publisher allowlist entry and
	// initialize the one-time global network policy before the first handoff.
	// Existing publishers and an existing (possibly tightened) policy are kept.
	if err := setup.EnsureSetupSbxDefaults(env); err != nil {
		fmt.Fprintf(os.Stderr, "pix setup: %v\n", err)
		os.Exit(1)
	}
	// An unreleased launcher uses its local checkout kit. Validate that kit with
	// the installed sbx parser before pack OAuth/setup or any other mutation;
	// nightly schema skew must fail once, early, without opening browsers and
	// only later dumping YAML from `sbx run`.
	if err := launch.ValidateSetupKit(version, launch.ResolveRepoRoot, launch.ValidateSbxKit); err != nil {
		fmt.Fprintf(os.Stderr, "pix setup: %v\n", err)
		os.Exit(1)
	}

	// A requested pack is adopted through the existing pack trust transaction
	// before host setup. This gives `pix setup --pack owner/repo` one setup
	// engine while preserving the exact same BoM review, fingerprint, and
	// rollback behavior as `pix pack use`.
	var activatedPacks []string
	if len(parsed.Packs) > 0 && env.Quiet {
		fmt.Fprintln(os.Stdout, "Configuring pack integrations…")
	}
	for _, requestedPack := range parsed.Packs {
		packArg := setup.NormalizeSetupPackArg(requestedPack)
		useArgs := []string{packArg}
		if parsed.AssumeYes {
			useArgs = append([]string{"--yes"}, useArgs...)
		}
		pack.RunPackUse(env, os.Stdout, useArgs, registerServers)
		if cfg, err := config.Load(); err == nil && strings.TrimSpace(cfg.Pack) != "" {
			activatedPacks = append(activatedPacks, cfg.Pack)
		}
	}
	activatedPacks = pack.UniquePackRoots(activatedPacks)
	if len(activatedPacks) > 0 {
		if err := pack.PersistPackStack(activatedPacks); err != nil {
			fmt.Fprintf(os.Stderr, "pix setup: composing packs: %v\n", err)
			os.Exit(1)
		}
	}
	// A pack's required setup owns its interactive authorization flows. Run it
	// before the ordinary host gate: that gate verifies configured MCP servers,
	// so placing hooks afterward made it impossible for a fresh pack to satisfy
	// the very prerequisites the gate checked (and skipped integration
	// entirely on the first missing remote registration).
	setupRequests, err := pack.PlanPackSetupRequests(activatedPacks, parsed.WithSetup)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix setup: %v\n", err)
		os.Exit(1)
	}
	for _, root := range activatedPacks {
		if err := pack.RunPackSetup(env, os.Stdout, root, setupRequests[root], cli.IsTTY(os.Stdin) && !parsed.AssumeYes); err != nil {
			fmt.Fprintf(os.Stderr, "pix setup: %v\n", err)
			os.Exit(1)
		}
	}

	// Phase 1: host config — source keys from 1Password, ensure memory, create the
	// pack, seed identity, provision+enable host mode (see setup.SetupHostPhase). This
	// ALWAYS runs first, regardless of whether a sandbox already exists for dir —
	// `pix setup` run a second time must still reconcile host keys/config
	// (a changed/rotated 1Password ref, a newly-added --mcp, etc), not skip
	// straight past it because a sandbox happens to be there.
	//
	// DIR must be validated (exists AND is a directory) BEFORE any of that runs —
	// setup.SetupHostPhase mutates real host state (op-refs.env/hostmode.env, config.toml,
	// the default pack, memory, host-mode enablement). A typo'd or nonexistent DIR
	// must fail immediately, with nothing touched, not be caught only later when the
	// (already-mutated) phase 2 sandbox probe/handoff can't resolve it. setup.RunSetupCore
	// is the seam: it does the validation-then-hostPhase-call as one pure step so a
	// test can assert hostPhase is never invoked for a bad DIR without exercising
	// os.Exit.
	if env.Quiet {
		fmt.Fprintln(os.Stdout, "Setting up inference and host services…")
	}
	if err := setup.RunSetupCore(env, dir, hostArgs, os.Stdin, os.Stdout, cli.IsTTY(os.Stdin), setup.SetupHostPhase); err != nil {
		fmt.Fprintf(os.Stderr, "pix setup: %v\n", err)
		var usage setup.ErrUsage
		if errors.As(err, &usage) {
			os.Exit(2) // an argument mistake, caught before any probe or mutation
		}
		os.Exit(1)
	}
	// --no-agent stops here: the host phase is the whole command. The phase
	// header is still printed so the transcript is complete and a reader can
	// see that the handoff was skipped by request, not silently dropped.
	if noAgent {
		setup.SetupPhaseHeader(os.Stdout, setup.SetupPhaseHandoff, "skipped (--no-agent): host phase only, no sandbox")
		return
	}

	setup.SetupPhaseHeader(os.Stdout, setup.SetupPhaseHandoff, "")

	// Phase 2 decision: probe the sandbox for dir and branch on the POSITIVE
	// state. Existing without --replace is left alone — setup never
	// force-removes it and never replays the onboarding kickoff into a live
	// session (the fenced agent inside it may be mid-task). Existing WITH
	// --replace relaunches through `run --replace` carrying the kickoff, so
	// the recreated sandbox actually receives the tour. Only a POSITIVE
	// launch.SbxAbsent gets the normal first handoff; an unprobeable sbx (launch.SbxUnknown,
	// or an unresolvable name) FAILS CLOSED — launching blind could replay the
	// kickoff into a live session we simply couldn't see.
	name, nameOK := setup.SetupSandboxName(dir)
	state := launch.SbxUnknown
	if nameOK && name != "" {
		state = launch.ProbeTaskSandbox(env, name)
	}
	if err := setup.RunSetupHandoff(dir, name, state, replace, os.Stdout, runRun); err != nil {
		fmt.Fprintf(os.Stderr, "pix setup: %v\n", err)
		os.Exit(1)
	}
}

// init supplies the composition setup declares but cannot perform. This is the
// whole cost of setup being a workflow instead of living at L4, and it is four
// lines.
func init() {
	setup.DefaultEnv = defaultShellEnv
	setup.HostBinary = hostBinaryResolver
	setup.Register = registerServers
	setup.Credentials = mcpCredentials
}
