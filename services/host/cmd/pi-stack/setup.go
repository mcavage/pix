// setup.go implements `pi-stack setup` — the explicit, guided onboarding entry.
//
// Owner decision (supersedes the in-`run` auto-offer): onboarding is a TWO-PHASE
// thing the user opts into by NAME.
//
//  1. HOST phase (here, on the host): source model keys from 1Password
//     (setupProvisionKeys) — op is REQUIRED and the only source; ensure the
//     memory service, create the default pack, and seed first-name identity.
//     Host mode is NOT set up here — it's opt-in via `pi-stack host setup`.
//     Host-config (gog/knowledge/mcp) comes from FLAGS, not interactive prompts;
//     the only interaction is pasting op:// refs on a TTY. Flag/non-TTY = CI-safe.
//  2. AGENT phase (handoff): launch a normal `pi-stack run` whose FIRST pi
//     message kicks off the `onboarding` skill, so the agent PROACTIVELY starts
//     the conversation (identity, tone, a real first task) instead of sitting
//     silent — the passive system-prompt marker never spoke until the user
//     typed, which is the bug this replaces.
//
// `pi-stack run` on its own NEVER onboards. `pi-stack onboard` is the host-only,
// no-handoff path for CI.
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"pi-stack/host/config"
)

// generatedInputMarker prefixes any user-role message that `pi-stack` itself
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
const generatedInputMarker = "[pi-stack-generated:onboarding] "

// onboardingKickoff is the first message `setup` hands the agent. It is
// DELIBERATELY short and human — it reads like something the user would type,
// not a machine directive wall. The rewritten `onboarding` skill owns the actual
// flow (guided teach, read host-state, land a task); the word "guided" is all it
// needs to pick GUIDED mode. (Making this fully invisible — agent greets with no
// visible prompt at all — needs a session-start extension + an image rebuild;
// tracked as a follow-up.) It carries generatedInputMarker so memory-capture.ts
// can tell this was machine-generated, not typed by the user.
const onboardingKickoff = generatedInputMarker + "I just ran pi-stack setup. Give me the upfront guide and help me get started."

// runSetupCmd is the `pi-stack setup` entry. It accepts the same host-config
// flags as `onboard` plus an optional DIR (default "."), runs the host phase,
// prints the handoff, then execs the run with the onboarding kickoff message.
func runSetupCmd(argv []string) {
	if wantsHelp(argv) {
		fmt.Print(setupUsage)
		return
	}

	// Split an optional positional DIR from the onboard-style flags. DIR is the
	// single non-flag token; everything else is forwarded to the host phase.
	// --replace is SETUP'S OWN flag (recreate an existing sandbox and hand it
	// the tour): consumed here, never forwarded to parseOnboardArgs — it is not
	// host config.
	dir := "."
	dirSet := false
	replace := false
	var hostArgs []string
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--replace" {
			replace = true
			continue
		}
		if len(a) > 0 && a[0] != '-' {
			if dirSet {
				fmt.Fprintf(os.Stderr, "pi-stack setup: too many directories (%q and %q); pass at most one DIR\n", dir, a)
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

	// Phase 1: host config — source keys from 1Password, ensure memory, create the
	// pack, seed identity, provision+enable host mode (see setupHostPhase). This
	// ALWAYS runs first, regardless of whether a sandbox already exists for dir —
	// `pi-stack setup` run a second time must still reconcile host keys/config
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
	if err := runSetupCore(env, dir, hostArgs, os.Stdin, os.Stdout, isTTY(os.Stdin), setupHostPhase); err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack setup: %v\n", err)
		os.Exit(1)
	}

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
		fmt.Fprintf(os.Stderr, "pi-stack setup: %v\n", err)
		os.Exit(1)
	}
}

