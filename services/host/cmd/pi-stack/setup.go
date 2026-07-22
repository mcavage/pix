// setup.go implements `pi-stack setup` — the explicit, guided onboarding entry.
//
// Owner decision (supersedes the in-`run` auto-offer): onboarding is a TWO-PHASE
// thing the user opts into by NAME.
//
//  1. HOST phase (here, on the host): source model keys from 1Password
//     (setupProvisionKeys), ensure the memory service, create the personal pack,
//     seed git identity, and ALWAYS provision + enable host mode when it can.
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

// onboardingKickoff is the first message `setup` hands the agent. It is
// DELIBERATELY short and human — it reads like something the user would type,
// not a machine directive wall. The rewritten `onboarding` skill owns the actual
// flow (guided teach, read host-state, land a task); the word "guided" is all it
// needs to pick GUIDED mode. (Making this fully invisible — agent greets with no
// visible prompt at all — needs a session-start extension + an image rebuild;
// tracked as a follow-up.)
const onboardingKickoff = "I just ran pi-stack setup — give me the guided walkthrough and help me get started."

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
	dir := "."
	dirSet := false
	var hostArgs []string
	for i := 0; i < len(argv); i++ {
		a := argv[i]
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

	// GUARD: setup's agent handoff needs a FRESH pi session to receive the
	// onboarding kickoff message. If a sandbox for this dir already exists, `run`
	// re-attaches to its (possibly live) session and the kickoff silently no-ops
	// — the "dumped into a silent agent" trap. Refuse early with the exact fix
	// rather than half-onboarding. Best-effort: an unresolvable name/profile or an
	// sbx that can't be probed (sbxUnknown) does NOT block — we only refuse on a
	// POSITIVELY existing sandbox.
	if name, ok := setupSandboxName(dir); ok {
		if setupSandboxExists(probeTaskSandbox(env, name)) {
			fmt.Fprintf(os.Stderr, "pi-stack setup: a sandbox %q already exists for this directory.\n", name)
			fmt.Fprintln(os.Stderr, "setup onboards a FRESH session; re-attaching to an existing one would skip it.")
			fmt.Fprintln(os.Stderr, "Choose one:")
			fmt.Fprintf(os.Stderr, "  pi-stack rm %s && pi-stack setup   # recreate and onboard cleanly\n", name)
			fmt.Fprintln(os.Stderr, "  pi-stack run                       # just start working in the existing sandbox")
			fmt.Fprintln(os.Stderr, "  (or say \"onboard me\" to the agent inside the running sandbox)")
			os.Exit(1)
		}
	}

	// Phase 1: host config — source keys from 1Password, ensure memory, create the
	// pack, seed identity, provision+enable host mode (see setupHostPhase).
	if err := setupHostPhase(env, hostArgs, os.Stdin, os.Stdout, isTTY(os.Stdin)); err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack setup: %v\n", err)
		os.Exit(1)
	}

	// Phase 2: hand off to the in-VM onboarding agent via an initial message.
	fmt.Fprintln(os.Stdout, "")
	fmt.Fprintln(os.Stdout, "Sandbox ready. Launching pi — it'll introduce itself, show you how it works,")
	fmt.Fprintln(os.Stdout, "and get you into a real task. (You can quit any time; just run `pi-stack run`.)")

	runArgs := []string{}
	if dir != "." {
		runArgs = append(runArgs, dir)
	}
	runArgs = append(runArgs, "--", onboardingKickoff)
	runRun(runArgs)
}

// setupHostPhase does the deterministic host configuration and reports what is
// (and is not) ready. The only interactive step is pasting op:// refs for
// providers missing one (TTY + op installed); with flags OR no TTY it is fully
// non-interactive (the CI path).
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
	// Interactive prompts fire only on a real TTY with NO flags and no --yes: any
	// flag / --non-interactive is the scripted (CI) path and must never prompt.
	interactive := tty && !opts.assumeYes && len(flags) == 0

	// Provider keys: prefer 1Password as the source (steer there), regardless of
	// what sbx already has. Solicit an op:// ref
	// for any provider without one yet, mirror refs into hostmode.env, and
	// force-sync them into sbx (overwriting) — regardless of what sbx already has,
	// no yes/no gate. A model key is REQUIRED to run, so abort if none ends up
	// wired rather than hand off to a session that can't talk to a model.
	reportProviderKeys(env, out)
	if !setupProvisionKeys(env, in, out, interactive, opts.assumeYes) {
		fmt.Fprint(out, modelKeyMissingMessage(env))
		return fmt.Errorf("no model provider key configured — set one and re-run `pi-stack setup`")
	}

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
	changes, err := applyOnboardingResult(r, cfg, env, out, func(c *config.Config) error { return c.Save() })
	if err != nil {
		return err
	}

	// Ensure a personal pack exists (git-init'd) so authored skills + captured
	// knowledge have a durable, versioned home the onboarding agent can point at.
	// Best-effort; adopts an existing repo at the path, else inits one.
	personalRoot := personalPackRoot() // runs the legacy "pack" -> "personal" migration
	if _, err := os.Stat(filepath.Join(personalRoot, packManifestName)); err != nil {
		runPackNew(env, out, []string{personalRoot})
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
	// ~/.gitconfig) and seed it so onboarding greets you by name instead of
	// starting anonymous. host-state carries it deterministically; memory gets it
	// durably.
	seedIdentity(env, out)

	// Host mode: setup ALWAYS sets it up (owner decision) — provision + enable, no
	// prompt. Its cloud keys are the same hostmode.env op:// refs written above.
	// provision-before-enable stays, so a `pi`-less box just stays disabled.
	setupHostMode(env, out)
	return nil
}

