// setup.go implements `pix setup` — the explicit, guided onboarding entry.
//
// Owner decision (supersedes the in-`run` auto-offer): onboarding is a TWO-PHASE
// thing the user opts into by NAME.
//
//  1. HOST phase (here, on the host): configure callable inference through
//     direct 1Password-backed APIs, a gateway, Ollama, or pack-provided
//     bindings; enable memory only when its local models are verified; and
//     seed first-name identity.
//     Host mode is NOT set up here — it's opt-in via `pix host setup`.
//     Host-config (gog/knowledge/mcp) comes from FLAGS, not interactive prompts;
//     Flag/non-TTY operation is CI-safe.
//  2. AGENT phase (handoff): launch a normal `pix run` whose FIRST pi
//     message kicks off the `onboarding` skill, so the agent PROACTIVELY starts
//     the conversation (identity, tone, a real first task) instead of sitting
//     silent — the passive system-prompt marker never spoke until the user
//     typed, which is the bug this replaces.
//
// `pix run` on its own NEVER onboards. `pix setup --no-agent` is the host-only,
// no-handoff path for CI.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"pix/host/cli"
	"pix/host/hostenv"
	"pix/host/readiness"
	"pix/host/readiness/axis"
	"pix/host/secret"
	"pix/host/sys"
	"pix/host/workflow/onboard"
	"pix/host/workflow/pack"
	"slices"
	"strings"

	"pix/host/config"
	"pix/host/inference"
	"pix/host/rpc"
	"pix/host/workspace"
)

// generatedInputMarker prefixes any user-role message that `pix` itself
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
const generatedInputMarker = "[pix-generated:onboarding] "

// onboardingKickoff is the first message `setup` hands the agent. It is
// DELIBERATELY short and human — it reads like something the user would type,
// not a machine directive wall. The rewritten `onboarding` skill owns the actual
// flow (guided teach, read host-state, land a task); the word "guided" is all it
// needs to pick GUIDED mode. (Making this fully invisible — agent greets with no
// visible prompt at all — needs a session-start extension + an image rebuild;
// tracked as a follow-up.) It carries generatedInputMarker so memory-capture.ts
// can tell this was machine-generated, not typed by the user.
const onboardingKickoff = generatedInputMarker + "I just ran pix setup. Give me the upfront guide and help me get started."