// runSetupCore validates DIR (reusing validateRunWorkspace's exists-and-is-a-
// directory check — the same rule `pi-stack run` enforces, so setup and run
// never disagree about what counts as a launchable DIR) and, ONLY if that
// passes, invokes hostPhase. Extracted as its own tiny function — rather than
// inlining the check in runSetupCmd — so a nonexistent/file DIR is provably
// caught BEFORE hostPhase (which mutates op-refs.env/hostmode.env/config.toml/
// the default pack/memory/host-mode) ever runs: a test can pass a hostPhase
// stub that fails the test if called, and assert on the returned error alone,
// without needing to exercise runSetupCmd's os.Exit calls.
func runSetupCore(env shellEnv, dir string, hostArgs []string, in io.Reader, out io.Writer, tty bool, hostPhase func(shellEnv, []string, io.Reader, io.Writer, bool) error) error {
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
	// when explicit so `pi-stack setup` from inside a repo behaves exactly
	// like `pi-stack run` there. --replace is harmless on an absent sandbox
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
		dirArg = " " + shellQuoteArg(dir)
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
		return fmt.Errorf("cannot determine the state of %s (`sbx ls` failed or sbx is unavailable). Host setup completed; fix sbx and retry with: pi-stack setup%s", which, retryArg)
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
		fmt.Fprintf(out, "  pi-stack run%s              # reattach as-is\n", dirArg)
		fmt.Fprintf(out, "  pi-stack setup%s --replace  # recreate with current settings + get the tour\n", dirArg)
		return nil
	}

	// sbxAbsent (positively confirmed): normal first launch — hand off to the
	// in-VM onboarding agent via an initial message. A --replace here is
	// harmless (the create path ignores it).
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Launching sandbox — pi will introduce itself, show you how it works,")
	fmt.Fprintln(out, "and get you into a real task. (You can quit any time; just run `pi-stack run`.)")
	runFn(kickoffArgs())
	return nil
}

