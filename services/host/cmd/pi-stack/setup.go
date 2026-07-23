// setup.go implements `pi-stack setup` — the explicit, guided onboarding entry.
//
// Owner decision (supersedes the in-`run` auto-offer): onboarding is a TWO-PHASE
// thing the user opts into by NAME.
//
//  1. HOST phase (here, on the host): source model keys, preferring 1Password
//     (setupProvisionKeys), but accepting a complete existing sbx key set via
//     --use-sbx-keys or a one-time interactive prompt; ensure the memory
//     service, create the default pack, seed git identity, and ALWAYS provision
//     + enable host mode when it can.
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

	// Provider keys PREFER 1Password (never merely trusting whatever sbx already
	// has, on the normal path): provisionProviderKeysFn collects+validates an
	// op:// ref for every provider (prompting once each on a TTY; printing exact
	// `pi-stack secret set` commands otherwise), mirrors them into hostmode.env,
	// and reconciles sbx to match. Three ways to skip 1Password for a run: the
	// explicit --use-sbx-keys flag, a PERSISTED mode of "sbx" from a prior
	// successful run (cfg.ProviderKeyMode; an explicit --use-1password overrides
	// it back to strict for this run), or (interactive only, mode unset, no ref
	// configured yet) accepting the one-time convenience prompt. Every path still
	// requires an EXACT successful sbx probe with all three keys present; none
	// trusts a partial or unprobeable sbx. It already prints exactly what's
	// wrong; abort setup on failure rather than hand off to a session that can't
	// talk to a model. Only reached now that the flags themselves are known-valid
	// (parseOnboardArgs already rejected --use-sbx-keys + --use-1password
	// together).
	keyResult := provisionProviderKeysFn(env, in, out, interactive, opts.assumeYes, opts.useSbxKeys, opts.use1Password, cfg.ProviderKeyMode)
	if !keyResult.OK {
		// Not always "re-run the same setup command": the fix printed above may
		// instead be `sbx secret set` for a missing key, `pi-stack setup
		// --use-1password`, or `pi-stack config unset provider_key_mode` — so
		// point at that, not a single fixed rerun command.
		return fmt.Errorf("provider keys not fully configured — follow the fix printed above")
	}
	// Persist the mode that just succeeded so a repeat run doesn't re-litigate
	// the choice (an unchanged value is a harmless no-op write, skipped to avoid
	// a pointless Save on every ordinary re-run). A save failure here is NOT
	// swallowed: setup must never report success while claiming a mode it could
	// not actually persist.
	if keyResult.Mode != "" && keyResult.Mode != cfg.ProviderKeyMode {
		cfg.ProviderKeyMode = keyResult.Mode
		if err := cfg.Save(); err != nil {
			return fmt.Errorf("provider keys configured, but could not persist provider_key_mode=%s: %w", keyResult.Mode, err)
		}
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
		if err := registerServers(cfg, env, out, nil, hostBinaryResolver); err != nil {
			fmt.Fprintf(out, "  mcp register skipped: %v (finish later: pi-stack mcp register)\n", err)
		}
	}

	// Identity: read it from the HOST's git config (the sandbox can't see
	// ~/.gitconfig) and seed it so onboarding can greet by name. The generated
	// kickoff carries it deterministically; memory writes remain best-effort.
	seedIdentity(env, out)

	// Host mode: setup ALWAYS sets it up (owner decision) — provision + enable, no
	// prompt. Its cloud keys are the same hostmode.env op:// refs written above.
	// provision-before-enable stays, so a `pi`-less box just stays disabled.
	setupHostMode(env, out, keyResult)
	return nil
}

// providerKeyPromptAttempts caps how many times setupProvisionKeys reprompts
// for a single provider's ref before giving up, since a human who keeps
// mistyping (or an unattended TTY feeding garbage) must not hang setup
// forever.
const providerKeyPromptAttempts = 3

// sbxKeysConveniencePrompt is the exact, single, one-time question offered on
// an interactive TTY when sbx already has all three provider keys AND no
// provider ref is configured yet (see probeProviderKeyRefs). It never
// repeats within a run and never fires once any ref exists.
const sbxKeysConveniencePrompt = "sbx already has anthropic, openai, and google. Use those keys and skip 1Password-backed host credentials? [Y/n]: "