// runSetupCmd is the `pix setup` entry. It accepts the same host-config
// flags as `onboard` plus an optional DIR (default "."), runs the host phase,
// prints the handoff, then execs the run with the onboarding kickoff message.
func runSetupCmd(argv []string) {
	if cli.WantsHelp(argv) {
		fmt.Print(setupUsage)
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
		if flagTakesValue(a) && i+1 < len(argv) {
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
		fmt.Fprintf(os.Stderr, "pix setup: %v\n\n%s", parseErr, setupUsage)
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
	if err := validateSetupSemantics(parsed, preflightCfg, env, hostBinaryResolver); err != nil {
		fmt.Fprintf(os.Stderr, "pix setup: %v\n\n%s", err, setupUsage)
		os.Exit(2)
	}

	// `--apply` is the surviving half of the deleted `pix onboard`: reconcile
	// a pending <DIR>/.pix/onboarding.json (the control-plane proposal an
	// in-sandbox onboarding agent wrote) and stop. It is deliberately NOT part of
	// the phase machine — it applies a proposal the user already reviewed rather
	// than provisioning a host — so it validates DIR, reconciles, and returns
	// without touching keys, packs, or the sandbox.
	if slices.Contains(hostArgs, "--apply") {
		if err := validateRunWorkspace(dir); err != nil {
			fmt.Fprintf(os.Stderr, "pix setup: %v\n", err)
			os.Exit(2)
		}
		opts, perr := onboard.ParseOnboardArgs(hostArgs)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "pix setup: %v\n\n%s", perr, setupUsage)
			os.Exit(2)
		}
		onboard.ReconcileOnboarding(dir, env, os.Stdin, os.Stdout, opts.AssumeYes, cli.IsTTY(os.Stdin), onboardDeps())
		return
	}
	if err := validateRunWorkspace(dir); err != nil {
		fmt.Fprintf(os.Stderr, "pix setup: %v\n", err)
		os.Exit(2)
	}
	// sbx is universally required. 1Password is conditional and is decided only
	// AFTER explicit packs have contributed inference; a keyless work gateway
	// must never trigger an irrelevant op installation/login flow.
	if err := ensureSetupPrereqsFor(env, os.Stdin, os.Stdout, cli.IsTTY(os.Stdin) && !parsed.AssumeYes, false); err != nil {
		fmt.Fprintf(os.Stderr, "pix setup: %v\n", err)
		os.Exit(1)
	}
	if err := ensureSetupSbxSession(env, os.Stdout, cli.IsTTY(os.Stdin) && !parsed.AssumeYes); err != nil {
		fmt.Fprintf(os.Stderr, "pix setup: %v\n", err)
		os.Exit(1)
	}
	// Pix's published base kit comes from GitHub, while a fresh sbx install only
	// trusts docker.io kit sources. Fill that one publisher allowlist entry and
	// initialize the one-time global network policy before the first handoff.
	// Existing publishers and an existing (possibly tightened) policy are kept.
	if err := ensureSetupSbxDefaults(env); err != nil {
		fmt.Fprintf(os.Stderr, "pix setup: %v\n", err)
		os.Exit(1)
	}
	// An unreleased launcher uses its local checkout kit. Validate that kit with
	// the installed sbx parser before pack OAuth/setup or any other mutation;
	// nightly schema skew must fail once, early, without opening browsers and
	// only later dumping YAML from `sbx run`.
	if err := validateSetupKit(version, resolveRepoRoot, validateSbxKit); err != nil {
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
		packArg := normalizeSetupPackArg(requestedPack)
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
	// pack, seed identity, provision+enable host mode (see setupHostPhase). This
	// ALWAYS runs first, regardless of whether a sandbox already exists for dir —
	// `pix setup` run a second time must still reconcile host keys/config
	// (a changed/rotated 1Password ref, a newly-added --mcp, etc), not skip
	// straight past it because a sandbox happens to be there.
	//
	// DIR must be validated (exists AND is a directory) BEFORE any of that runs —
	// setupHostPhase mutates real host state (op-refs.env/hostmode.env, config.toml,
	// the default pack, memory, host-mode enablement). A typo'd or nonexistent DIR
	// must fail immediately, with nothing touched, not be caught only later when the
	// (already-mutated) phase 2 sandbox probe/handoff can't resolve it. runSetupCore
	// is the seam: it does the validation-then-hostPhase-call as one pure step so a
	// test can assert hostPhase is never invoked for a bad DIR without exercising
	// os.Exit.
	if env.Quiet {
		fmt.Fprintln(os.Stdout, "Setting up inference and host services…")
	}
	if err := runSetupCore(env, dir, hostArgs, os.Stdin, os.Stdout, cli.IsTTY(os.Stdin), setupHostPhase); err != nil {
		fmt.Fprintf(os.Stderr, "pix setup: %v\n", err)
		var usage errUsage
		if errors.As(err, &usage) {
			os.Exit(2) // an argument mistake, caught before any probe or mutation
		}
		os.Exit(1)
	}
	// --no-agent stops here: the host phase is the whole command. The phase
	// header is still printed so the transcript is complete and a reader can
	// see that the handoff was skipped by request, not silently dropped.
	if noAgent {
		setupPhaseHeader(os.Stdout, setupPhaseHandoff, "skipped (--no-agent): host phase only, no sandbox")
		return
	}

	setupPhaseHeader(os.Stdout, setupPhaseHandoff, "")

	// Phase 2 decision: probe the sandbox for dir and branch on the POSITIVE
	// state. Existing without --replace is left alone — setup never
	// force-removes it and never replays the onboarding kickoff into a live
	// session (the fenced agent inside it may be mid-task). Existing WITH
	// --replace relaunches through `run --replace` carrying the kickoff, so
	// the recreated sandbox actually receives the tour. Only a POSITIVE
	// sbxAbsent gets the normal first handoff; an unprobeable sbx (sbxUnknown,
	// or an unresolvable name) FAILS CLOSED — launching blind could replay the
	// kickoff into a live session we simply couldn't see.
	name, nameOK := setupSandboxName(dir)
	state := sbxUnknown
	if nameOK && name != "" {
		state = probeTaskSandbox(env, name)
	}
	if err := runSetupHandoff(dir, name, state, replace, os.Stdout, runRun); err != nil {
		fmt.Fprintf(os.Stderr, "pix setup: %v\n", err)
		os.Exit(1)
	}
}

// runSetupCore validates DIR (reusing validateRunWorkspace's exists-and-is-a-
// directory check — the same rule `pix run` enforces, so setup and run
// never disagree about what counts as a launchable DIR) and, ONLY if that
// passes, invokes hostPhase. Extracted as its own tiny function — rather than
// inlining the check in runSetupCmd — so a nonexistent/file DIR is provably
// caught BEFORE hostPhase (which mutates op-refs.env/hostmode.env/config.toml/
// the default pack/memory/host-mode) ever runs: a test can pass a hostPhase
// stub that fails the test if called, and assert on the returned error alone,
// without needing to exercise runSetupCmd's os.Exit calls.
func runSetupCore(env hostenv.Env, dir string, hostArgs []string, in io.Reader, out io.Writer, tty bool, hostPhase func(hostenv.Env, []string, io.Reader, io.Writer, bool) error) error {
	if err := validateRunWorkspace(dir); err != nil {
		return err
	}
	return hostPhase(env, hostArgs, in, out, tty)
}

// runSetupHandoff is the pure post-host-phase decision + action, extracted so
// the state/replace matrix is testable without exercising os.Exit or actually
// exec'ing sbx (runFn is called instead of runRun directly; tests pass a stub
// that records the call). Returns an error ONLY for the fail-closed unknown
// state; the caller prints it and exits non-zero.
func runSetupHandoff(dir, name string, state sbxState, replace bool, out io.Writer, runFn func([]string)) error {
	// kickoffArgs builds the runRun argv for a launch that should receive the
	// tour: [DIR] [--replace] -- <onboardingKickoff>. DIR is forwarded only
	// when explicit so `pix setup` from inside a repo behaves exactly
	// like `pix run` there. --replace is harmless on an absent sandbox
	// (create path) and forces the recreate on an existing one.
	kickoffArgs := func() []string {
		args := []string{}
		if dir != "." {
			args = append(args, dir)
		}
		if replace {
			args = append(args, "--replace")
		}
		return append(args, "--", onboardingKickoff)
	}
	dirArg := ""
	if dir != "." {
		dirArg = " " + sys.ShellQuote(dir)
	}
	// retryArg carries the caller's ORIGINAL --replace request into the exact
	// retry command we print below. Dropping it would silently downgrade a
	// requested recreate into a plain reattach on retry — the user asked for
	// --replace once, and an unknown sbx state is exactly the case where they
	// have to run the command again by hand, so it must still say --replace.
	retryArg := dirArg
	if replace {
		retryArg += " --replace"
	}

	switch state {
	case sbxUnknown:
		// FAIL CLOSED: we could not determine whether a sandbox exists (sbx
		// errored/missing, or the name could not be resolved). Never launch:
		// runRun would re-attach a live session and replay the kickoff into it.
		// The host phase already completed, so a retry is cheap.
		which := fmt.Sprintf("sandbox %q", name)
		if name == "" {
			which = fmt.Sprintf("the sandbox for %s", dir)
		}
		return fmt.Errorf("cannot determine the state of %s (`sbx ls` failed or sbx is unavailable). Host setup completed; install or fix sbx (`%s`) and retry with: pix setup%s", which, sbxInstallHint, retryArg)
	case sbxRunning, sbxStopped:
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

	// sbxAbsent (positively confirmed): normal first launch — hand off to the
	// in-VM onboarding agent via an initial message. A --replace here is
	// harmless (the create path ignores it).
	fmt.Fprintln(out, "")
	if !setupTranscriptVerbose {
		fmt.Fprintln(out, "Launching Pix — the agent will take it from here.")
	} else {
		fmt.Fprintln(out, "Launching sandbox: pi will introduce itself, show you how it works,")
		fmt.Fprintln(out, "and get you into a real task. (You can quit any time; just run `pix run`.)")
	}
	runFn(kickoffArgs())
	return nil
}

// setupHostPhase does the deterministic host configuration and reports what is
// (and is not) ready. The only interactive step is pasting op:// refs for
// providers missing one (TTY + op installed); with flags OR no TTY it is fully
// non-interactive (the CI path).
// setupInteractivePrompts decides whether setup's key-collection/overwrite
// prompts fire: a real TTY, unless the caller explicitly opted out with
// --yes/-y/--non-interactive. Deliberately does NOT take the parsed flag list
// — ordinary value flags (--account/--knowledge/--mcp/--model) configure host
// settings and must never silently suppress the mandatory key prompts; only
// assumeYes (the explicit opt-out) does. Extracted as its own tiny function so
// this exact invariant has a direct regression test, not just end-to-end
// coverage through setupProvisionKeys.
func setupInteractivePrompts(tty, assumeYes bool) bool {
	return tty && !assumeYes
}

// ---------------------------------------------------------------------------
// The setup phase machine (AC-P0-301).
//
// `pix setup` runs as a NUMBERED TRANSCRIPT of eight phases, and each
// phase header is printed BEFORE that phase does any work — so a run that
// hangs names the phase it hung in instead of leaving the user staring at a
// blank terminal.
//
//	parse      read flags; argument mistakes exit 2 before any probe
//	inventory  read the current host state; NOTHING is written
//	gate       preconditions that must hold before the first mutation
//	mutate     the fixed-order, individually idempotent writes
//	consent    bounded interactive questions and what they authorize
//	verify     re-probe what was just changed
//	report     render, purely from the post-mutation evidence
//	handoff    launch the sandbox (skipped by --no-agent)
//
// Two invariants make the transcript trustworthy and are worth stating
// separately, because both were bugs before:
//
//   - The MUTATE phase returns no user-facing success strings at all — only
//     the set of readiness axes it touched (AC-P0-302). Every ✓ the user reads
//     comes from the report, which renders post-mutation probes. A mutation
//     that fails therefore cannot print a ✓ for its axis, because it never had
//     the ability to print one.
//   - Mutations run in a FIXED order with the riskiest last (AC-P0-303):
//     keys → config → pack → MCP → knowledge → identity → Google Workspace →
//     model pulls. Each step is individually idempotent, so an interrupted run
//     is resumed by simply re-running setup: the next run re-probes and
//     re-applies, and no journal file is consulted (a journal is state that
//     can itself be stale — trusting recorded over observed state is the exact
//     defect this command exists to remove).
const (
	setupPhaseParse     = "parse"
	setupPhaseInventory = "inventory"
	setupPhaseGate      = "gate"
	setupPhaseMutate    = "mutate"
	setupPhaseConsent   = "consent"
	setupPhaseVerify    = "verify"
	setupPhaseReport    = "report"
	setupPhaseHandoff   = "handoff"
)

// setupPhaseOrder is the transcript, in order. The index in this slice is the
// number the header prints, so the phases can never be renumbered by accident.
var setupPhaseOrder = []struct{ name, what string }{
	{setupPhaseParse, "reading flags"},
	{setupPhaseInventory, "reading the current host state (nothing is written yet)"},
	{setupPhaseGate, "checking preconditions before anything is written"},
	{setupPhaseMutate, "applying host configuration"},
	{setupPhaseConsent, "the things that cost you something"},
	{setupPhaseVerify, "re-probing what changed"},
	{setupPhaseReport, "what is actually ready"},
	{setupPhaseHandoff, "launching the sandbox"},
}

// setupPhaseHeader prints `[n/8] <phase> — <what>` BEFORE the phase runs.
// Pass a non-empty override to say something more specific than the default
// (e.g. that the handoff was skipped on purpose).
func setupPhaseHeader(out io.Writer, name, override string) {
	if !setupTranscriptVerbose {
		return
	}
	for i, p := range setupPhaseOrder {
		if p.name != name {
			continue
		}
		what := p.what
		if override != "" {
			what = override
		}
		fmt.Fprintf(out, "\n[%d/%d] %s — %s\n", i+1, len(setupPhaseOrder), p.name, what)
		return
	}
}

var setupTranscriptVerbose bool

// setupMaxPrompts is the hard cap on interactive questions ONE setup run may
// ask (AC-P0-307). There are exactly two: model-pull consent and the Google
// Workspace route. Pasting a 1Password ref in the keys step is not counted —
// it is not a question with a default, it is the mandatory input to a hard
// precondition, and a run that reaches it has already failed closed without it.
const setupMaxPrompts = 2

// setupPromptBudget enforces the cap. Every setup-owned prompt site must take
// its slot from here BEFORE prompting; a site that cannot get one falls back to
// its non-interactive behavior (which is always the safe default: don't pull,
// don't authorize). Non-interactive runs hand out no slots at all, which is how
// "non-TTY never prompts" (AC-P0-306) is enforced in one place instead of at
// each call site.
type setupPromptBudget struct {
	interactive bool
	spent       int
	asked       []string
}

// reserve claims one prompt slot for what, reporting whether the caller may
// prompt. It is deliberately EAGER (claimed when the site is reached, not when
// the question is finally printed) so the budget is a static property of the
// run rather than something that depends on probe results.
func (b *setupPromptBudget) reserve(what string) bool {
	if b == nil || !b.interactive || b.spent >= setupMaxPrompts {
		return false
	}
	b.spent++
	b.asked = append(b.asked, what)
	return true
}

// setupInventory is the PRE-mutation read of the host: what setup found before
// it changed anything. It is consumed by the gate and by the mutation steps —
// and NEVER by the report, which is a pure function of post-mutation evidence
// (AC-P0-302, guarded by TestSetupReport_NeverReadsInventory).
type setupInventory struct {
	cfg      *config.Config
	proposal *onboard.OnboardingResult
	retired  []string
}

// takeSetupInventory reads current state. It writes NOTHING: every call in
// here is a load, a parse, or a bounded probe.
func takeSetupInventory(env hostenv.Env, opts onboard.Opts) (setupInventory, error) {
	cfg, err := config.Load()
	if err != nil {
		return setupInventory{}, fmt.Errorf("loading config: %w", err)
	}
	inv := setupInventory{
		cfg:      cfg,
		retired:  cfg.RetiredKeys(),
		proposal: setupProposal(opts),
	}
	return inv, nil
}

// setupProposal is the single flag -> proposal translation used by both the
// pre-adoption semantic validator and the later inventory/mutation phase.
// Keeping one constructor prevents the early safety boundary from accepting a
// value that the host phase interprets differently.
func setupProposal(opts onboard.Opts) *onboard.OnboardingResult {
	p := &onboard.OnboardingResult{
		Version:           1,
		MCP:               append([]string(nil), opts.Mcp...),
		OllamaBridgeModel: strings.TrimSpace(opts.Model),
	}
	if k := strings.TrimSpace(opts.Knowledge); k != "" {
		p.Knowledge = &onboard.Knowledge{Action: "use", Source: k}
	}
	return p
}

// validateSetupSemantics checks only built-in argument meaning. It performs no
// writes and opens no authorization flow, so runSetupCmd can call it before the
// first pack is adopted. External readiness (catalog OAuth, provider reachability,
// model pulls) remains in the later gate/verify phases.
func validateSetupSemantics(opts onboard.Opts, cfg *config.Config, env hostenv.Env, hostResolver func() (string, error)) error {
	if len(opts.WithSetup) > 0 && len(opts.Packs) == 0 {
		return errUsage{fmt.Errorf("--with requires --pack")}
	}
	if err := checkGoogleWorkspaceFlags(opts); err != nil {
		return err
	}
	if err := onboard.ValidateOnboardingResult(setupProposal(opts), cfg, env, hostResolver); err != nil {
		return errUsage{err}
	}
	return nil
}

// setupGate is every precondition that must hold BEFORE the first mutation.
// Each failure names the exact command that fixes it and returns an error, so
// nothing is half-written when a run cannot succeed:
//
// Built-in semantic flag/value validation has already run before pack adoption
// in runSetupCmd (and immediately after inventory for direct callers). This gate
// owns only external readiness that cannot be established from argument meaning:
//   - a shipped-catalog MCP remote that is not registered AND auth-ready fails
//     here rather than being persisted on the promise of a later fix;
//
// The 1Password preconditions (op installed, op signed in, every provider ref
// resolvable, and the non-interactive "this needs a human" refusal) are NOT
// duplicated here: they belong to the keys step, which is the FIRST mutation
// and fails closed before it writes anything, so a gate copy would be a second
// implementation of the same rule that could drift from it.
func setupGate(env hostenv.Env, inv setupInventory, out io.Writer, interactive bool) error {
	// Shipped-catalog remotes (mcp.McpCatalogNames) must be registered AND
	// auth-ready BEFORE setup writes anything — setup must never claim success
	// for a server the gateway cannot spawn or that 401s on first use. The gate
	// covers both the new --mcp proposal and any catalog name already persisted
	// in cfg.MCP (the handoff would preload it too). It probes with bounded
	// native checks only and never opens an OAuth flow, so a non-interactive
	// setup can't trigger a browser grant.
	if err := verifyCatalogMCPReady(env, append(append([]string{}, inv.proposal.MCP...), inv.cfg.MCP...)); err != nil {
		return err
	}
	return nil
}

// setupMutationStep is one idempotent write, named for the transcript and
// tagged with the readiness axes it touches. run() returns an error or nil and
// writes NO success prose (see setupMutationOut).
type setupMutationStep struct {
	name string
	axes []readiness.Axis
	// fatal marks a step whose failure aborts setup. A non-fatal step reports
	// its own failure and lets the run continue to the report, which will show
	// the axis as not ready — the failure is never swallowed, it just is not
	// worth throwing away the rest of a working host over.
	fatal bool
	run   func() error
}

// setupMutationOrder is the FIXED order (AC-P0-303), riskiest last, named
// here so the order is a value a test can assert on rather than a property of
// the control flow. gworkspace, models and inference sit at the end because
// they are the only steps that talk to the user; models is second-to-last
// because it is the only step that can cost gigabytes, and inference is last
// because it can only judge what models left behind.
var setupMutationOrder = []string{"keys", "config", "pack", "mcp", "knowledge", "identity", "gworkspace", "models", "inference"}

// runSetupMutations executes steps in order and returns the axes it touched.
// It returns NO user-facing strings (AC-P0-302): the report is rendered from
// post-mutation probes, so a stubbed-to-fail mutation cannot print a ✓ for its
// axis. Steps that must talk to the user (the keys step's ref prompt, a
// non-fatal step's failure line) write diagnostics, never success claims.
func runSetupMutations(steps []setupMutationStep) (touched []readiness.Axis, err error) {
	for _, s := range steps {
		if e := s.run(); e != nil {
			if s.fatal {
				return touched, e
			}
			err = e
		}
		touched = append(touched, s.axes...)
	}
	return touched, err
}

// setupMutationSteps builds the ordered step table. Every closure here writes
// to io.Discard unless it is reporting a failure or collecting mandatory input.
func setupMutationSteps(env hostenv.Env, inv setupInventory, opts onboard.Opts, in io.Reader, out io.Writer, interactive bool, models *setupModelsOutcome, prompts *setupPromptBudget) []setupMutationStep {
	cfg := inv.cfg
	return []setupMutationStep{{
		name:  "keys",
		axes:  []readiness.Axis{readiness.AxisProviders, readiness.AxisSecrets},
		fatal: true,
		run: func() error {
			selected, err := setupChooseInference(cfg, env, in, out, interactive)
			if err != nil {
				return err
			}
			// GitHub is not an inference provider, but gh is a core sandbox CLI.
			// Reuse an existing host login without another prompt/browser flow.
			// It remains optional: an unauthenticated host does not block setup.
			if err := syncGitHubCredentialFromHost(env); err != nil {
				fmt.Fprintf(out, "  github: host credential was not synced (%v)\n", err)
			}
			if selected {
				// The roster moved to the `inference` step: an Ollama binding is only a
				// CANDIDATE here (its weights may not be pulled until the models step),
				// so choosing a roster now would either offer an unproven model or
				// hard-fail a user whose first setup has not downloaded anything yet.
				return cfg.Save()
			}
			if err := ensureSetupPrereqsFor(env, in, out, interactive, true); err != nil {
				return err
			}
			// The ONLY mutation that may write to the real terminal: on a TTY
			// it collects the mandatory op:// refs, and on failure it prints
			// exactly what is wrong. It prints no ✓ — the keys row in the
			// report comes from secret.HostModeProviderKeys AFTER this ran.
			if !setupProvisionKeysFn(env, in, out, interactive, opts.AssumeYes) {
				return fmt.Errorf("provider keys not fully configured — follow the fix printed above")
			}
			// Bind -> verify -> save -> judge -> roster, all of it in
			// reconcileDirectInference so `pix models add` runs the IDENTICAL
			// sequence. It living only here is why a key added any other way stayed
			// inert: the ref was written and nothing ever rebuilt the bindings.
			res, err := reconcileDirectInference(cfg, env, in, out, interactive, opts.Models, "")
			if err != nil {
				return err
			}
			if res.Verified > 0 && len(res.Failures) > 0 {
				fmt.Fprintf(out, "  inference: %d model(s) verified; %d candidate(s) unavailable or unauthorized (%s)\n",
					res.Verified, len(res.Failures), strings.Join(res.Failures, "; "))
			}
			return nil
		},
	}, {
		name:  "config",
		fatal: true,
		run: func() error {
			// Retired config keys (mcp_static/mcp_dynamic) are dropped by the
			// sparse encode whenever the config is saved; do it here so the
			// migration is deterministic even if a later step fails.
			if len(inv.retired) > 0 {
				if err := cfg.Save(); err != nil {
					return fmt.Errorf("dropping retired config keys: %w", err)
				}
			}
			// Config only: the knowledge half of the proposal is its own,
			// later step so the fixed order is real and not an illusion of one
			// combined call.
			cfgOnly := *inv.proposal
			cfgOnly.Knowledge = nil
			_, err := onboard.ApplyOnboardingResult(&cfgOnly, cfg, env, io.Discard, func(c *config.Config) error { return c.Save() })
			if err != nil {
				return err
			}
			if setupSelectRunnableIntent(cfg, env) {
				return cfg.Save()
			}
			return nil
		},
	}, {
		name:  "pack",
		axes:  nil,
		fatal: true,
		run: func() error {
			// Packs are explicit (`pix setup --pack ...`). Personal AGENTS.md and
			// skills live in XDG_DATA_HOME/pix/context, so default setup must not
			// manufacture a git repo or introduce the pack concept.
			return nil
		},
	}, {
		name: "mcp",
		axes: mcpAxes(cfg.MCP),
		run: func() error {
			if len(cfg.MCP) == 0 {
				return nil
			}
			var buf bytes.Buffer
			if err := registerServers(cfg, env, &buf, nil, hostBinaryResolver, pack.ActiveContainerMCP(cfg)); err != nil {
				fmt.Fprintf(out, "  mcp register skipped: %v (finish later: pix mcp register)\n", err)
				return err
			}
			return nil
		},
	}, {
		name:  "knowledge",
		axes:  []readiness.Axis{readiness.AxisServiceKnowledge},
		fatal: true,
		run: func() error {
			if inv.proposal.Knowledge == nil {
				return nil
			}
			only := &onboard.OnboardingResult{Version: 1, Knowledge: inv.proposal.Knowledge}
			_, err := onboard.ApplyOnboardingResult(only, cfg, env, io.Discard, func(c *config.Config) error { return c.Save() })
			return err
		},
	}, {
		name: "identity",
		run: func() error {
			// Read the user's first name from the HOST's git config (the
			// sandbox cannot see ~/.gitconfig) and seed it into memory so
			// onboarding can greet by name. Best-effort and SILENT: the report
			// re-reads git config itself, so nothing here needs to claim
			// anything.
			seedIdentity(env, io.Discard)
			return nil
		},
	}, {
		name: "gworkspace",
		axes: []readiness.Axis{readiness.AxisGworkspace},
		run: func() error {
			// Google Workspace is OFF unless --google-workspace. It runs the
			// SAME transaction `pix gworkspace setup` runs, through the
			// same façade, so there is exactly one writer. It returns no
			// success text: the row in the report is rendered from a
			// post-mutation probe, so a half-finished authorization can never
			// print a ✓.
			if !opts.GoogleWorkspace {
				return nil
			}
			ask := prompts.reserve("google ws route")
			if err := setupGoogleWorkspaceFn(env, gogSetupOpts{
				account:     strings.TrimSpace(opts.Account),
				credentials: strings.TrimSpace(opts.Credentials),
				assumeYes:   opts.AssumeYes,
			}, in, out, ask); err != nil {
				return fmt.Errorf("google ws: %w", err)
			}
			return nil
		},
	}, {
		name: "models",
		axes: []readiness.Axis{readiness.AxisModelWatcher, readiness.AxisModelEmbed, readiness.AxisModelBridge},
		run: func() error {
			// The riskiest step, therefore last: probe Ollama once, classify on
			// the shared axis.ModelReadiness axes, pull confirmed-missing tags only
			// under explicit consent (--pull-models, or the one default-No
			// prompt), verify once after the pulls, receipt the outcome. Never
			// installs Ollama; never pulls a tag it could not positively verify
			// as missing.
			// Local models are progressive enhancement. When Ollama is already
			// healthy, interactive setup may offer a default-No pull for positively
			// missing memory models; unattended setup requires --pull-models.
			ask := prompts.reserve("enable local memory")
			*models = setupLocalModels(cfg, env, in, out, ask, opts.PullModels)
			if setupMemoryModelsReady(cfg, *models) {
				cfg.AddService("memory")
				if err := cfg.Save(); err != nil {
					return fmt.Errorf("enabling verified memory: %w", err)
				}
			}
			receiptSetupModels(env, out, *models)
			return nil
		},
	}, {
		name:  "inference",
		axes:  []readiness.Axis{readiness.AxisProviders},
		fatal: false,
		run: func() error {
			// LAST, and non-fatal: it can only judge what the models step left
			// behind, and a probe failure must not stop the report from rendering
			// the axes the run did touch. It prints no routine success line —
			// success words come from the post-mutation probe (AC-P0-302).
			return runSetupInferenceStep(cfg, env, in, out, interactive, *models)
		},
	}}
}

// runSetupInferenceStep verifies ollama bindings with real requests, picks the
// roster from what actually answered, and then branches on the ONE question
// that matters: is there anything callable, and if not, whose decision was it.
//
// Declining a multi-gigabyte download is a decision, not a failure: it returns
// nil and setup exits 0 with an honest `✗ inference` summary. A non-zero exit
// stays reserved for probes that were dispatched and refused, and for a pull
// that was consented to and then failed (which the models step already reports
// — a second error would double-report one cause).
func runSetupInferenceStep(cfg *config.Config, env hostenv.Env, in io.Reader, out io.Writer, interactive bool, models setupModelsOutcome) error {
	probe, err := verifyOllamaInference(cfg, env, out)
	if err != nil {
		return fmt.Errorf("verifying ollama models: %w", err)
	}
	attempted, verified, failures, notProbed := probe.Attempted, probe.Verified, probe.Failures, probe.NotProbed
	callable, _ := axis.ConfiguredInferenceSummary(cfg)
	if callable > 0 {
		// Deviation from the design: the roster prompt is NOT taken from
		// prompts.reserve. setupMaxPrompts is 2 and both slots are already claimed
		// (gworkspace, models), so reserving here would deny the prompt and
		// silently auto-select every candidate — a regression, not a budget fix.
		if err := configureModelRoster(cfg, in, out, interactive, ""); err != nil {
			return fmt.Errorf("choosing models: %w", err)
		}
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	if verified > 0 {
		if len(failures) > 0 {
			fmt.Fprintf(out, "  inference: %d model(s) verified; %d candidate(s) unavailable or unauthorized (%s)\n",
				verified, len(failures), strings.Join(failures, "; "))
		}
		return nil
	}
	// Cloud was selected but nothing on the plan answered: that is a hard
	// failure, because a silent "configured" for an account that can call
	// nothing is the exact class of claim this whole path exists to delete.
	if cloud := ollamaCloudCandidates(cfg); len(cloud) > 0 && attempted > 0 {
		return fmt.Errorf("Ollama Cloud was selected, but no cloud model answered a request: %s. Sign in with `ollama signin`, then re-run `pix setup`",
			strings.Join(failures, "; "))
	}
	// A DECLINED (or never-offered) pull explains the failure completely: the
	// weights are not on disk, so the probe had nothing to answer with. That is
	// the documented contract of this whole step — "declining a multi-gigabyte
	// download is a decision, not a failure" — and it belongs ahead of the
	// generic hard error below, which would otherwise exit non-zero for a user
	// who simply said no.
	//
	// This was live and invisible: the test covering it wired NO probe, so
	// `attempted` was 0 and control fell through to the consent switch by
	// accident. With a probe that actually refuses — which is what a real host
	// does when the tag was never pulled — `pix setup`, choose Ollama local,
	// decline the download, exited non-zero.
	//
	// Cloud is deliberately excluded (handled above): an entitlement refusal is
	// not explained by a download nobody started.
	declinedPull := models.consent == "none" || models.consent == "prompt-no"
	if attempted > 0 && !declinedPull {
		return fmt.Errorf("ollama models are bound, but none answered a request: %s", strings.Join(failures, "; "))
	}
	if len(axis.UnverifiedOllamaCandidates(cfg)) == 0 {
		return nil // nothing ollama-shaped here; the keys step owns this host
	}
	switch models.consent {
	case "--pull-models", "prompt-yes":
		// The pull was consented to and did not produce a callable model. The
		// models step already failed with the exact retry command and owns the
		// non-zero exit; repeating it here would report one cause twice.
		return nil
	default:
		// Declined or never asked. Print the truth, claim nothing, exit 0.
		if len(notProbed) > 0 {
			fmt.Fprintf(out, "  inference: %d candidate(s) not probed (the local budget ran out) — re-run: pix setup\n", len(notProbed))
		}
		fmt.Fprintf(out, "  inference: no model has passed a probe yet — pull one: %s\n", axis.PullModelsFixCmd)
		return nil
	}
}

// syncGitHubCredentialFromHost mirrors the current host gh login into sbx's
// global github service. The token exists only in process memory and the child
// argv accepted by sbx (the same unavoidable boundary used by provider-key
// sync); output/errors are redacted before they can reach a transcript.
func syncGitHubCredentialFromHost(env hostenv.Env) error {

	if _, err := env.LookPath("gh"); err != nil {
		return nil
	}
	if _, err := env.LookPath("sbx"); err != nil {
		return nil
	}
	token, err := env.Run("gh", "auth", "token")
	if err != nil || strings.TrimSpace(token) == "" {
		return nil // optional: no host login to reuse
	}
	token = strings.TrimSpace(token)
	out, err := env.Run("sbx", "secret", "set", "github", "-f", "-t", token)
	if err == nil {
		return nil
	}
	detail := secret.RedactSecretValue(strings.TrimSpace(secret.FirstLine(out)), token)
	if detail == "" {
		detail = secret.RedactSecretValue(err.Error(), token)
	}
	return fmt.Errorf("sbx secret set github failed: %s", detail)
}

// mcpAxes maps configured server names to their readiness axes.
func mcpAxes(servers []string) []readiness.Axis {
	var out []readiness.Axis
	for _, s := range servers {
		if strings.TrimSpace(s) == "" {
			continue
		}
		out = append(out, readiness.MCPAxis(s))
	}
	return out
}

// setupHostPhase runs the host half of `pix setup` as the eight-phase
// transcript documented above. The only interactive steps are the mandatory
// op:// ref collection (TTY + op installed) and bounded consent questions;
// with --yes/--non-interactive or no TTY it is fully
// non-interactive (the CI path).
func setupHostPhase(env hostenv.Env, flags []string, in io.Reader, out io.Writer, tty bool) error {
	setupTranscriptVerbose = !env.Quiet
	if !env.Quiet {
		fmt.Fprintln(out, "pix setup — configuring the host")
	}

	// PHASE 1 — parse. Argument mistakes are caught here, before any probe or
	// mutation, and map to exit 2 at the call site.
	setupPhaseHeader(out, setupPhaseParse, "")
	opts, perr := onboard.ParseOnboardArgs(flags)
	if perr != nil {
		return errUsage{perr}
	}
	if opts.Apply {
		// --apply is intercepted by runSetupCmd (it reconciles a pending
		// onboarding.json and stops). Reaching the host phase with it set means
		// a caller bypassed that route, which would silently ignore the flag.
		return errUsage{fmt.Errorf("--apply is handled before the host phase; run `pix setup [DIR] --apply`")}
	}
	// Interactive prompts fire on any real TTY unless the caller explicitly opted
	// out with --yes/-y/--non-interactive (opts.AssumeYes). Ordinary VALUE flags
	// (--account/--knowledge/--mcp/--model) configure host settings; they say
	// nothing about whether pasting a 1Password ref should still prompt, so their
	// mere presence must NOT silently suppress the key-collection/overwrite
	// prompts — only an explicit non-interactive opt-out does.
	interactive := setupInteractivePrompts(tty, opts.AssumeYes)
	prompts := &setupPromptBudget{interactive: interactive}

	// PHASE 2 — inventory. Reads only.
	setupPhaseHeader(out, setupPhaseInventory, "")
	inv, err := takeSetupInventory(env, opts)
	if err != nil {
		return err
	}
	if err := validateSetupSemantics(opts, inv.cfg, env, hostBinaryResolver); err != nil {
		return err
	}
	if len(inv.retired) > 0 {
		fmt.Fprintf(out, "note: dropping retired config key(s) %s on save (no longer read); every configured MCP server preloads at sandbox create\n", strings.Join(inv.retired, ", "))
	}

	// PHASE 3 — gate. Nothing has been written yet; a failure here leaves the
	// host exactly as it was found.
	setupPhaseHeader(out, setupPhaseGate, "")
	if err := setupGate(env, inv, out, interactive); err != nil {
		return err
	}

	// PHASE 4 — mutate, and PHASE 5 — consent. One ordered step table
	// (setupMutationOrder), split at the point where the steps start asking
	// permission: the first six are unattended, the last three (gworkspace,
	// models, inference) are the consented, riskiest-last group.
	var models setupModelsOutcome
	steps := setupMutationSteps(env, inv, opts, in, out, interactive, &models, prompts)
	split := len(steps) - 3
	setupPhaseHeader(out, setupPhaseMutate, "")
	if _, err := runSetupMutations(steps[:split]); err != nil {
		return err
	}
	setupPhaseHeader(out, setupPhaseConsent, "")
	if _, err := runSetupMutations(steps[split:]); err != nil {
		return err
	}

	// PHASE 6 — verify. Re-probe, from scratch, everything the mutations
	// touched. Nothing recorded by the mutate phase is trusted here.
	setupPhaseHeader(out, setupPhaseVerify, "")
	postCfg, cerr := config.Load()
	if cerr != nil {
		postCfg = inv.cfg
	}
	req := readiness.RequestAll(postCfg.MCP, setupRequestedAxes(opts)...)
	snap := readiness.Build(req, setupReadinessAxes(postCfg, env, models))

	// PHASE 7 — report. A pure function of the post-mutation snapshot: it
	// takes no inventory, no mutation log, and no "what we meant to do".
	setupPhaseHeader(out, setupPhaseReport, "")
	printSetupSummary(postCfg, env, out, models)

	if !env.Quiet {
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "host mode (optional, UNSANDBOXED: runs `pi` directly on the host): not enabled.")
		fmt.Fprintln(out, "  set it up only if you need it:  pix host setup")
	}

	// A partial pull failure is a real, verified gap the user consented to
	// closing: fail setup (non-zero) with the exact retry commands. The summary
	// above already reported it truthfully.
	if len(models.failed) > 0 {
		return fmt.Errorf("local model pull failed for %s — retry by hand: ollama pull %s, then re-run pix setup",
			strings.Join(models.failed, ", "), strings.Join(models.failed, "; ollama pull "))
	}
	// An axis the user explicitly ASKED for on this invocation and that did not
	// end ready is a failed request, not a shrug: exit 1 (AC-P0-210). Stale
	// optional config never blocks unrelated repair, because only the axes this
	// invocation's flags promoted are consulted.
	if short := snap.RequestedShortfall(req); len(short) > 0 {
		return fmt.Errorf("%s — see the rows above; nothing else was left half-done", requestedShortfallMessage(short, snap))
	}
	return nil
}

// setupRequestedAxes maps THIS invocation's flags to the axes they promote from
// optional to blocking (AC-P0-209). Promotion itself lives in the readiness
// type (build); this is only the flag→axis mapping, so no command
// re-implements the rule.
//
// `--mcp X` promotes `mcp:X`, which setup additionally enforces in the gate
// (verifyCatalogMCPReady) — a requested server that cannot come up fails before
// anything is written, which is strictly earlier than an exit code.
func setupRequestedAxes(opts onboard.Opts) []readiness.Axis {
	var out []readiness.Axis
	if opts.PullModels {
		out = append(out, readiness.AxisOllamaHost, readiness.AxisModelWatcher, readiness.AxisModelEmbed, readiness.AxisModelBridge)
	}
	if opts.GoogleWorkspace {
		out = append(out, readiness.AxisGworkspace)
	}
	out = append(out, mcpAxes(opts.Mcp)...)
	return out
}

// requestedShortfallMessage names the requested axes that did not end ready,
// in snapshot order, with the verdict word for each — so the exit-1 line says
// which request failed and how, never just "setup failed".
func requestedShortfallMessage(short []readiness.Axis, s readiness.Snapshot) string {
	parts := make([]string, 0, len(short))
	for _, a := range short {
		_, v, ok := s.AxisVerdict(a)
		if !ok {
			continue
		}
		parts = append(parts, string(a)+": "+readiness.VerdictWord(v))
	}
	return "you asked for " + strings.Join(parts, ", ")
}

// setupReadinessAxes is the builder set for setup's VERIFY phase: the shared
// Ollama/model and service builders doctor uses (so setup and doctor can never
// disagree), plus the three axes only setup's own post-mutation reads can speak
// to. Every builder here probes; none reads the inventory.
func setupReadinessAxes(cfg *config.Config, env hostenv.Env, models setupModelsOutcome) map[readiness.Axis]readiness.AxisBuilder {
	builders := map[readiness.Axis]readiness.AxisBuilder{}
	for a, b := range ollamaReadinessAxes(cfg, env, "", nil) {
		builders[a] = b
	}
	if env.IdentityProbe != nil {
		for a, b := range axis.ServiceReadinessAxes(env, config.ServiceEnabled(cfg, "memory"), config.ServiceEnabled(cfg, "knowledge"), env.IdentityProbe) {
			builders[a] = b
		}
	}
	builders[readiness.AxisProviders] = func() []readiness.Check { return setupProvidersAxis(cfg, env) }
	if strings.TrimSpace(cfg.Pack) != "" {
		builders[readiness.AxisPack] = func() []readiness.Check { return setupPackAxis(cfg) }
	}
	if strings.TrimSpace(cfg.GogAccount) != "" || slices.Contains(cfg.MCP, config.GWServerName) {
		// Absent by default (AC-P0-319): with no opt-in there is no axis at
		// all, so the report says nothing about Google Workspace.
		builders[readiness.AxisGworkspace] = func() []readiness.Check { return setupGworkspaceAxis(cfg, env) }
	}
	return builders
}

// setupProvidersAxis is the post-mutation provider-key fact: ready when at
// least one model-provider ref resolves (any one key launches a sandbox).
func setupProvidersAxis(cfg *config.Config, env hostenv.Env) []readiness.Check {
	if cfg != nil && len(cfg.Inference.Models) > 0 {
		callable := 0
		candidates := 0
		for _, b := range cfg.Inference.Models {
			if b.Available && inference.Allowed(cfg, b) {
				candidates++
				if b.Verified {
					callable++
				}
			}
		}
		if callable > 0 {
			detail := fmt.Sprintf("%d callable model(s)", callable)
			if candidates > callable {
				detail += fmt.Sprintf("; %d candidate(s) did not pass live verification", candidates-callable)
			}
			return []readiness.Check{{Label: "inference", Requirement: readiness.RequirementCore, Verdict: readiness.VerdictReady,
				Detail: detail, Evidence: "model-specific live inference probes"}}
		}
		if candidates > 0 {
			return []readiness.Check{{Label: "inference", Requirement: readiness.RequirementCore, Verdict: readiness.VerdictUnverifiable,
				Detail: fmt.Sprintf("%d configured model candidate(s)", candidates), Evidence: "first sandbox inference is the live probe"}}
		}
		return []readiness.Check{{Label: "inference", Requirement: readiness.RequirementCore, Verdict: readiness.VerdictTodo,
			Detail: "no callable model", Evidence: "configured bindings have no successful probe"}}
	}
	names, err := secret.HostModeProviderKeys(env)
	switch {
	case err != nil:
		return []readiness.Check{{Label: "provider keys", Requirement: readiness.RequirementCore, Verdict: readiness.VerdictUnverifiable,
			Detail: "could not read hostmode.env (" + err.Error() + ")", Evidence: "hostmode.env unreadable: " + err.Error()}}
	case len(names) == 0:
		return []readiness.Check{{Label: "provider keys", Requirement: readiness.RequirementCore, Verdict: readiness.VerdictTodo,
			Detail: "no provider key configured", Evidence: "hostmode.env lists no provider key",
			Todo: "pix models add anthropic"}}
	default:
		return []readiness.Check{{Label: "provider keys", Requirement: readiness.RequirementCore, Verdict: readiness.VerdictReady,
			Detail: strings.Join(names, ", "), Evidence: "hostmode.env lists " + strings.Join(names, ", ")}}
	}
}

// setupSelectRunnableIntent prevents a successful one-provider setup from
// immediately selecting a model whose provider has no key. It changes only
// the shipped OpenAI-specific default. Explicit non-default user choices and
// multi-provider installations are left untouched.
func setupSelectRunnableIntent(cfg *config.Config, env hostenv.Env) bool {
	if cfg == nil || cfg.RunIntent != config.DefaultRunIntent {
		return false
	}
	names, err := secret.HostModeProviderKeys(env)
	if err != nil || len(names) != 1 {
		return false
	}
	switch names[0] {
	case "anthropic":
		cfg.RunIntent = "strategy"
		return true
	case "google":
		cfg.RunIntent = "review"
		return true
	default:
		return false
	}
}

// setupPackAxis is the post-mutation pack fact: an ACTIVE but EMPTY pack is a
// TODO, never green.
func setupPackAxis(cfg *config.Config) []readiness.Check {
	p := resolveHostStatePack(cfg, "")
	switch {
	case p.Active && p.Exists && (p.Skills || p.Knowledge):
		return []readiness.Check{{Label: "pack", Requirement: readiness.RequirementCore, Verdict: readiness.VerdictReady,
			Detail: p.Path + " (active)", Evidence: "active pack " + p.Path + " has content"}}
	case p.Active && p.Exists:
		return []readiness.Check{{Label: "pack", Requirement: readiness.RequirementCore, Verdict: readiness.VerdictTodo,
			Detail: "active but empty (" + p.Path + ")", Evidence: "active pack " + p.Path + " has no skills or knowledge",
			Todo: "pix pack add skill <name>"}}
	default:
		return []readiness.Check{{Label: "pack", Requirement: readiness.RequirementCore, Verdict: readiness.VerdictTodo,
			Detail: "no active pack", Evidence: "no pack is active", Todo: "pix pack new"}}
	}
}

// setupGworkspaceAxis is the post-mutation Google Workspace fact, probed the
// same way `pix gworkspace status` probes it.
func setupGworkspaceAxis(cfg *config.Config, env hostenv.Env) []readiness.Check {
	acct := strings.TrimSpace(cfg.GogAccount)
	switch {
	case acct == "":
		return []readiness.Check{{Label: "google ws", Requirement: readiness.RequirementOptional, Verdict: readiness.VerdictTodo,
			Detail: "enabled but no account authorized", Evidence: "google_workspace_account is empty",
			Todo: "pix gworkspace setup"}}
	case gogSetupAccountHealthy(env, acct):
		return []readiness.Check{{Label: "google ws", Requirement: readiness.RequirementOptional, Verdict: readiness.VerdictReady,
			Detail: acct + " authorized (read-only)", Evidence: "authorization probe passed for " + acct}}
	default:
		return []readiness.Check{{Label: "google ws", Requirement: readiness.RequirementOptional, Verdict: readiness.VerdictTodo,
			Detail: acct + " not verified", Evidence: "authorization probe failed for " + acct,
			Todo: "pix gworkspace setup"}}
	}
}

// providerKeyPromptAttempts caps how many times setupProvisionKeys reprompts
// for a single provider's ref before giving up, since a human who keeps
// mistyping (or an unattended TTY feeding garbage) must not hang setup
// forever.
const providerKeyPromptAttempts = 3

// setupProvisionKeys sources one or more model provider keys from 1Password
// and reconciles them into the sbx secret store. This is the ONLY
// provider-key path: op is required, and the removed --use-sbx-keys flag,
// persisted provider_key_mode, and the "already in sbx?" convenience prompt are
// gone. Returns whether all keys ended up usable.
//
// Step 0 (hard preconditions): `op` must be installed AND signed in. Without
// either there is nothing to source keys from — fail setup with the exact fix,
// before pack/host/onboarding ever run.
//
// Step 1 (collect + validate configured refs): existing provider refs are all
// validated. When none exists, an interactive setup asks which ONE provider to
// configure and then collects that provider's ref. One provider is enough to
// run Pix; additional providers can be added later with `pix secret set`.
//   - a ref already configured (op-refs.env OR hostmode.env, via secret.CurrentOpRef)
//     is CONFIRMED, not re-solicited — but it still must resolve via `op read`
//     to a non-empty value; a broken existing ref fails setup outright.
//   - a ref with NO configuration yet, on an interactive TTY, is prompted for
//     one at a time. Empty input or EOF is NOT "skip" — a key is mandatory, so
//     that fails setup. An invalid/unresolvable ref reprompts (capped at
//     providerKeyPromptAttempts).
//   - a ref with NO configuration and NO interactive TTY prints the exact
//     `pix secret set` command per missing provider and fails setup.
//
// Step 2 (mirror + verify): every validated ref is written to BOTH op-refs.env
// (sandbox) and hostmode.env (host mode); setup then verifies they all landed
// in hostmode.env.
//
// Step 3 (reconcile sbx): secret.ReconcileProviderKeysWithSbx brings sbx to the same
// state as the validated refs, fed the Step-1 snapshot. A reconcile failure
// fails setup. Step 4 (final probe) requires every configured key usable in sbx.
//
// Steps 1-3 run holding the provider-refs transaction lock so a concurrent
// `pix secret set`/`secret rm` cannot interleave. Never persists or prints
// a resolved secret value.
//
// setupProvisionKeysFn is the seam setupHostPhase calls through (a package var
// so a test can replace it with a stub).
var setupProvisionKeysFn = setupProvisionKeys

// setupProvisionKeys resolves provider keys from 1Password (the strict, and now
// only, flow) and returns whether it succeeded.
func setupProvisionKeys(env hostenv.Env, in io.Reader, out io.Writer, interactive, assumeYes bool) bool {
	return runStrictProviderKeyFlow(env, bufio.NewScanner(in), out, interactive, assumeYes)
}

// runStrictProviderKeyFlow resolves the configured provider keys from 1Password and
// reconciles it into sbx (Steps 0-4 documented on setupProvisionKeys). It is the
// only provider-key path now that the sbx-keys shortcut is gone.
func runStrictProviderKeyFlow(env hostenv.Env, sc *bufio.Scanner, out io.Writer, interactive, assumeYes bool) bool {
	fmt.Fprintln(out, "")

	if !secret.OpInstalled(env) {
		fmt.Fprintln(out, "1Password provider setup requires the `op` CLI, but it isn't installed.")
		fmt.Fprintln(out, "  fix: brew install 1password-cli   (or https://developer.1password.com/docs/cli/)")
		fmt.Fprintln(out, "then re-run the same setup command.")
		return false
	}
	if !secret.OpSignedIn(env) {
		if interactive {
			fmt.Fprintln(out, "1Password needs authorization. Continuing with the official `op signin` flow.")
			if err := env.RunInteractive("op", "signin"); err == nil && secret.OpSignedIn(env) {
				// Continue directly into provider selection. No separate user command
				// or Pix identity is introduced.
			} else {
				fmt.Fprintln(out, "`op signin` did not establish a usable 1Password session.")
				fmt.Fprintln(out, "  fix: op signin   (or add an account in the 1Password app)")
				fmt.Fprintln(out, "then re-run the same setup command.")
				return false
			}
		} else {
			fmt.Fprintln(out, "`op` is installed but no 1Password account is configured.")
			fmt.Fprintln(out, "  fix: op signin   (or add an account in the 1Password app)")
			fmt.Fprintln(out, "then re-run the same setup command.")
			return false
		}
	}

	// Hold the provider-refs transaction lock across the WHOLE flow: initial
	// ref reads/validation, the canonical both-file writes, the hostmode
	// verification, and the sbx reconciliation + synced-ref metadata. A lock
	// acquisition failure fails setup honestly — never proceed unlocked.
	ok := false
	if lerr := secret.WithProviderRefsLock(env, func() error {
		ok = strictProviderKeyFlowLocked(env, sc, out, interactive, assumeYes)
		return nil
	}); lerr != nil {
		fmt.Fprintf(out, "  \u2717 could not lock provider refs (%s): %v — another pix credential operation may hold it; fix that and re-run the same setup command.\n", secret.ProviderRefsLockPath(env), lerr)
		return false
	}
	return ok
}

// strictProviderKeyFlowLocked is runStrictProviderKeyFlow's transaction body
// (Steps 1-4). Caller MUST hold the provider-refs lock; every refs-file write
// in here goes through a *Locked variant for exactly that reason.
func strictProviderKeyFlowLocked(env hostenv.Env, sc *bufio.Scanner, out io.Writer, interactive, assumeYes bool) bool {
	// refs is the validated snapshot reconcile (STEP 3) works from (envVar ->
	// op:// ref — every entry validated AND canonical-written to both files
	// below); resolved caches each provider's validated op-read value so
	// reconcile never pays for a second `op read` of the same ref.
	refs := make(map[string]string, len(secret.ProviderKeyRefOrder))
	resolved := make(map[string]string, len(secret.ProviderKeyRefOrder))
	configured := make([]secret.ProviderKeyRef, 0, len(secret.ProviderKeyRefOrder))
	for _, p := range secret.ProviderKeyRefOrder {
		if _, ok := secret.CurrentOpRef(env, p.EnvVar); ok {
			configured = append(configured, p)
		}
	}
	if len(configured) == 0 {
		if !interactive {
			fmt.Fprintln(out, "No model provider is configured. Add any ONE provider:")
			for _, p := range secret.ProviderKeyRefOrder {
				// No terminal here, so `pix models add` cannot collect a ref: it would
				// exit 2 pointing back at this command. Lead with the scripted form.
				fmt.Fprintf(out, "  pix secret set %s op://Vault/Item/field  # %s\n", p.EnvVar, p.Name)
			}
			fmt.Fprintln(out, "then re-run: pix setup   (or, on a terminal: pix models add <provider>)")
			return false
		}
		chosen, ok := promptProviderChoice(sc, out)
		if !ok {
			return false
		}
		configured = append(configured, chosen)
	}

	for _, p := range configured {
		ref, hasRef := secret.CurrentOpRef(env, p.EnvVar)
		switch {
		case hasRef:
			val, ok := secret.OpReadNonEmpty(env, ref)
			if !ok {
				fmt.Fprintf(out, "  %s \u2717 configured 1Password ref does not resolve (op read failed or empty)\n", p.Name)
				fmt.Fprintf(out, "    fix it: pix secret set %s op://Vault/Item/field\n", p.EnvVar)
				return false
			}
			// secret.CurrentOpRef may have found this ref in EITHER file (op-refs.env OR
			// hostmode.env), not necessarily both. Idempotently upsert it into BOTH
			// here — a no-op where it already matches — and FAIL setup if either
			// write errors, rather than silently backfilling one file and calling
			// that success (the bug: a ref found only in hostmode.env must not be
			// allowed to leave op-refs.env permanently missing it).
			if err := secret.WriteOpRefQuietLocked(env, p.EnvVar, ref); err != nil {
				fmt.Fprintf(out, "  %s \u2717 could not write ref to op-refs.env: %v\n", p.Name, err)
				return false
			}
			if err := secret.WriteOpRefFileQuietLocked(env, secret.HostModeRefsPath(env), p.EnvVar, ref); err != nil {
				fmt.Fprintf(out, "  %s \u2717 could not write ref to hostmode.env: %v\n", p.Name, err)
				return false
			}
			// No ✓ here on purpose (AC-P0-302): the keys step runs in the
			// mutate phase, which prints no success claims. The keys row in
			// setup's report is rendered from a post-mutation read of
			// hostmode.env, so a run whose key writes fail cannot have
			// printed a green line for them earlier.
			refs[p.EnvVar] = ref
			resolved[p.EnvVar] = val
		case interactive:
			ref, val, ok := promptProviderRef(env, sc, out, p)
			if !ok {
				return false
			}
			if err := secret.WriteOpRefQuietLocked(env, p.EnvVar, ref); err != nil {
				fmt.Fprintf(out, "  %s \u2717 could not save ref: %v\n", p.Name, err)
				return false
			}
			if err := secret.WriteOpRefFileQuietLocked(env, secret.HostModeRefsPath(env), p.EnvVar, ref); err != nil {
				fmt.Fprintf(out, "  %s \u2717 could not save host-mode ref: %v\n", p.Name, err)
				return false
			}
			refs[p.EnvVar] = ref
			resolved[p.EnvVar] = val
		default:
			// Only reachable for the one provider selected above.
			return false
		}
	}

	// Every validated ref was already canonical-written to BOTH files above, so
	// there is no reread-and-remirror pass here — just the final membership
	// verification that every configured provider landed in hostmode.env (host mode reads ONLY
	// hostmode.env via `op run --env-file`, never op-refs.env).
	got, kerr := secret.HostModeProviderKeys(env)
	if kerr != nil {
		fmt.Fprintf(out, "  \u2717 credential state unreadable: %v\n", kerr)
		return false
	}
	gotSet := map[string]bool{}
	for _, name := range got {
		gotSet[name] = true
	}
	for _, p := range configured {
		if !gotSet[p.Name] {
			fmt.Fprintf(out, "  \u2717 hostmode.env is missing configured provider %s after mirroring\n", p.Name)
			return false
		}
	}

	if !secret.ReconcileProviderKeysWithSbx(env, sc, out, interactive, assumeYes, refs, resolved) {
		return false
	}

	// Tri-state: only abort setup when we can POSITIVELY confirm sbx is missing
	// a key (secret.SbxSecretsOK). sbx being entirely ABSENT is portability — fail
	// open, we can't tell. sbx being installed but the check command FAILING is
	// a real, diagnosable problem — fail CLOSED with a message, never silently
	// pass a box whose completeness we couldn't actually verify.
	sbxOut, state := secret.ProbeSbxSecrets(env)
	switch state {
	case secret.SbxSecretsAbsent:
		return true
	case secret.SbxSecretsError:
		fmt.Fprintln(out, "  \u2717 could not verify sbx has the configured provider keys (`sbx secret ls` failed) \u2014 check sbx and re-run the same setup command")
		return false
	}
	for _, p := range secret.ProviderKeyRefOrder {
		if _, configured := refs[p.EnvVar]; configured && !cli.GrepWord(sbxOut, p.Name) {
			fmt.Fprintf(out, "  \u2717 sbx is missing configured provider %s after reconciliation\n", p.Name)
			return false
		}
	}
	return true
}

// promptProviderChoice keeps first-run setup to one decision and one ref. It
// accepts either the displayed number or provider name and defaults to OpenAI,
// matching Pix's default overlord route.
func promptProviderChoice(sc *bufio.Scanner, out io.Writer) (secret.ProviderKeyRef, bool) {
	empty := secret.ProviderKeyRef{}
	fmt.Fprintln(out, "One model provider is enough to start.")
	// Name the literal command. "You can add others later" was true and useless:
	// the only later path was `pix secret set`, which stores a ref and stops, so
	// the second key stayed inert and there was nothing to search for.
	fmt.Fprintln(out, "Add the others whenever you like with: pix models add <provider>")
	fmt.Fprintln(out, "  1. openai (default)")
	fmt.Fprintln(out, "  2. anthropic")
	fmt.Fprintln(out, "  3. google")
	fmt.Fprint(out, "Choose a provider [1]: ")
	if !sc.Scan() {
		fmt.Fprintln(out, "\n  no input; setup cannot continue")
		return empty, false
	}
	choice := strings.ToLower(strings.TrimSpace(sc.Text()))
	var envVar string
	switch choice {
	case "", "1", "openai":
		envVar = "OPENAI_API_KEY"
	case "2", "anthropic":
		envVar = "ANTHROPIC_API_KEY"
	case "3", "google", "gemini":
		envVar = "GEMINI_API_KEY"
	default:
		fmt.Fprintf(out, "  unknown provider %q; choose 1, 2, or 3 and re-run setup\n", choice)
		return empty, false
	}
	for _, p := range secret.ProviderKeyRefOrder {
		if p.EnvVar == envVar {
			return p, true
		}
	}
	return empty, false
}

// promptProviderRef prompts (once at a time, on a real TTY) for a NEW op://
// ref for a provider with none configured yet. It validates the ref resolves
// via `op read` to a non-empty value BEFORE returning it, and never echoes
// the resolved value. Empty input or EOF is a hard failure (a key is
// mandatory, not optional to skip); an invalid or unresolvable ref explains
// why and reprompts, up to providerKeyPromptAttempts, then fails.
func promptProviderRef(env hostenv.Env, sc *bufio.Scanner, out io.Writer, p secret.ProviderKeyRef) (ref, value string, ok bool) {
	for attempt := 1; attempt <= providerKeyPromptAttempts; attempt++ {
		fmt.Fprintf(out, "  %s: paste a 1Password ref (op://Vault/Item/field): ", p.Name)
		if !sc.Scan() {
			fmt.Fprintln(out, "")
			fmt.Fprintf(out, "  %s: no input — a 1Password ref is required; setup cannot continue.\n", p.Name)
			return "", "", false
		}
		ref = secret.NormalizeOpRef(sc.Text())
		if ref == "" {
			fmt.Fprintf(out, "    a ref is required for %s (it is not optional) — try again.\n", p.Name)
			continue
		}
		if !validOpRefSyntax(ref) {
			fmt.Fprintln(out, "    not a valid op:// ref (want op://Vault/Item/field) — try again.")
			continue
		}
		val, resolves := secret.OpReadNonEmpty(env, ref)
		if !resolves {
			fmt.Fprintf(out, "    could not resolve that ref for %s via `op read` (check the vault/item/field) — try again.\n", p.Name)
			continue
		}
		return ref, val, true
	}
	fmt.Fprintf(out, "  %s: too many invalid attempts — aborting setup.\n", p.Name)
	return "", "", false
}

// validOpRefSyntax requires the op:// prefix, rejects an unfilled
// <vault>/<item>/<field> placeholder, and rejects control characters —
// defense in depth beside op read's own validation (a pasted literal secret
// or a copy/paste artifact should never be written as if it were a ref).
func validOpRefSyntax(ref string) bool {
	if !strings.HasPrefix(ref, "op://") {
		return false
	}
	if secret.HasPlaceholder(ref) {
		return false
	}
	for _, r := range ref {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// identityMemory is the slice of the memory client seedIdentity needs,
// injectable via newIdentityMemory so tests can simulate per-call RPC
// failures without a live daemon.
type identityMemory interface {
	Up() bool
	Call(method string, params map[string]any) (map[string]any, error)
}

// newIdentityMemory is seedIdentity's seam to the memory daemon.
var newIdentityMemory = func() identityMemory { return rpc.MemoryClient() }

// rememberPersistedID extracts the "id" field from a remember RPC result,
// returning "" for anything that does not prove a durable write actually
// happened: a nil/empty result map, a missing "id" key, an "id" of the wrong
// type, or the daemon's own no-error-but-nothing-persisted response for empty
// content ({"id": "", "reaffirmed": false} — memory.go's remember handler).
// seedIdentity must count a save only when this returns non-empty; err == nil
// alone is not proof anything landed.
func rememberPersistedID(res map[string]any) string {
	id, _ := res["id"].(string)
	return id
}

// seedIdentity greets the user by name (from git config) and stores durable
// identity facts in memory (best-effort, only if the daemon is up), so their
// first session isn't anonymous. The trusted host-state payload injected into
// the onboarding kickoff prompt (see hoststate.go's injectTrustedHostState)
// also carries identity, built fresh at every launch, so "available to
// sessions via host state" is true regardless of the memory outcome. Each
// remember RPC is tracked individually: the output claims a memory save ONLY
// for writes that actually succeeded, is honest about a partial or failed
// batch, and never promises recall it can't guarantee.
func seedIdentity(env hostenv.Env, out io.Writer) {
	id := readGitIdentity(env)
	if id.Name == "" {
		return
	}
	who := id.Name
	// Store ONLY the first name (readGitIdentity already reduces to it). No
	// surname, no email: this fact is recalled into every session's context, so
	// it carries the minimum needed to greet, not a pile of PII. The warm
	// greeting itself belongs to the in-session agent, not this log.
	facts := []string{"The user's first name is " + id.Name + "."}
	attempted, saved := 0, 0
	if c := newIdentityMemory(); c.Up() {
		for _, f := range facts {
			attempted++
			res, err := c.Call("remember", map[string]any{"content": f, "source": "setup", "profile": "default"})
			// Count a save ONLY when the daemon's OWN result shape proves something
			// was actually persisted (a nonempty "id"), never merely because the RPC
			// didn't error. The real memory daemon's remember handler (memory.go)
			// returns {"id": id, "reaffirmed": bool} on a genuine write, but ALSO
			// returns a NO-ERROR {"id": "", "reaffirmed": false} for empty content —
			// a legitimate response that persisted NOTHING. Treating err==nil alone
			// as "saved" would silently over-claim in exactly that case.
			if err == nil && rememberPersistedID(res) != "" {
				saved++
			}
		}
	}
	switch {
	case attempted > 0 && saved == attempted:
		fmt.Fprintf(out, "\nIdentity (from your git config): %s. Saved to memory and available to sessions via host state.\n", who)
	case saved > 0:
		fmt.Fprintf(out, "\nIdentity (from your git config): %s. Only %d of %d facts saved to memory; identity is still available to sessions via host state.\n", who, saved, attempted)
	case attempted > 0:
		fmt.Fprintf(out, "\nIdentity (from your git config): %s. Could not save to memory; identity is still available to sessions via host state.\n", who)
	default:
		fmt.Fprintf(out, "\nIdentity (from your git config): %s. Available to sessions via host state.\n", who)
	}
}

// setupSandboxName derives the sandbox name `pix run` would use for dir
// (base name + active-profile suffix), so setup's guard can probe the SAME
// sandbox run would attach to. ok=false when the name can't be resolved (e.g. a
// unresolvable config) — the caller then skips the guard rather than blocking
// setup.
func setupSandboxName(dir string) (string, bool) {
	if _, err := config.Load(); err != nil {
		return "", false
	}
	return workspace.DeriveSandboxName(dir), true
}

// flagTakesValue reports whether an onboard flag consumes a following token
// (only the space-separated form; `--flag=value` is self-contained).
func flagTakesValue(a string) bool {
	switch a {
	case "--account", "--credentials", "--knowledge", "--mcp", "--model", "--models", "--pack", "--with":
		return true
	}
	return false
}

func normalizeSetupPackArg(arg string) string {
	arg = strings.TrimSpace(arg)
	if strings.Count(arg, "/") == 1 && !strings.Contains(arg, ":") && !strings.HasPrefix(arg, ".") && !strings.HasPrefix(arg, "~") {
		return "https://github.com/" + arg + ".git"
	}
	return arg
}

const setupUsage = `usage: pix setup [DIR] [host-config flags]

Sets up callable inference, then starts Pix. Ordinary setup begins with one
model-runtime choice. The selected path may then ask only for the credentials
or consent it actually needs. API keys are the default; a healthy existing
Ollama is offered; a custom gateway can use the current sbx login or no auth.
1Password is required only for the API-key path. Ollama is never installed
automatically.

Memory is progressive enhancement: it is enabled only when Ollama is healthy
and its watcher and embedding models are verified. Without Ollama, Pix still
runs normally with memory off. Host mode is separate and opt-in via
'pix host setup'.

DIR defaults to the current directory (like ` + "`pix run`" + `). Repeat semantics:
the host phase ALWAYS reconciles again, even when a sandbox
already exists for DIR. If one exists and you did not pass --replace, setup
leaves it alone (never force-removes it, never replays the tour into a live
session) and prints your choices: 'pix run [DIR]' to reattach, or
'pix setup [DIR] --replace' to recreate it with your current settings and
get the tour. Only a POSITIVELY absent sandbox gets the first-launch handoff;
if the sandbox state cannot be determined at all (sbx errored), setup fails
closed after the host phase — fix sbx and re-run.

Setup flags:
  --no-agent               run the HOST phase only: no sandbox, no handoff.
                           This is the scripted/CI path (it replaces the
                           removed ` + "`pix onboard`" + ` verb); --yes and
                           --non-interactive stay orthogonal to it
  --apply                  apply a pending .pix/onboarding.json in DIR
                           (the control-plane proposal an in-sandbox onboarding
                           agent wrote), under a confirmation gate
  --replace                recreate an existing sandbox for DIR (sbx rm -f +
                           create) so it picks up current pack/MCP/skills and
                           receives the guided tour; harmless when absent
  --verbose                show underlying sbx, Git, Docker, and setup command
                           output; ordinary setup prints only actions/results
  --pull-models            pull any CONFIRMED-missing configured local Ollama
                           models (watcher/embed/bridge, deduplicated); the
                           ONLY download consent a non-interactive setup honors
                           (a broad --yes never downloads). Interactive setup
                           may offer a default-No pull when an existing Ollama
                           positively lacks required memory models. Setup never installs
                           Ollama itself, and never pulls a tag it could not
                           positively verify as missing.
                           pix setup --pull-models with Ollama down exits
                           1. pix setup with the same Ollama down exits 0
                           with an optional ⚠ row. Stale optional config never
                           blocks unrelated repair.

Host-config flags (all optional):
  --pack <path|git+https-url#ref=branch|tag|sha>
                           activate a pack through the normal host trust gate,
                           then run its required, resumable setup hooks;
                           repeatable, composed in command order (collections
                           union; later scalar declarations win)
  --with <setup-id>        also run a named optional setup hook from --pack;
                           repeatable, and invalid without --pack
  --google-workspace       opt in to Google Workspace (absent otherwise): runs
                           the same transaction as 'pix gworkspace setup'
                           (may open a browser). Requires --account, and
                           --credentials unless the client was already imported
  --account <email>        the Google Workspace account to authorize; valid
                           ONLY with --google-workspace
  --credentials <path>     your Desktop OAuth client JSON; valid ONLY with
                           --google-workspace
  --knowledge <path|url>   scaffold/point the global knowledge base
  --mcp <name>             enable an MCP server (repeatable; allowlisted)
  --model <ollama-model>   set the ollama-bridge model
  --models <id,id,...>     restrict agents to these canonical catalog models;
                           interactive first setup otherwise offers every
                           model available through the selected runtime(s)
  --yes | --non-interactive  never prompt (CI); callable inference must already
                           be configured through provider refs, a pack/session
                           gateway, a no-auth gateway, or verified Ollama
  -h | --help              this help

Ordinary setup prints a short action-oriented transcript. --verbose exposes the
underlying eight phases — parse, inventory, gate, mutate, consent, verify,
report, handoff — and the commands they run for diagnosis. Setup sequences
prompts one at a time and never prompts at all without a TTY.
Mutations run in a fixed order with the
riskiest last (keys, config, pack, MCP, knowledge, identity, Google Workspace,
model pulls) and each one is individually idempotent, so an interrupted run is
resumed by re-running the same command: setup re-probes what is actually there
rather than reading back a journal of what it once intended.
`

// checkGoogleWorkspaceFlags enforces AC-P0-312: --account and --credentials are
// Google Workspace inputs, valid ONLY alongside --google-workspace. The error
// is deliberately in the standard grammar (invoked path, lowercase, no trailing
// period) and maps to exit 2 at the call site, because it is an argument
// mistake, not a failed probe.
func checkGoogleWorkspaceFlags(opts onboard.Opts) error {
	if opts.GoogleWorkspace {
		return nil
	}
	if strings.TrimSpace(opts.Account) != "" {
		return errUsage{fmt.Errorf("--account requires --google-workspace")}
	}
	if strings.TrimSpace(opts.Credentials) != "" {
		return errUsage{fmt.Errorf("--credentials requires --google-workspace")}
	}
	return nil
}

// errUsage marks an argument error, which exits 2 rather than 1.
type errUsage struct{ error }

// setupGoogleWorkspaceFn is the seam tests stub so setup's phases can be
// exercised without a browser or an installed dependency CLI. Production wires
// the real façade over the unchanged transaction.
var setupGoogleWorkspaceFn = func(env hostenv.Env, opts gogSetupOpts, in io.Reader, out io.Writer, interactive bool) error {
	return gworkspaceSetup(env, opts, in, out, interactive)
}