// shellQuoteArg quotes s for safe copy-paste into a POSIX shell: a token made
// only of clearly-safe characters passes through untouched; anything else is
// single-quoted using the standard close-escape-reopen sequence for embedded
// apostrophes, so a DIR like `my repo's` round-trips exactly.
func shellQuoteArg(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.' || r == '/' || r == ':' || r == '@' || r == '%' || r == '+' || r == ',' || r == '=':
		default:
			safe = false
		}
		if !safe {
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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

func setupHostPhase(env shellEnv, flags []string, in io.Reader, out io.Writer, tty bool) error {
	fmt.Fprintln(out, "pi-stack setup — configuring the host")
	fmt.Fprintln(out, "")

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	opts, perr := parseOnboardArgs(flags)
	if perr != nil {
		return perr
	}
	if opts.apply {
		return fmt.Errorf("--apply belongs to `pi-stack onboard --apply`; setup does not apply workspace proposals")
	}
	// Interactive prompts fire on any real TTY unless the caller explicitly opted
	// out with --yes/-y/--non-interactive (opts.assumeYes). Ordinary VALUE flags
	// (--account/--knowledge/--mcp/--model) configure host settings; they say
	// nothing about whether pasting a 1Password ref should still prompt, so their
	// mere presence must NOT silently suppress the key-collection/overwrite
	// prompts — only an explicit non-interactive opt-out does.
	interactive := setupInteractivePrompts(tty, opts.assumeYes)

	// Build + VALIDATE the onboarding proposal from the flags BEFORE anything
	// touches provider keys or host state. An invalid --mcp/--knowledge/--model
	// must fail setup immediately, with NOTHING done yet — no 1Password
	// prompts, no ref writes to op-refs.env/hostmode.env, no sbx reconciliation
	// — rather than running the entire (expensive, side-effecting) provider-key
	// flow first only to reject the very flags that drove this run afterward.
	r := &onboardingResult{
		Version:           1,
		GogAccount:        strings.TrimSpace(opts.account),
		MCP:               opts.mcp,
		OllamaBridgeModel: strings.TrimSpace(opts.model),
	}
	if k := strings.TrimSpace(opts.knowledge); k != "" {
		r.Knowledge = &onboardKnowledge{Action: "use", Source: k}
	}
	if err := validateOnboardingResult(r, cfg, env, hostBinaryResolver); err != nil {
		return err
	}

	// Retired config keys (mcp_static/mcp_dynamic) are dropped by the sparse
	// encode whenever the config is saved. Announce the drop ONCE, concisely,
	// and perform the save right here so the migration is deterministic even if
	// a later step fails. UNKNOWN keys are a different thing (cfg.UnknownKeys —
	// doctor flags those) and are never swept into this notice as "retired".
	if retired := cfg.RetiredKeys(); len(retired) > 0 {
		fmt.Fprintf(out, "note: dropping retired config key(s) %s on save — no longer read; every configured MCP server preloads at sandbox create\n", strings.Join(retired, ", "))
		if err := cfg.Save(); err != nil {
			return fmt.Errorf("dropping retired config keys: %w", err)
		}
	}

	// Provider keys come from 1Password, the only source: setupProvisionKeysFn
	// requires `op` installed + signed in, collects+validates an op:// ref for
	// every provider (prompting once each on a TTY; printing exact `pi-stack
	// secret set` commands otherwise), mirrors them into hostmode.env, and
	// reconciles sbx to match. It prints exactly what's wrong on failure; abort
	// setup rather than hand off to a session that can't talk to a model.
	if !setupProvisionKeysFn(env, in, out, interactive, opts.assumeYes) {
		return fmt.Errorf("provider keys not fully configured — follow the fix printed above")
	}

	changes, err := applyOnboardingResult(r, cfg, env, out, func(c *config.Config) error { return c.Save() })
	if err != nil {
		return err
	}

	// Ensure a default pack exists (git-init'd) so authored skills + captured
	// knowledge have a durable, versioned home the onboarding agent can point at,
	// AND that it is ACTIVE (cfg.Pack) whenever cfg.Pack is currently empty —
	// including a pack that already exists (a migrated legacy dir, or one
	// discovered/created by an earlier run whose activation never landed), not
	// only a brand-new one. runPackNew handles both creation AND activation for
	// the fresh case; the already-exists branch must activate explicitly, since
	// runPackNew is never called there. A real activation failure (cfg.Save
	// error) FAILS setup — it must never report success while cfg.Pack still
	// points nowhere. Does NOT override an explicitly active alternate pack
	// (activateDefaultPack no-ops when cfg.Pack != "").
	defaultRoot := defaultPackRoot() // runs the legacy pack/personal -> default migration
	if _, err := os.Stat(filepath.Join(defaultRoot, packManifestName)); err != nil {
		runPackNew(env, out, []string{defaultRoot})
	} else if err := activateDefaultPack(defaultRoot); err != nil {
		return fmt.Errorf("ensuring default pack is active: %w", err)
	}

	fmt.Fprintln(out, "")
	fmt.Fprintf(out, "memory service: enabled (:%d)\n", memoryPortDefault)
	if len(changes) == 0 {
		fmt.Fprintln(out, "knowledge:      (none) — add later with `pi-stack knowledge init` / `use`")
	} else {
		for _, c := range changes {
			fmt.Fprintf(out, "  + %s\n", c)
		}
	}
	if len(cfg.MCP) > 0 {
		if err := registerServers(cfg, env, out, nil, hostBinaryResolver, activeContainerMCP(cfg)); err != nil {
			fmt.Fprintf(out, "  mcp register skipped: %v (finish later: pi-stack mcp register)\n", err)
		}
	}

	// Local models (optional): probe Ollama ONCE, classify on the shared
	// ModelReadiness axes, pull confirmed-missing tags only under explicit
	// consent (--pull-models, or the interactive default-No prompt), verify
	// once after the pulls, and receipt the outcome into launcher state. Never
	// installs Ollama; a partial pull failure fails setup below, AFTER the
	// truthful summary has printed.
	models := setupLocalModels(cfg, env, in, out, interactive, opts.pullModels)
	receiptSetupModels(env, out, models)

	// Identity: read it from the HOST's git config (the sandbox can't see
	// ~/.gitconfig) and seed it so onboarding can greet by name. The generated
	// kickoff carries it deterministically; memory writes remain best-effort.
	seedIdentity(env, out)

	// Host mode (the UNSANDBOXED escape hatch) is NOT set up here — it's opt-in and
	// only relevant to some users, and provisioning it needs `pi` on PATH, which a
	// normal sandbox-only user won't have. Point at the dedicated command instead of
	// running (and noisily half-failing) it on every `pi-stack setup`.
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "host mode (optional, UNSANDBOXED — runs `pi` directly on the host): not enabled.")
	fmt.Fprintln(out, "  set it up only if you need it:  pi-stack host setup")

	// Completion summary: keys / knowledge / pack / local models / gog on
	// separate readiness axes, then the core-provisioned line.
	printSetupSummary(cfg, env, out, models)

	// A partial pull failure is a real, verified gap the user consented to
	// closing: fail setup (non-zero) with the exact retry commands. The summary
	// above already reported it truthfully.
	if len(models.failed) > 0 {
		return fmt.Errorf("local model pull failed for %s — retry by hand: ollama pull %s, then re-run pi-stack setup",
			strings.Join(models.failed, ", "), strings.Join(models.failed, "; ollama pull "))
	}
	return nil
}