// setupProvisionKeys wires model keys through 1Password with NO blind re-sync:
// a 1Password ref is REQUIRED per provider (host mode needs it), but once a
// ref and sbx are both in a known-good state, re-running setup touches
// neither `op` nor `sbx secret set` for that provider again. Two steps per
// provider (providerKeyRefOrder):
//
//	STEP 1 (op-refs.env, this file): a ref is REQUIRED.
//	 - a ref already exists -> CONFIRM it (print it, Enter keeps it, or paste a
//	   new op:// ref to replace it) — never a blind re-paste.
//	 - no ref -> GET it (interactive prompt only); non-interactive never
//	   prompts and never blocks — it just notes host mode will be Ollama-only
//	   for that provider until one is added.
//	 A new/changed ref is written to BOTH op-refs.env and hostmode.env.
//
//	STEP 2 (secret_sync.go's reconcileProviderKeysWithSbx): reconcile that ref
//	 against sbx using the launcher-owned synced-ref record (syncedrefs.go) —
//	 sbx secret VALUES are write-only (`sbx secret ls` lists names only), so
//	 "did the ref change since we last synced it" is tracked by us, not read
//	 back from sbx. Missing key -> set + record, no ask. Present key with an
//	 unchanged recorded ref -> NO op read, NO sbx set. Present key with a new/
//	 changed ref -> ask before overwriting (default No); non-interactive only
//	 overwrites with --yes.
//
// Returns whether a usable model key is in place afterward (true when sbx
// can't be probed — never block a box without sbx).
func setupProvisionKeys(env shellEnv, in io.Reader, out io.Writer, interactive, assumeYes bool) bool {
	fmt.Fprintln(out, "")
	sc := bufio.NewScanner(in)
	if !opInstalled(env) {
		fmt.Fprintln(out, "Model keys come from 1Password, but the `op` CLI isn't installed.")
		fmt.Fprintln(out, "Install it (https://developer.1password.com/docs/cli/) and re-run setup;")
		fmt.Fprintln(out, "for now I'll use whatever keys are already in sbx.")
	} else {
		if interactive {
			fmt.Fprintln(out, "Model keys come from 1Password (a ref is required per provider).")
			fmt.Fprintln(out, "Paste an op:// ref (op://Vault/Item/field), or press Enter to keep/skip:")
		}
		for _, p := range providerKeyRefOrder {
			provisionProviderRef(env, sc, out, p, interactive)
		}
	}
	// Covers refs that pre-date this feature or were set via `pi-stack secret
	// set` (op-refs.env only), so host mode has the same keys the sandbox does.
	mirrorProviderRefsToHostMode(env)
	reconcileProviderKeysWithSbx(env, sc, out, interactive, assumeYes)
	// Tri-state: only abort setup when we can POSITIVELY confirm no key. If sbx is
	// absent OR its control plane can't be probed, don't block — we can't tell.
	present, probeOK := sbxModelKeyState(env)
	if !probeOK {
		return true
	}
	return present
}

// provisionProviderRef is setupProvisionKeys' STEP 1 for one provider.
func provisionProviderRef(env shellEnv, sc *bufio.Scanner, out io.Writer, p struct{ envVar, name string }, interactive bool) {
	current, hasRef := currentOpRef(env, p.envVar)
	switch {
	case hasRef && interactive:
		fmt.Fprintf(out, "  %-9s %s\n", p.name, current)
		fmt.Fprint(out, "    Enter to keep, or paste a new op:// ref to replace: ")
		if !sc.Scan() {
			return
		}
		ref := normalizeOpRef(sc.Text())
		if ref == "" {
			return // kept
		}
		if !strings.HasPrefix(ref, "op://") {
			fmt.Fprintln(out, "    skipped: not an op:// ref (kept the existing one)")
			return
		}
		writeProviderRef(env, out, p, ref)
	case hasRef:
		// non-interactive: confirming a blind re-paste is meaningless — keep it.
	case interactive:
		fmt.Fprintf(out, "  %s: ", p.name)
		if !sc.Scan() {
			return
		}
		ref := normalizeOpRef(sc.Text())
		if ref == "" {
			fmt.Fprintf(out, "    (no ref for %s — host mode will be Ollama-only for it)\n", p.name)
			return
		}
		if !strings.HasPrefix(ref, "op://") {
			fmt.Fprintln(out, "    skipped: not an op:// ref")
			return
		}
		writeProviderRef(env, out, p, ref)
	default:
		// non-interactive, no ref yet: never block, never prompt.
		fmt.Fprintf(out, "  %-9s (no 1Password ref — host mode Ollama-only for it; add one: pi-stack secret set %s op://vault/item/field)\n", p.name, p.envVar)
	}
}