// providerKeyRefsProbeState is the tri-state result of probeProviderKeyRefs:
// unlike this helper's bool-returning predecessor (which silently treated
// ANY read failure as "no refs"), a genuine read error (permission denied, an
// unreadable symlink, a real I/O failure) is distinguished from a PROVEN
// absence, so the caller never masks a real problem as a clean "nothing
// configured yet".
type providerKeyRefsProbeState int

const (
	providerKeyRefsProbeNone  providerKeyRefsProbeState = iota // proven: neither file has a configured provider ref
	providerKeyRefsProbeSome                                   // at least one provider ref is configured
	providerKeyRefsProbeError                                  // a file could not be read/parsed safely
)

// probeProviderKeyRefs reads op-refs.env AND hostmode.env through env's
// injected readFile and classifies whether any provider-key ref is already
// configured, as a tri-state: ENOENT on a file means that file is absent (the
// OTHER file might still carry the ref, or neither does — a normal, common
// case), but any OTHER read error is NOT the same as absence and must never be
// silently treated as "no refs configured" — the caller (the interactive
// convenience-prompt gate) would otherwise offer to skip 1Password while
// masking a real problem reading credentials state. This gates the interactive
// convenience prompt: once any ref exists, the user has already made a
// 1Password choice, so the prompt must not resurface it.
func probeProviderKeyRefs(env shellEnv) (state providerKeyRefsProbeState, path string, err error) {
	check := func(p string) (found bool, readErr error) {
		if env.readFile == nil {
			return false, nil
		}
		content, rerr := env.readFile(p)
		if rerr != nil {
			if os.IsNotExist(rerr) {
				return false, nil
			}
			return false, rerr
		}
		for _, r := range parseOpRefs(content) {
			if _, ok := providerKeyRefs[r.key]; ok && r.isRef && !r.placeholder {
				return true, nil
			}
		}
		return false, nil
	}

	opPath := defaultOpRefsPath(env)
	if found, rerr := check(opPath); rerr != nil {
		return providerKeyRefsProbeError, opPath, rerr
	} else if found {
		return providerKeyRefsProbeSome, "", nil
	}

	hmPath := hostModeRefsPath(env)
	if found, rerr := check(hmPath); rerr != nil {
		return providerKeyRefsProbeError, hmPath, rerr
	} else if found {
		return providerKeyRefsProbeSome, "", nil
	}
	return providerKeyRefsProbeNone, "", nil
}

// missingModelProviders returns the subset of modelProviders (anthropic,
// openai, google — order preserved) NOT present in a `sbx secret ls` output,
// shared by acceptExistingSbxKeys and providerKeyFlow's partial-sbx gate so
// both name the exact same missing set from the same probe output.
func missingModelProviders(sbxOut string) []string {
	var missing []string
	for _, name := range modelProviders {
		if !grepWord(sbxOut, name) {
			missing = append(missing, name)
		}
	}
	return missing
}