// providerKeyPromptAttempts caps how many times setupProvisionKeys reprompts
// for a single provider's ref before giving up, since a human who keeps
// mistyping (or an unattended TTY feeding garbage) must not hang setup
// forever.
const providerKeyPromptAttempts = 3

// setupProvisionKeys sources model provider keys (Anthropic/OpenAI/Google) from
// 1Password and reconciles them into the sbx secret store. This is the ONLY
// provider-key path: op is required, and the removed --use-sbx-keys flag,
// persisted provider_key_mode, and the "already in sbx?" convenience prompt are
// gone. Returns whether all keys ended up usable.
//
// Step 0 (hard preconditions): `op` must be installed AND signed in. Without
// either there is nothing to source keys from — fail setup with the exact fix,
// before pack/host/onboarding ever run.
//
// Step 1 (collect + validate every ref): for each provider, in order
// (providerKeyRefOrder):
//   - a ref already configured (op-refs.env OR hostmode.env, via currentOpRef)
//     is CONFIRMED, not re-solicited — but it still must resolve via `op read`
//     to a non-empty value; a broken existing ref fails setup outright.
//   - a ref with NO configuration yet, on an interactive TTY, is prompted for
//     one at a time. Empty input or EOF is NOT "skip" — a key is mandatory, so
//     that fails setup. An invalid/unresolvable ref reprompts (capped at
//     providerKeyPromptAttempts).
//   - a ref with NO configuration and NO interactive TTY prints the exact
//     `pi-stack secret set` command per missing provider and fails setup.
//
// Step 2 (mirror + verify): every validated ref is written to BOTH op-refs.env
// (sandbox) and hostmode.env (host mode); setup then verifies all three landed
// in hostmode.env.
//
// Step 3 (reconcile sbx): reconcileProviderKeysWithSbx brings sbx to the same
// state as the validated refs, fed the Step-1 snapshot. A reconcile failure
// fails setup. Step 4 (final probe) requires all three keys usable in sbx.
//
// Steps 1-3 run holding the provider-refs transaction lock so a concurrent
// `pi-stack secret set`/`secret rm` cannot interleave. Never persists or prints
// a resolved secret value.
//
// setupProvisionKeysFn is the seam setupHostPhase calls through (a package var
// so a test can replace it with a stub).
var setupProvisionKeysFn = setupProvisionKeys

// setupProvisionKeys resolves provider keys from 1Password (the strict, and now
// only, flow) and returns whether it succeeded.
func setupProvisionKeys(env shellEnv, in io.Reader, out io.Writer, interactive, assumeYes bool) bool {
	return runStrictProviderKeyFlow(env, bufio.NewScanner(in), out, interactive, assumeYes)
}

// runStrictProviderKeyFlow resolves every provider key from 1Password and
// reconciles it into sbx (Steps 0-4 documented on setupProvisionKeys). It is the
// only provider-key path now that the sbx-keys shortcut is gone.
func runStrictProviderKeyFlow(env shellEnv, sc *bufio.Scanner, out io.Writer, interactive, assumeYes bool) bool {
	fmt.Fprintln(out, "")

	if !opInstalled(env) {
		fmt.Fprintln(out, "1Password provider setup requires the `op` CLI, but it isn't installed.")
		fmt.Fprintln(out, "Install it (https://developer.1password.com/docs/cli/) and re-run the same setup command.")
		return false
	}
	if !opSignedIn(env) {
		fmt.Fprintln(out, "`op` is installed but no 1Password account is configured.")
		fmt.Fprintln(out, "Run `op signin` (or add an account in the 1Password app) and re-run the same setup command.")
		return false
	}

	// Hold the provider-refs transaction lock across the WHOLE flow: initial
	// ref reads/validation, the canonical both-file writes, the hostmode
	// verification, and the sbx reconciliation + synced-ref metadata. A lock
	// acquisition failure fails setup honestly — never proceed unlocked.
	ok := false
	if lerr := withProviderRefsLock(env, func() error {
		ok = strictProviderKeyFlowLocked(env, sc, out, interactive, assumeYes)
		return nil
	}); lerr != nil {
		fmt.Fprintf(out, "  \u2717 could not lock provider refs (%s): %v — another pi-stack credential operation may hold it; fix that and re-run the same setup command.\n", providerRefsLockPath(env), lerr)
		return false
	}
	return ok
}

