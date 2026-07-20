// setup.go implements `pi-stack setup` — the explicit, guided onboarding entry.
//
// Owner decision (supersedes the in-`run` auto-offer): onboarding is a TWO-PHASE
// thing the user opts into by NAME.
//
//  1. HOST phase (here, on the host): report provider keys (with the exact fix
//     command for any that are missing), ensure the memory service, and — on a
//     TTY — offer to wire the optional host bits (knowledge base, Google
//     Workspace account). Non-interactive / flag-driven runs stay CI-safe.
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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"pi-stack/host/config"
)

// onboardingKickoff is the first pi message `setup` sends into the fresh session
// to hand off to the agent. Kept minimal on purpose: it names the skill so it
// auto-loads and frames the user as having opted in (they typed `setup`). The
// rich flow (host-state truth file, task-first sequencing, tracks) is the v2
// design in docs/design/onboarding-v2-spec.md; this is the honest interim.
const onboardingKickoff = "Run the `onboarding` skill in GUIDED mode — I typed `pi-stack setup`, so I " +
	"want the full walkthrough, not a quick start. Teach me the pi-stack flow by " +
	"doing (memory, skills, the crew, knowledge, packs), land me in a real first " +
	"task, and co-author at least one real artifact into my pack. Both the sandbox " +
	"and host mode are set up on the host; don't re-ask host config. Begin now."

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
	var hostArgs []string
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if len(a) > 0 && a[0] != '-' {
			dir = a
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

	// Phase 1: host config (report keys, ensure memory; a single opt-in 1Password
	// offer on a TTY — see setupHostPhase).
	if err := setupHostPhase(env, hostArgs, os.Stdin, os.Stdout, isTTY(os.Stdin)); err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack setup: %v\n", err)
		os.Exit(1)
	}

	// Phase 2: hand off to the in-VM onboarding agent via an initial message.
	fmt.Fprintln(os.Stdout, "")
	fmt.Fprintln(os.Stdout, "Host ready. Launching a sandbox — the agent will pick up onboarding from here,")
	fmt.Fprintln(os.Stdout, "ask a couple of questions, and drop you into a working session.")

	runArgs := []string{}
	if dir != "." {
		runArgs = append(runArgs, dir)
	}
	runArgs = append(runArgs, "--", onboardingKickoff)
	runRun(runArgs)
}

// setupHostPhase does the deterministic host configuration and reports what is
// (and is not) ready. It stays non-interactive EXCEPT for one tightly-gated,
// default-No offer to wire model keys to 1Password (TTY + op installed + no key
// refs yet). With flags OR no TTY it is fully non-interactive (the CI path).
func setupHostPhase(env shellEnv, flags []string, in io.Reader, out io.Writer, tty bool) error {
	fmt.Fprintln(out, "pi-stack setup — configuring the host")
	fmt.Fprintln(out, "")

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Provider keys: the SHARED bootstrap (identical to what `run` auto-runs) —
	// resolve existing 1Password refs into sbx, and on a TTY steer you to 1Password,
	// writing refs to BOTH op-refs.env (sandbox) and hostmode.env (host mode) so one
	// paste wires both. Report status either way.
	reportProviderKeys(env, out)
	bootstrapProviderKeys(env, in, out, tty)

	// Build the config proposal. Flags win; otherwise, on a TTY, ask.
	opts, perr := parseOnboardArgs(flags)
	if perr != nil {
		return perr
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
	if _, err := os.Stat(filepath.Join(config.PackDir(), packManifestName)); err != nil {
		runPackNew(env, out, []string{config.PackDir()})
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

	// Host mode: setup's job is to leave you able to run BOTH the sandbox AND host
	// mode (the unsandboxed escape hatch). On a TTY, offer to enable + provision it
	// — default-Yes, since you explicitly asked to be set up, but LOUD about what it
	// means. Its cloud keys come from hostmode.env, already written by the key
	// bootstrap above. Skipped on non-TTY (CI never auto-enables the unsandboxed
	// path).
	offerHostMode(in, out, tty)
	return nil
}

// offerHostMode enables + provisions `pi-stack host` (unsandboxed) as part of
// guided setup, so you finish able to run both worlds. Default-Yes on a TTY,
// with a clear warning; never on non-TTY. Best-effort provisioning (needs a
// pi-stack checkout to symlink the harness from; a consumer install without one
// gets a clear TODO).
func offerHostMode(in io.Reader, out io.Writer, tty bool) {
	if !tty || in == nil {
		return
	}
	cfg, err := config.Load()
	if err != nil {
		return
	}
	if cfg.Host.Enabled {
		fmt.Fprintln(out, "host mode: already enabled.")
		return
	}
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Host mode runs pi DIRECTLY on this machine — NO sandbox, NO network fence,")
	fmt.Fprintln(out, "real credentials. It's for work the sandbox can't do (system installs, real")
	fmt.Fprintln(out, "devices). Cloud keys come from your 1Password refs (hostmode.env).")
	if !confirmYN(in, out, "Enable host mode too? [Y/n]: ", true) {
		fmt.Fprintln(out, "host mode: left disabled (enable later: pi-stack config set host.enabled true).")
		return
	}
	cfg.Host.Enabled = true
	if err := cfg.Save(); err != nil {
		fmt.Fprintf(out, "host mode: could not enable (%v)\n", err)
		return
	}
	if err := runHostSetup(osStderrOr(out)); err != nil {
		fmt.Fprintf(out, "host mode enabled, but provisioning is incomplete: %v\n", err)
		fmt.Fprintln(out, "finish it later with: pi-stack host setup")
		return
	}
	fmt.Fprintln(out, "host mode: enabled + provisioned. Launch it with: pi-stack host")
}

// osStderrOr returns os.Stderr (runHostSetup writes to an *os.File); the out
// writer in setup is os.Stdout in practice, so host provisioning logs go to
// stderr like the rest of host setup.
func osStderrOr(io.Writer) *os.File { return os.Stderr }

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
  1. host   — provision keys (steer to 1Password, wiring BOTH the sandbox and
              host mode), ensure memory, create your personal pack, and offer to
              enable + provision host mode
  2. agent  — launch a sandbox and hand off to a GUIDED walkthrough: teach the
              flow by doing, co-author a real artifact into your pack, land a task
You finish able to run BOTH the sandbox and host mode ('pi-stack host').

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