// acceptExistingSbxKeys is the SKIP-1Password path. It requires an EXACT
// successful sbx probe reporting ALL THREE provider keys present, never a
// partial match, and never a fail-open on an absent/unreachable sbx (that
// leniency belongs only to the strict flow's final probe, where sbx was never
// the thing being trusted). It never touches op, op-refs.env, hostmode.env, or
// the synced-ref record: existing refs and synced metadata are left exactly as
// they are, simply unused for this run. On success it prints ONE compact,
// honest status line about ONLY what this function itself did (the sandbox now
// uses existing sbx keys; 1Password reconciliation was skipped) — it does NOT
// speak for host mode's cloud-key status (local/Ollama-only vs configured):
// setupHostMode owns that ONE status line, so the two never risk printing
// conflicting claims about the same thing. On an incomplete sbx it names the
// EXACT missing provider(s) from the probe output, not a vague "at least one"
// (unwritten copy, unwatched here, at least once printed a confidently wrong
// culprit — a set-membership check against the real probe output is the fix).
//
// The copy here is deliberately SOURCE-AGNOSTIC: this path fires for an
// explicit --use-sbx-keys flag, a persisted provider_key_mode=sbx from a
// PRIOR run, or an accepted interactive convenience prompt — in the latter
// two cases the user typed nothing this run, so wording like "drop the flag"
// would misname something they never passed. persisted names whether a
// prior-run persisted mode is the reason this path is running (as opposed to
// an explicit flag or a fresh convenience-prompt accept); only then is
// `pi-stack config unset provider_key_mode` an actually useful thing to
// suggest.
func acceptExistingSbxKeys(env shellEnv, out io.Writer, persisted bool) bool {
	sbxOut, state := probeSbxSecrets(env)
	switch state {
	case sbxSecretsAbsent:
		fmt.Fprintln(out, "sbx isn't installed or reachable here, so its provider keys can't be used.")
		fmt.Fprintln(out, "Install/configure sbx and re-run, or use: pi-stack setup --use-1password")
		return false
	case sbxSecretsError:
		fmt.Fprintln(out, "could not verify sbx's provider keys (`sbx secret ls` failed).")
		fmt.Fprintln(out, "Check sbx and re-run, or use: pi-stack setup --use-1password")
		return false
	}
	if missing := missingModelProviders(sbxOut); len(missing) > 0 {
		fmt.Fprintf(out, "sbx is missing provider key(s): %s.\n", strings.Join(missing, ", "))
		fmt.Fprintln(out, "Choose one:")
		for _, name := range missing {
			fmt.Fprintf(out, "  sbx secret set -g %s -t <value>          # restore it in sbx, then re-run setup\n", name)
		}
		fmt.Fprintln(out, "  pi-stack setup --use-1password           # source all provider keys from 1Password instead")
		if persisted {
			fmt.Fprintln(out, "  pi-stack config unset provider_key_mode  # stop auto-using sbx keys on future runs")
		}
		return false
	}
	fmt.Fprintln(out, "provider keys: using existing sbx keys (anthropic, openai, google). 1Password reconciliation and host credential setup are skipped this run.")
	return true
}

// setupProvisionKeys sources model provider keys (Anthropic/OpenAI/Google),
// preferring 1Password but accepting a complete existing sbx key set instead:
//
// Skip path 1 (explicit, useSbxKeys / --use-sbx-keys): trust sbx outright,
// skipping every op install/signin/ref/reconciliation step below. Wins even if
// provider refs already exist, since an explicit flag is a deliberate choice
// for THIS run, and it never deletes or touches those refs or their synced
// metadata. Requires the exact successful sbx probe (acceptExistingSbxKeys);
// absent/erroring/incomplete sbx fails with a clear message.
//
// Skip path 2 (interactive convenience, no flag): offered ONCE, only when
// interactive, not --yes, the exact sbx probe reports all three keys present,
// AND no provider ref is configured yet (probeProviderKeyRefs), so it
// never nags someone who already chose 1Password. Default Enter is yes. Yes
// re-probes (acceptExistingSbxKeys) and returns immediately on success; No
// falls through to the strict flow below with no further retries.
//
// Strict flow (the fallback, and the only path when neither skip applies):
//
// Step 0 (hard preconditions): `op` must be installed AND have an account
// configured. Without either, there is nothing to source keys from — fail
// setup with the exact fix, before pack/host/onboarding ever run.
//
// Step 1 (collect + validate every ref): for each provider, in order
// (providerKeyRefOrder):
//   - a ref already configured (op-refs.env OR hostmode.env, via currentOpRef)
//     is CONFIRMED, not re-solicited — but it still must resolve via `op read`
//     to a non-empty value; a broken existing ref fails setup outright rather
//     than silently degrading.
//   - a ref with NO configuration yet, on an interactive TTY, is prompted for
//     exactly one at a time. Empty input or EOF is NOT "skip" — a key is
//     mandatory, so that fails setup. An invalid or unresolvable ref explains
//     the problem and reprompts (capped at providerKeyPromptAttempts).
//   - a ref with NO configuration and NO interactive TTY prints the exact
//     `pi-stack secret set` command per missing provider and fails setup —
//     never prompts blind on a pipe/CI runner.
//
// Step 2 (mirror + verify): every validated ref is written to BOTH
// op-refs.env (sandbox) and hostmode.env (host mode) so both worlds see
// identical keys — the canonical both-file write happens per ref inside Step
// 1's loop, so there is no separate reread-and-remirror pass. Setup then
// verifies all three landed in hostmode.env (the final membership check).
//
// Step 3 (reconcile sbx): reconcileProviderKeysWithSbx brings sbx to the same
// state as the now-validated refs — fed the Step-1 SNAPSHOT (envVar -> ref +
// envVar -> resolved value), never a reread of the files (missing -> set;
// present+same -> no-op; present+changed -> ask, default no). A reconcile
// failure (sbx ends up without a key it should have) fails setup.
//
// The whole strict flow — Step 1's initial ref reads/validation through the
// canonical writes, the hostmode verification, and Step 3/4's sbx
// reconciliation + synced-ref metadata — runs holding the provider-refs
// transaction lock (withProviderRefsLock), so a concurrent `pi-stack secret
// set`/`secret rm` in another process cannot interleave between validating a
// ref and acting on it. Inside that section only *Locked write variants are
// used (nested flock on the same file would deadlock).
//
// Step 4 (final probe): success requires ALL THREE keys are actually usable in
// sbx — or sbx genuinely can't be probed (absent/control-plane down), in
// which case we fail OPEN rather than block a box with no sbx at all. A
// probe that finds even one key missing is a real failure, not portability.
//
// Never persists or prints a resolved secret value.
//
// setupProvisionKeysFn is the seam setupHostPhase's legacy call path used to
// go through (a package var so a test can replace it with a stub that records
// whether it was invoked); setupHostPhase itself now calls the typed
// provisionProviderKeysFn instead (see below), but setupProvisionKeysFn stays
// for the many tests exercising setupProvisionKeys directly (it never touches
// the persisted mode, matching its unchanged bool-returning signature).
var setupProvisionKeysFn = setupProvisionKeys

