// setup_cmd.go — the argv seam for `pix setup`, plus the one thing that is
// deliberately NOT part of the provision loop: the agent handoff.
//
// The handoff lives here, at L4, because it is not a capability that can be
// checked, applied and checked again — it is an exec into another command, and
// its whole decision matrix is about a sandbox that may already be alive. A
// step that cannot be re-probed does not belong in a loop whose contract is
// that the second check is authoritative.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"pix/host/cli"
	"pix/host/config"
	"pix/host/sys"
	"pix/host/workflow/doctor"
	"pix/host/workflow/launch"
	"pix/host/workflow/onboard"
	"pix/host/workflow/pack"
	"pix/host/workflow/provision"
	"slices"
)

// runSetupCmd is the `pix setup` entry: parse, run the host provisioning loop,
// then (unless --no-agent) hand off to the sandbox.
func runSetupCmd(argv []string) {
	if cli.WantsHelp(argv) {
		fmt.Print(provision.Usage)
		return
	}

	// Split an optional positional DIR from the onboard-style flags. DIR is the
	// single non-flag token; everything else is forwarded to the host phase.
	// --replace, --verbose and --no-agent are SETUP'S OWN flags: consumed here,
	// never forwarded to the host-config parser.
	dir := "."
	dirSet := false
	replace := false
	noAgent := false
	verbose := false
	var hostArgs []string
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch a {
		case "--replace":
			replace = true
			continue
		case "--verbose":
			verbose = true
			continue
		case "--no-agent":
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
		if provision.FlagTakesValue(a) && i+1 < len(argv) {
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
		fmt.Fprintf(os.Stderr, "pix setup: %v\n\n%s", parseErr, provision.Usage)
		os.Exit(2)
	}
	// Load + validate every built-in semantic flag/value before pack adoption or
	// any other mutation. The host phase repeats the same pure validator for
	// direct/test callers.
	preflightCfg, cfgErr := config.Load()
	if cfgErr != nil {
		fmt.Fprintf(os.Stderr, "pix setup: loading config: %v\n", cfgErr)
		os.Exit(1)
	}
	if err := provision.ValidateSetupSemantics(parsed, preflightCfg, env, hostBinaryResolver); err != nil {
		fmt.Fprintf(os.Stderr, "pix setup: %v\n\n%s", err, provision.Usage)
		os.Exit(2)
	}

	// `--apply` reconciles a pending <DIR>/.pix/onboarding.json (the
	// control-plane proposal an in-sandbox onboarding agent wrote) and stops. It
	// is deliberately NOT provisioning — it applies a proposal the user already
	// reviewed — so it validates DIR, reconciles, and returns without touching
	// packs, models, or the sandbox.
	if slices.Contains(hostArgs, "--apply") {
		if err := launch.ValidateRunWorkspace(dir); err != nil {
			fmt.Fprintf(os.Stderr, "pix setup: %v\n", err)
			os.Exit(2)
		}
		onboard.ReconcileOnboarding(dir, env, os.Stdin, os.Stdout, parsed.AssumeYes, cli.IsTTY(os.Stdin), onboardDeps())
		return
	}
	// DIR must be validated (exists AND is a directory) BEFORE the host phase
	// runs: provisioning mutates real host state, and a typo'd DIR must fail
	// with nothing touched rather than be caught only at the handoff.
	if err := launch.ValidateRunWorkspace(dir); err != nil {
		fmt.Fprintf(os.Stderr, "pix setup: %v\n", err)
		os.Exit(2)
	}
	if err := provision.EnsureSetupSbxSession(env, os.Stdout, cli.IsTTY(os.Stdin) && !parsed.AssumeYes); err != nil {
		fmt.Fprintf(os.Stderr, "pix setup: %v\n", err)
		os.Exit(1)
	}
	// Pix's published base kit comes from GitHub, while a fresh sbx install only
	// trusts docker.io kit sources. Fill that one publisher allowlist entry and
	// initialize the one-time global network policy before the first handoff.
	if err := provision.EnsureSetupSbxDefaults(env); err != nil {
		fmt.Fprintf(os.Stderr, "pix setup: %v\n", err)
		os.Exit(1)
	}
	// An unreleased launcher uses its local checkout kit. Validate that kit with
	// the installed sbx parser before pack OAuth or any other mutation; nightly
	// schema skew must fail once, early.
	if err := launch.ValidateSetupKit(version, launch.ResolveRepoRoot, launch.ValidateSbxKit); err != nil {
		fmt.Fprintf(os.Stderr, "pix setup: %v\n", err)
		os.Exit(1)
	}

	// The host phase: check, apply the verified gaps, check again.
	if err := provision.RunSetup(env, hostArgs, os.Stdin, os.Stdout, cli.IsTTY(os.Stdin)); err != nil {
		fmt.Fprintf(os.Stderr, "pix setup: %v\n", err)
		var usage provision.ErrUsage
		if errors.As(err, &usage) {
			os.Exit(2) // an argument mistake, caught before any probe or mutation
		}
		os.Exit(1)
	}
	// --no-agent stops here: the host phase is the whole command.
	if noAgent {
		return
	}

	// Handoff decision: probe the sandbox for dir and branch on the POSITIVE
	// state. Existing without --replace is left alone — setup never
	// force-removes it and never replays the onboarding kickoff into a live
	// session. Existing WITH --replace relaunches through `run --replace`
	// carrying the kickoff. Only a POSITIVE launch.SbxAbsent gets the normal
	// first handoff; an unprobeable sbx FAILS CLOSED.
	name, nameOK := provision.SetupSandboxName(dir)
	state := launch.SbxUnknown
	if nameOK && name != "" {
		state = launch.ProbeTaskSandbox(env, name)
	}
	if err := runSetupHandoff(dir, name, state, replace, os.Stdout, runRun); err != nil {
		fmt.Fprintf(os.Stderr, "pix setup: %v\n", err)
		os.Exit(1)
	}
}

// runSetupHandoff is the pure post-host-phase decision + action, kept separate
// from runSetupCmd so the state/replace matrix is testable without exercising
// os.Exit or actually exec'ing sbx. Returns an error ONLY for the fail-closed
// unknown state.
func runSetupHandoff(dir, name string, state doctor.SbxState, replace bool, out io.Writer, runFn func([]string)) error {
	// kickoffArgs builds the runRun argv for a launch that should receive the
	// tour: [DIR] [--replace] -- <OnboardingKickoff>. DIR is forwarded only when
	// explicit so `pix setup` from inside a repo behaves exactly like `pix run`
	// there.
	kickoffArgs := func() []string {
		args := []string{}
		if dir != "." {
			args = append(args, dir)
		}
		if replace {
			args = append(args, "--replace")
		}
		return append(args, "--", provision.OnboardingKickoff)
	}
	dirArg := ""
	if dir != "." {
		dirArg = " " + sys.ShellQuote(dir)
	}
	// retryArg carries the caller's ORIGINAL --replace request into the retry
	// command printed below: dropping it would silently downgrade a requested
	// recreate into a plain reattach.
	retryArg := dirArg
	if replace {
		retryArg += " --replace"
	}

	switch state {
	case launch.SbxUnknown:
		// FAIL CLOSED: we could not determine whether a sandbox exists. Never
		// launch — runRun would re-attach a live session and replay the kickoff
		// into it. The host phase already completed, so a retry is cheap.
		which := fmt.Sprintf("sandbox %q", name)
		if name == "" {
			which = fmt.Sprintf("the sandbox for %s", dir)
		}
		return fmt.Errorf("cannot determine the state of %s (`sbx ls` failed or sbx is unavailable). Host setup completed; install or fix sbx and retry with: pix setup%s", which, retryArg)
	case launch.SbxRunning, launch.SbxStopped:
		if replace {
			fmt.Fprintln(out, "")
			fmt.Fprintf(out, "Recreating sandbox %q (--replace): it'll come back with your current\n", name)
			fmt.Fprintln(out, "pack/MCP/skills and walk you through the guided tour.")
			runFn(kickoffArgs())
			return nil
		}
		fmt.Fprintln(out, "")
		fmt.Fprintf(out, "Host configuration reconciled. Existing sandbox %q was left alone.\n", name)
		fmt.Fprintln(out, "Reattaching keeps the sandbox exactly as it was created (its pack, MCP")
		fmt.Fprintln(out, "servers, and skills were attached at create time); recreating applies the")
		fmt.Fprintln(out, "current ones. Choose one:")
		fmt.Fprintf(out, "  pix run%s              # reattach as-is\n", dirArg)
		fmt.Fprintf(out, "  pix setup%s --replace  # recreate with current settings + get the tour\n", dirArg)
		return nil
	}

	// launch.SbxAbsent (positively confirmed): normal first launch.
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Launching Pix — the agent will take it from here.")
	runFn(kickoffArgs())
	return nil
}

// init supplies the composition provisioning declares but cannot perform.
func init() {
	provision.DefaultEnv = defaultShellEnv
	provision.HostBinary = hostBinaryResolver
	provision.Register = pack.RegisterFn(registerServers)
}