// writeProviderRef upserts a new/changed ref into BOTH op-refs.env and
// hostmode.env (one paste wires sandbox + host mode).
func writeProviderRef(env shellEnv, out io.Writer, p struct{ envVar, name string }, ref string) {
	if err := writeOpRefQuiet(env, p.envVar, ref); err != nil {
		fmt.Fprintf(out, "    could not save: %v\n", err)
		return
	}
	_ = writeOpRefFileQuiet(env, hostModeRefsPath(env), p.envVar, ref)
}

// currentOpRef returns the current FILLED op:// ref for a provider env var. It
// checks op-refs.env (sandbox) AND hostmode.env (host mode): a ref given via
// EITHER path counts, so setup never re-prompts for a ref the user already
// provided in one file but not the other. If it's found only in hostmode.env, it
// backfills op-refs.env so the sbx reconcile step has it too.
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
					_ = writeOpRefQuiet(env, envVar, r.value) // backfill op-refs.env
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
// seedIdentity greets the user by name (from git config) and stores durable
// identity facts in memory (best-effort, only if the daemon is up), so their
// first session isn't anonymous. host-state.json also carries identity for the
// onboarding skill to read directly.
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
	seeded := false
	if c := memoryClient(); c.Up() {
		if id.Name != "" {
			_, _ = c.Call("remember", map[string]any{"content": "The user's name is " + id.Name + ".", "source": "setup", "profile": "default"})
			seeded = true
		}
		if id.Email != "" {
			_, _ = c.Call("remember", map[string]any{"content": "The user's git email is " + id.Email + ".", "source": "setup", "profile": "default"})
			seeded = true
		}
	}
	if seeded {
		fmt.Fprintf(out, "\nIdentity (from your git config): %s — saved so your sessions know you.\n", who)
	} else {
		fmt.Fprintf(out, "\nIdentity (from your git config): %s\n", who)
	}
}

func setupHostMode(env shellEnv, out io.Writer) {
	cfg, err := config.Load()
	if err != nil {
		return
	}
	if rerr := runHostSetup(os.Stderr); rerr != nil || !hostProvisioned() {
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
	if keys := hostModeProviderKeys(env); len(keys) > 0 {
		fmt.Fprintf(out, "host mode: enabled + provisioned; cloud keys wired (%s). Launch: pi-stack host\n", strings.Join(keys, ", "))
	} else {
		fmt.Fprintln(out, "host mode: enabled + provisioned, but Ollama-only — no 1Password key refs in hostmode.env yet.")
		fmt.Fprintln(out, "  add them: pi-stack secret set ANTHROPIC_API_KEY op://Vault/Item/field (then re-run setup).")
	}
}

// reportProviderKeys prints the anthropic/openai/google/github key status and,
// for any that are missing, the exact `sbx secret set` command. Best-effort: if
// sbx is not on PATH we say so instead of guessing.
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
		fmt.Fprintf(out, "  set a model key:  sbx secret set -g %s -t \"sk-...\"\n", missing[0])
	}
}

// setupSandboxExists reports whether a probed state means a sandbox is POSITIVELY
// present (running or stopped) — the case setup must refuse. sbxAbsent (nothing
// there) and sbxUnknown (sbx couldn't be probed) both mean "don't block".
func setupSandboxExists(state sbxState) bool {
	return state == sbxRunning || state == sbxStopped
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
  1. host   — provision model keys from 1Password (wiring BOTH the sandbox and
              host mode), ensure memory, create your personal pack, and provision
              + enable host mode ('pi-stack host') when the host can run it
  2. agent  — launch a sandbox and hand off to a GUIDED walkthrough that teaches
              the flow by doing your real first task (crew, skills, memory) and
              introduces each capability as your work needs it
The sandbox is always set up. Host mode (pi UNSANDBOXED) is provisioned + enabled
when provisioning succeeds (it needs pi on the host); disable it any time with
'pi-stack config set host.enabled false'.

DIR defaults to the current directory (like ` + "`pi-stack run`" + `). Setup REFUSES
if a sandbox already exists for DIR (its agent handoff needs a fresh session);
it tells you to 'pi-stack rm <name> && pi-stack setup', or just 'pi-stack run'.

Host-config flags (all optional; passing ANY skips the interactive prompts):
  --account <email>        set the Google Workspace (gog) account + enable gog
  --knowledge <path|url>   scaffold/point the global knowledge base
  --mcp <name>             enable an MCP server (repeatable; allowlisted)
  --model <ollama-model>   set the ollama-bridge model
  -h | --help              this help

For scripted host config with NO agent handoff, use ` + "`pi-stack onboard`" + ` instead.
`