// setupProvisionKeys is the legacy, bool-returning entry point: it resolves
// keys with NO persisted-mode awareness and NO --use-1password override,
// exactly as before this feature. It now delegates to providerKeyFlow (the
// shared decision core also used by provisionProviderKeys), passing
// use1Password=false and persistedMode="" so its behavior is byte-for-byte
// unchanged for every existing caller/test.
func setupProvisionKeys(env shellEnv, in io.Reader, out io.Writer, interactive, assumeYes, useSbxKeys bool) bool {
	ok, _ := providerKeyFlow(env, in, out, interactive, assumeYes, useSbxKeys, false, "")
	return ok
}

// providerKeySetupResult is the typed outcome of resolving provider keys for
// one setup run: whether it succeeded, and — only when it did — which mode
// actually supplied the keys, so the caller can persist that choice. It is a
// plain returned value, never a package global: nothing about "which mode won"
// is stored anywhere except this struct and the config field the caller
// chooses to save it into.
type providerKeySetupResult struct {
	OK   bool
	Mode string // config.ProviderKeyModeSbx | config.ProviderKeyMode1Password | "" (OK==false, or not applicable)
}

// provisionProviderKeysFn is setupHostPhase's seam to provisionProviderKeys,
// mirroring setupProvisionKeysFn's existing pattern: a package-level function
// variable purely for dependency injection in tests (never mutated at
// runtime to carry state), so a test can replace it with a stub that records
// exactly what it was called with.
var provisionProviderKeysFn = provisionProviderKeys

// provisionProviderKeys is setupHostPhase's actual entry point: it resolves
// the effective provider-key source for this run — an explicit override flag
// (useSbxKeys/use1Password, mutually exclusive, validated by parseOnboardArgs)
// wins over a persisted mode from a prior successful run, which in turn wins
// over the on-the-fly interactive convenience prompt used only when neither
// exists — and reports back which mode actually succeeded via the typed
// result, so setupHostPhase can persist it.
func provisionProviderKeys(env shellEnv, in io.Reader, out io.Writer, interactive, assumeYes, useSbxKeys, use1Password bool, persistedMode string) providerKeySetupResult {
	ok, mode := providerKeyFlow(env, in, out, interactive, assumeYes, useSbxKeys, use1Password, persistedMode)
	if !ok {
		return providerKeySetupResult{}
	}
	return providerKeySetupResult{OK: true, Mode: mode}
}