// strictProviderKeyFlowLocked is runStrictProviderKeyFlow's transaction body
// (Steps 1-4). Caller MUST hold the provider-refs lock; every refs-file write
// in here goes through a *Locked variant for exactly that reason.
func strictProviderKeyFlowLocked(env shellEnv, sc *bufio.Scanner, out io.Writer, interactive, assumeYes bool) bool {
	// refs is the validated snapshot reconcile (STEP 3) works from (envVar ->
	// op:// ref — every entry validated AND canonical-written to both files
	// below); resolved caches each provider's validated op-read value so
	// reconcile never pays for a second `op read` of the same ref.
	refs := make(map[string]string, len(providerKeyRefOrder))
	resolved := make(map[string]string, len(providerKeyRefOrder))
	var missingNonInteractive []struct{ envVar, name string }

	for _, p := range providerKeyRefOrder {
		ref, hasRef := currentOpRef(env, p.envVar)
		switch {
		case hasRef:
			val, ok := opReadNonEmpty(env, ref)
			if !ok {
				fmt.Fprintf(out, "  %s \u2717 configured 1Password ref does not resolve (op read failed or empty)\n", p.name)
				fmt.Fprintf(out, "    fix it: pi-stack secret set %s op://Vault/Item/field\n", p.envVar)
				return false
			}
			// currentOpRef may have found this ref in EITHER file (op-refs.env OR
			// hostmode.env), not necessarily both. Idempotently upsert it into BOTH
			// here — a no-op where it already matches — and FAIL setup if either
			// write errors, rather than silently backfilling one file and calling
			// that success (the bug: a ref found only in hostmode.env must not be
			// allowed to leave op-refs.env permanently missing it).
			if err := writeOpRefQuietLocked(env, p.envVar, ref); err != nil {
				fmt.Fprintf(out, "  %s \u2717 could not write ref to op-refs.env: %v\n", p.name, err)
				return false
			}
			if err := writeOpRefFileQuietLocked(env, hostModeRefsPath(env), p.envVar, ref); err != nil {
				fmt.Fprintf(out, "  %s \u2717 could not write ref to hostmode.env: %v\n", p.name, err)
				return false
			}
			fmt.Fprintf(out, "  %s \u2713 1Password ref configured\n", p.name)
			refs[p.envVar] = ref
			resolved[p.envVar] = val
		case interactive:
			ref, val, ok := promptProviderRef(env, sc, out, p)
			if !ok {
				return false
			}
			if err := writeOpRefQuietLocked(env, p.envVar, ref); err != nil {
				fmt.Fprintf(out, "  %s \u2717 could not save ref: %v\n", p.name, err)
				return false
			}
			if err := writeOpRefFileQuietLocked(env, hostModeRefsPath(env), p.envVar, ref); err != nil {
				fmt.Fprintf(out, "  %s \u2717 could not save host-mode ref: %v\n", p.name, err)
				return false
			}
			fmt.Fprintf(out, "  %s \u2713 saved\n", p.name)
			refs[p.envVar] = ref
			resolved[p.envVar] = val
		default:
			missingNonInteractive = append(missingNonInteractive, p)
		}
	}

	if len(missingNonInteractive) > 0 {
		fmt.Fprintln(out, "Missing required 1Password refs for:")
		for _, p := range missingNonInteractive {
			fmt.Fprintf(out, "  pi-stack secret set %s op://Vault/Item/field\n", p.envVar)
		}
		// `pi-stack secret set` mirrors a provider key into BOTH op-refs.env AND
		// hostmode.env, so the three commands above really are enough — no extra
		// step needed before re-running setup.
		fmt.Fprintln(out, "then re-run: pi-stack setup")
		return false
	}

	// Every validated ref was already canonical-written to BOTH files above, so
	// there is no reread-and-remirror pass here — just the final membership
	// verification that all three landed in hostmode.env (host mode reads ONLY
	// hostmode.env via `op run --env-file`, never op-refs.env).
	got, kerr := hostModeProviderKeys(env)
	if kerr != nil {
		fmt.Fprintf(out, "  \u2717 credential state unreadable: %v\n", kerr)
		return false
	}
	if !hasAllProviderKeyNames(got) {
		// Compare the EXACT required set, not a length — hostModeProviderKeys
		// already dedupes by provider name, but the completeness check itself
		// must never accept "the count matches" as a proxy for "every provider is
		// actually present".
		fmt.Fprintf(out, "  \u2717 hostmode.env has %v after mirroring, want all of %v\n", got, modelProviders)
		return false
	}

	if !reconcileProviderKeysWithSbx(env, sc, out, interactive, assumeYes, refs, resolved) {
		return false
	}

	// Tri-state: only abort setup when we can POSITIVELY confirm sbx is missing
	// a key (sbxSecretsOK). sbx being entirely ABSENT is portability — fail
	// open, we can't tell. sbx being installed but the check command FAILING is
	// a real, diagnosable problem — fail CLOSED with a message, never silently
	// pass a box whose completeness we couldn't actually verify.
	allPresent, state := sbxAllModelKeysPresent(env)
	switch state {
	case sbxSecretsAbsent:
		return true
	case sbxSecretsError:
		fmt.Fprintln(out, "  \u2717 could not verify sbx has all three provider keys (`sbx secret ls` failed) \u2014 check sbx and re-run the same setup command")
		return false
	}
	return allPresent
}