// providerKeyFlow is the single decision core behind BOTH setupProvisionKeys
// (the legacy bool-returning entry many existing tests exercise directly,
// which always passes use1Password=false and persistedMode="") and
// provisionProviderKeys (the persisted-mode-aware, typed-result entry
// setupHostPhase actually calls). Resolution order:
//
//  1. useSbxKeys (explicit --use-sbx-keys, or a persisted mode of "sbx" when
//     neither explicit flag is given): skip straight to acceptExistingSbxKeys,
//     no prompt, exact all-three probe required.
//  2. use1Password (explicit --use-1password, or a persisted mode of
//     "1password" when neither explicit flag is given): skip the interactive
//     convenience prompt entirely and go straight to the strict flow.
//  3. Neither resolved (mode unset, no explicit flag): fall back to the
//     original behavior — if any provider ref is already configured
//     (probeProviderKeyRefs), go straight to strict; otherwise, on an
//     interactive TTY with sbx reporting all three keys present, offer the
//     one-time convenience prompt (default yes); declining, or any other
//     case, falls through to the strict flow.
//
// mode is only meaningful when ok is true: "sbx" when acceptExistingSbxKeys
// supplied the keys, "1password" when the strict flow did.
//
// A fourth case sits between (1) and (3): mode unset, no explicit flag, no
// provider ref configured yet, and sbx reports SOME but not all three keys.
// That is never silently handed to the strict flow (which would demand a
// fresh 1Password ref even for providers sbx already has) — it fails early
// with sbxPartialKeysGuidance, naming exactly what's missing and offering
// the cheaper sbx fix alongside the 1Password alternative. This applies
// whether or not the run is interactive; it's guidance, not a question.
func providerKeyFlow(env shellEnv, in io.Reader, out io.Writer, interactive, assumeYes, useSbxKeys, use1Password bool, persistedMode string) (ok bool, mode string) {
	effectiveSbx := useSbxKeys || (!use1Password && persistedMode == config.ProviderKeyModeSbx)
	effectiveStrict := use1Password || (!useSbxKeys && persistedMode == config.ProviderKeyMode1Password)

	if effectiveSbx {
		// persisted (not useSbxKeys, but a prior run saved provider_key_mode=sbx)
		// is the only case where suggesting `pi-stack config unset
		// provider_key_mode` on failure is actually useful — an explicit flag or
		// a fresh convenience-prompt accept never persisted anything to unset.
		persisted := !useSbxKeys && persistedMode == config.ProviderKeyModeSbx
		if acceptExistingSbxKeys(env, out, persisted) {
			return true, config.ProviderKeyModeSbx
		}
		return false, ""
	}

	sc := bufio.NewScanner(in)

	if !effectiveStrict {
		switch state, path, rerr := probeProviderKeyRefs(env); state {
		case providerKeyRefsProbeError:
			fmt.Fprintf(out, "could not check whether provider-key refs are already configured (reading %s: %v) \u2014 fix that and re-run.\n", path, rerr)
			return false, ""
		case providerKeyRefsProbeNone:
			sbxOut, sstate := probeSbxSecrets(env)
			if sstate != sbxSecretsOK {
				break // can't probe sbx here at all — nothing sbx-related to say
			}
			missing := missingModelProviders(sbxOut)
			switch {
			case len(missing) == 0:
				// sbx already has all three: offer the one-time interactive
				// convenience prompt. Non-interactive/--yes never asks; it falls
				// straight through to the strict flow below, unchanged from
				// before this feature.
				if interactive && !assumeYes {
					fmt.Fprint(out, sbxKeysConveniencePrompt)
					line, gotAnswer := scanYN(sc)
					if !gotAnswer {
						fmt.Fprintln(out, "  no answer read (EOF) \u2014 that is not consent; re-run setup and answer y or n.")
						return false, ""
					}
					yes := true // default
					if line != "" {
						yes = line == "y" || line == "yes"
					}
					if yes {
						if acceptExistingSbxKeys(env, out, false) {
							return true, config.ProviderKeyModeSbx
						}
						return false, ""
					}
				}
			case len(missing) < len(modelProviders):
				// sbx has SOME but not all three: fail early, interactive or
				// not, rather than blindly launching the strict flow.
				sbxPartialKeysGuidance(out, missing)
				return false, ""
			}
			// len(missing) == len(modelProviders): sbx has none either — nothing
			// sbx-related to say; fall through to the strict flow.
		}
	}

	if runStrictProviderKeyFlow(env, sc, out, interactive, assumeYes) {
		return true, config.ProviderKeyMode1Password
	}
	return false, ""
}

// sbxPartialKeysGuidance is printed when sbx already carries SOME but not all
// three provider keys, mode is unset, and no 1Password ref is configured yet:
// rather than silently launching the full strict 1Password flow (which would
// demand a fresh ref even for providers sbx already has), it names exactly
// what's missing and offers the cheaper sbx fix alongside the 1Password
// alternative. Applies whether or not the run is interactive — this is
// guidance, not a question.
func sbxPartialKeysGuidance(out io.Writer, missing []string) {
	fmt.Fprintf(out, "sbx already has some provider keys, but is missing: %s.\n", strings.Join(missing, ", "))
	fmt.Fprintln(out, "Choose one:")
	for _, name := range missing {
		fmt.Fprintf(out, "  sbx secret set -g %s -t <value>          # restore it in sbx, then re-run setup\n", name)
	}
	fmt.Fprintln(out, "  pi-stack setup --use-1password           # source all provider keys from 1Password instead")
}

// runStrictProviderKeyFlow is setupProvisionKeys' original strict-flow body
// (Steps 0-4 documented above), extracted verbatim so providerKeyFlow can
// invoke it from either the legacy or persisted-mode-aware entry point
// without duplicating the logic.
func runStrictProviderKeyFlow(env shellEnv, sc *bufio.Scanner, out io.Writer, interactive, assumeYes bool) bool {
	fmt.Fprintln(out, "")
	reportProviderKeys(env, out)

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
		fmt.Fprintln(out, "then re-run: pi-stack setup --use-1password")
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

// setupHostMode ALWAYS provisions host mode and enables it when provisioning
// actually succeeds — no prompt (owner decision: setup always sets up host mode,
// its keys are the same op:// refs written to hostmode.env above). The
// provision-before-enable invariant stays: runHostSetup is lenient (returns nil
// even when `pi` is missing), so we verify with hostProvisioned() and never flip
// the gate on with nothing behind it.
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
	if id.Name == "" && id.Email == "" {
		return
	}
	who := id.Name
	if id.Email != "" {
		who = strings.TrimSpace(who + " <" + id.Email + ">")
	}
	// State it factually and NAME the source (git config) so it isn't mysterious.
	// The warm by-first-name greeting belongs to the in-session agent, not this log.
	var facts []string
	if id.Name != "" {
		facts = append(facts, "The user's name is "+id.Name+".")
	}
	if id.Email != "" {
		facts = append(facts, "The user's git email is "+id.Email+".")
	}
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

// runHostSetupFn and hostProvisionedFn are setupHostMode's seams to the real
// host-mode provisioning (hostrun.go): runHostSetup does real filesystem work
// (symlinks, settings.json) AND — whenever `pi` happens to be on the CALLING
// machine's PATH — real `pi install npm:...` network calls; hostProvisioned
// probes for `pi` on PATH. A test exercising setupHostMode/setupHostPhase
// through to success MUST replace both with fakes, or it silently mutates
// real host state and can make real network calls regardless of what's on the
// test runner's PATH. Package vars purely for dependency injection (like
// setupProvisionKeysFn/newIdentityMemory), never mutated to carry state.
var (
	runHostSetupFn    = runHostSetup
	hostProvisionedFn = hostProvisioned
)

func setupHostMode(env shellEnv, out io.Writer, keyResult providerKeySetupResult) {
	cfg, err := config.Load()
	if err != nil {
		return
	}
	if rerr := runHostSetupFn(os.Stderr); rerr != nil || !hostProvisionedFn() {
		if cfg.Host.Enabled {
			cfg.Host.Enabled = false
			if serr := cfg.Save(); serr != nil {
				// Report honestly: the gate is STILL on and we couldn't turn it off.
				fmt.Fprintf(out, "host mode: provisioning incomplete AND could not disable the stale gate (%v) — run `pi-stack config set host.enabled false`.\n", serr)
				return
			}
		}
		fmt.Fprintln(out, "host mode: not provisioned (usually a missing `pi`) — left disabled.")
		fmt.Fprintln(out, "Finish later: pi-stack host setup && pi-stack config set host.enabled true")
		return
	}
	if !cfg.Host.Enabled {
		cfg.Host.Enabled = true
		if serr := cfg.Save(); serr != nil {
			fmt.Fprintf(out, "host mode: provisioned but could not enable (%v)\n", serr)
			return
		}
	}
	// Report BOTH axes so "is host mode on?" is never ambiguous: it's enabled +
	// provisioned (the command works), AND which cloud keys it actually has. Host
	// mode reaches cloud models ONLY through op:// refs in hostmode.env (it does not
	// use the sandbox proxy); with none it runs Ollama-only.
	//
	// Wording depends on WHICH mode supplied the keys this run (keyResult.Mode),
	// never merely on hostModeProviderKeys' syntax check: the strict (1password)
	// flow just resolved and reconciled every ref via `op read` THIS run, so
	// "validated" is earned; the sbx skip path (explicit --use-sbx-keys,
	// persisted sbx mode, or the accepted convenience prompt) never touched
	// hostmode.env at all this run — any refs it lists are simply whatever a
	// PRIOR run configured, unverified here, so it must say "configured", not
	// "wired"/"validated". Either way, actual validation still happens for real
	// at every host launch via `op run --env-file` (runHostLaunch) — this is
	// only about what the COPY may honestly claim happened just now.
	// Tri-state: an UNREADABLE hostmode.env (permission error, symlink loop,
	// real I/O failure) is neither "local-only" nor "configured" — either claim
	// would be a confident guess about state we could not actually read. Host
	// mode itself may still be enabled + provisioned (that already happened
	// above); this function just can't truthfully finish with a keys claim, so
	// it says so instead of guessing.
	keys, kerr := hostModeProviderKeys(env)
	if kerr != nil {
		fmt.Fprintf(out, "host mode: enabled + provisioned, but credential state unreadable: %v\n", kerr)
		fmt.Fprintln(out, "  cannot confirm whether cloud keys are configured; fix the above and re-run `pi-stack setup`.")
		return
	}
	if len(keys) > 0 {
		if keyResult.Mode == config.ProviderKeyMode1Password {
			fmt.Fprintf(out, "host mode: enabled + provisioned; cloud keys validated this run (%s). Launch: pi-stack host\n", strings.Join(keys, ", "))
		} else {
			fmt.Fprintf(out, "host mode: enabled + provisioned; cloud refs configured (%s) but not verified this run (used existing sbx keys) — resolved just-in-time at launch via `op run`. Launch: pi-stack host\n", strings.Join(keys, ", "))
		}
	} else {
		// A legitimate, expected result, not a bug: setup succeeds with no
		// hostmode.env refs whenever it skipped 1Password this run (--use-sbx-keys,
		// or accepting the interactive convenience prompt). sbx keys wire the
		// sandbox, but host mode reaches cloud models ONLY through hostmode.env's
		// op:// refs, never the sandbox proxy. Never asks for refs here or anywhere
		// else in this same setup run; the local/Ollama-only result stands until
		// the user configures refs themselves.
		fmt.Fprintln(out, "host mode: enabled + provisioned; local/Ollama-only for now, no 1Password key refs in hostmode.env.")
		fmt.Fprintln(out, "  add cloud keys later with: pi-stack secret set ANTHROPIC_API_KEY op://Vault/Item/field (repeat per provider)")
	}
}

// reportProviderKeys prints the anthropic/openai/google/github key status. It
// runs only on the strict (1Password) path, never when a run skipped
// 1Password via --use-sbx-keys or the interactive convenience prompt, where it
// would be noisy and confusing right before (or after) a one-line skip status.
// Best-effort: if sbx is not on PATH we say so instead of guessing. It never
// suggests a raw `sbx secret set ... -t "sk-..."` command; setup's own
// 1Password flow (setupProvisionKeys, called right after this) is what
// actually collects a missing ref, so pointing at a raw-secret shortcut here
// would contradict the invariant this command enforces.
func reportProviderKeys(env shellEnv, out io.Writer) {
	fmt.Fprintln(out, "provider keys (host secrets, injected by the sandbox proxy):")
	sbxOut, sbxOK := "", false
	if _, err := env.lookPath("sbx"); err == nil {
		if o, rerr := env.run("sbx", "secret", "ls"); rerr == nil {
			sbxOut, sbxOK = o, true
		}
	}
	if !sbxOK {
		fmt.Fprintln(out, "  (sbx not available — cannot check keys here)")
		return
	}
	var missing []string
	for _, key := range []string{"anthropic", "openai", "google", "github"} {
		if secretCheck(key, key, sbxOut, sbxOK).state == stateOK {
			fmt.Fprintf(out, "  %-10s ✓\n", key)
		} else {
			fmt.Fprintf(out, "  %-10s ✗ (not set)\n", key)
			if key != "github" {
				missing = append(missing, key)
			}
		}
	}
	if len(missing) > 0 {
		fmt.Fprintln(out, "  missing model key(s) above: `pi-stack setup` collects the mandatory 1Password ref(s) for you next.")
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
  1. host   — provision model keys (prefers 1Password, wiring BOTH the sandbox
              and host mode; accepts a complete existing sbx key set instead
              via --use-sbx-keys or a one-time prompt, see below), ensure
              memory, create your default pack, and provision + enable host
              mode ('pi-stack host') when the host can run it
  2. agent  - launch a sandbox and hand off to a ONE-SHOT upfront guide that
              names the exact workflows, explains memory and packs, reports
              grounded setup gaps, then asks for your real task
Host mode (pi UNSANDBOXED) is provisioned + enabled when provisioning succeeds
(it needs pi on the host); disable it any time with
'pi-stack config set host.enabled false'.

DIR defaults to the current directory (like ` + "`pi-stack run`" + `). Repeat semantics:
the host phase (keys/memory/pack/host-mode) ALWAYS reconciles again, even when a
sandbox already exists for DIR. If one exists and you did not pass --replace,
setup leaves it alone (never force-removes it, never replays the tour into a
live session) and prints your choices: 'pi-stack run [DIR]' to reattach, or
'pi-stack setup [DIR] --replace' to recreate it with your current settings and
get the tour. Only a POSITIVELY absent sandbox gets the first-launch handoff;
if the sandbox state cannot be determined at all (sbx errored), setup fails
closed after the host phase — fix sbx and re-run.

Setup flags:
  --replace                recreate an existing sandbox for DIR (sbx rm -f +
                           create) so it picks up current pack/MCP/skills and
                           receives the guided tour; harmless when absent

Host-config flags (all optional). Provider keys PREFER 1Password and are
collected/reconciled from it by default; an ordinary value flag (--account/
--knowledge/--mcp/--model) does NOT suppress that. --use-sbx-keys and
--use-1password are mutually exclusive explicit overrides for THIS run; either
one is persisted (provider_key_mode in config.toml) once it succeeds, so a
repeat 'pi-stack setup' with no flag reuses that same choice with no prompt.
  --use-sbx-keys            trust sbx outright; skips all 1Password checks.
                           Requires sbx to already have all three keys
                           (anthropic, openai, google), fails clearly if any
                           is missing, absent, or unverifiable. Works
                           non-interactively. Never deletes existing refs; it
                           just doesn't use them this run. Persists
                           provider_key_mode=sbx on success, so the NEXT
                           'pi-stack setup' auto-skips 1Password the same way
                           with no prompt (re-checking the exact all-three
                           probe every time).
  --use-1password            force the strict 1Password flow for this run even
                           if provider_key_mode is persisted as sbx, or sbx
                           already has all three keys (skips the convenience
                           prompt entirely). Persists provider_key_mode=
                           1password on success.
  (interactive, no flag,   when provider_key_mode is UNSET and no provider ref
   mode unset)             is configured yet, and sbx already has all three
                           keys, setup asks ONCE: "sbx already has anthropic,
                           openai, and google. Use those keys and skip
                           1Password-backed host credentials? [Y/n]" (default
                           yes). Accepting persists provider_key_mode=sbx;
                           declining falls through to the strict flow (which
                           persists provider_key_mode=1password on success),
                           with no further retries this run. Once any ref
                           exists, a mode is persisted, or setup runs
                           non-interactively, this prompt never appears.
  --account <email>        set the Google Workspace (gog) account + enable gog
  --knowledge <path|url>   scaffold/point the global knowledge base
  --mcp <name>             enable an MCP server (repeatable; allowlisted)
  --model <ollama-model>   set the ollama-bridge model
  --yes | --non-interactive  never prompt (CI); does NOT imply --use-sbx-keys.
                           Strict 1Password refs must already resolve unless
                           --use-sbx-keys is also given (or provider_key_mode
                           is already persisted as sbx)
  -h | --help              this help

Inspect or reset the persisted choice any time:
  pi-stack config get provider_key_mode
  pi-stack config unset provider_key_mode

For scripted host config with NO agent handoff, use ` + "`pi-stack onboard`" + ` instead.
`