// promptProviderRef prompts (once at a time, on a real TTY) for a NEW op://
// ref for a provider with none configured yet. It validates the ref resolves
// via `op read` to a non-empty value BEFORE returning it, and never echoes
// the resolved value. Empty input or EOF is a hard failure (a key is
// mandatory, not optional to skip); an invalid or unresolvable ref explains
// why and reprompts, up to providerKeyPromptAttempts, then fails.
func promptProviderRef(env shellEnv, sc *bufio.Scanner, out io.Writer, p struct{ envVar, name string }) (ref, value string, ok bool) {
	for attempt := 1; attempt <= providerKeyPromptAttempts; attempt++ {
		fmt.Fprintf(out, "  %s: paste a 1Password ref (op://Vault/Item/field): ", p.name)
		if !sc.Scan() {
			fmt.Fprintln(out, "")
			fmt.Fprintf(out, "  %s: no input — a 1Password ref is required; setup cannot continue.\n", p.name)
			return "", "", false
		}
		ref = normalizeOpRef(sc.Text())
		if ref == "" {
			fmt.Fprintf(out, "    a ref is required for %s (it is not optional) — try again.\n", p.name)
			continue
		}
		if !validOpRefSyntax(ref) {
			fmt.Fprintln(out, "    not a valid op:// ref (want op://Vault/Item/field) — try again.")
			continue
		}
		val, resolves := opReadNonEmpty(env, ref)
		if !resolves {
			fmt.Fprintf(out, "    could not resolve that ref for %s via `op read` (check the vault/item/field) — try again.\n", p.name)
			continue
		}
		return ref, val, true
	}
	fmt.Fprintf(out, "  %s: too many invalid attempts — aborting setup.\n", p.name)
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
	if hasPlaceholder(ref) {
		return false
	}
	for _, r := range ref {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// currentOpRef returns the current FILLED op:// ref for a provider env var. It
// checks op-refs.env (sandbox) AND hostmode.env (host mode): a ref given via
// EITHER path counts, so setup never re-prompts for a ref the user already
// provided in one file but not the other. Pure/read-only — it does NOT write
// anything: a ref found only in hostmode.env is backfilled into op-refs.env by
// the caller (setupProvisionKeys' hasRef branch), which writes to BOTH files
// and fails setup outright if either write errors, rather than this function
// doing a silent best-effort backfill whose failure nobody checks.
func currentOpRef(env shellEnv, envVar string) (string, bool) {
	if _, content, exists := opRefsContent(env); exists {
		for _, r := range parseOpRefs(content) {
			if r.key == envVar && r.isRef && !r.placeholder {
				return r.value, true
			}
		}
	}
	if env.readFile != nil {
		if content, err := env.readFile(hostModeRefsPath(env)); err == nil {
			for _, r := range parseOpRefs(content) {
				if r.key == envVar && r.isRef && !r.placeholder {
					return r.value, true
				}
			}
		}
	}
	return "", false
}

// identityMemory is the slice of the memory client seedIdentity needs,
// injectable via newIdentityMemory so tests can simulate per-call RPC
// failures without a live daemon.
type identityMemory interface {
	Up() bool
	Call(method string, params map[string]any) (map[string]any, error)
}

// newIdentityMemory is seedIdentity's seam to the memory daemon.
var newIdentityMemory = func() identityMemory { return memoryClient() }

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
func seedIdentity(env shellEnv, out io.Writer) {
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

// setupSandboxName derives the sandbox name `pi-stack run` would use for dir
// (base name + active-profile suffix), so setup's guard can probe the SAME
// sandbox run would attach to. ok=false when the name can't be resolved (e.g. a
// unresolvable config) — the caller then skips the guard rather than blocking
// setup.
func setupSandboxName(dir string) (string, bool) {
	if _, err := config.Load(); err != nil {
		return "", false
	}
	return deriveSandboxName(dir), true
}

// flagTakesValue reports whether an onboard flag consumes a following token
// (only the space-separated form; `--flag=value` is self-contained).
func flagTakesValue(a string) bool {
	switch a {
	case "--account", "--knowledge", "--mcp", "--model":
		return true
	}
	return false
}

const setupUsage = `usage: pi-stack setup [DIR] [host-config flags]

Actually sets you up (use 'pi-stack run' if you just want to start working):
  1. host   — resolve model keys from 1Password and reconcile them into sbx
              (op is REQUIRED), wiring BOTH the sandbox and host mode's
              hostmode.env; ensure memory; create your default pack
  2. agent  - launch a sandbox and hand off to a ONE-SHOT upfront guide that
              names the exact workflows, explains memory and packs, reports
              grounded setup gaps, then asks for your real task

Provider keys come from 1Password only — the ` + "`op`" + ` CLI must be installed and
signed in, or setup fails with the exact fix. There is no "trust existing sbx
keys" shortcut. Host mode (pi UNSANDBOXED) is NOT set up here; it's opt-in via
'pi-stack host setup' (which provisions AND enables it in one step).

DIR defaults to the current directory (like ` + "`pi-stack run`" + `). Repeat semantics:
the host phase (keys/memory/pack) ALWAYS reconciles again, even when a sandbox
already exists for DIR. If one exists and you did not pass --replace, setup
leaves it alone (never force-removes it, never replays the tour into a live
session) and prints your choices: 'pi-stack run [DIR]' to reattach, or
'pi-stack setup [DIR] --replace' to recreate it with your current settings and
get the tour. Only a POSITIVELY absent sandbox gets the first-launch handoff;
if the sandbox state cannot be determined at all (sbx errored), setup fails
closed after the host phase — fix sbx and re-run.

Setup flags:
  --replace                recreate an existing sandbox for DIR (sbx rm -f +
                           create) so it picks up current pack/MCP/skills and
                           receives the guided tour; harmless when absent
  --pull-models            pull any CONFIRMED-missing configured local Ollama
                           models (watcher/embed/bridge, deduplicated) — the
                           ONLY consent a non-interactive setup honors (a broad
                           --yes never downloads). Interactive setup without it
                           asks once, defaulting to No. Setup never installs
                           Ollama itself, and never pulls a tag it could not
                           positively verify as missing.

Host-config flags (all optional):
  --account <email>        set the Google Workspace (gog) account + enable gog
  --knowledge <path|url>   scaffold/point the global knowledge base
  --mcp <name>             enable an MCP server (repeatable; allowlisted)
  --model <ollama-model>   set the ollama-bridge model
  --yes | --non-interactive  never prompt (CI); each provider's op:// ref must
                           already be configured and resolve (setup prints the
                           exact 'pi-stack secret set' command for any missing)
  -h | --help              this help

For scripted host config with NO agent handoff, use ` + "`pi-stack onboard`" + ` instead.
`
