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
	"strings"

	"pi-stack/host/config"
)

// onboardingKickoff is the first pi message `setup` sends into the fresh session
// to hand off to the agent. Kept minimal on purpose: it names the skill so it
// auto-loads and frames the user as having opted in (they typed `setup`). The
// rich flow (host-state truth file, task-first sequencing, tracks) is the v2
// design in docs/design/onboarding-v2-spec.md; this is the honest interim.
const onboardingKickoff = "Run the `onboarding` skill to get me set up — I typed `pi-stack setup`, " +
	"so I've opted in; just begin."

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

	// Phase 1: host config (non-interactive: gate keys, report state, ensure memory).
	if err := setupHostPhase(env, hostArgs, os.Stdout); err != nil {
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
// (and is not) ready. On a TTY with no flags it interactively offers to wire the
// optional bits; with flags OR no TTY it applies the flags non-interactively
// (the CI path), exactly like `pi-stack onboard`.
func setupHostPhase(env shellEnv, flags []string, out io.Writer) error {
	fmt.Fprintln(out, "pi-stack setup — configuring the host")
	fmt.Fprintln(out, "")

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Provider keys: report status + the exact fix for any that are missing. Keys
	// are sbx secrets (proxy-injected); we only report them, never enter them.
	reportProviderKeys(env, out)

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
	return nil
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
// bad profile) — the caller then skips the guard rather than blocking setup.
func setupSandboxName(dir string) (string, bool) {
	name := deriveSandboxName(dir)
	_, profile, err := loadResolvedConfig()
	if err != nil {
		return "", false
	}
	if profile != config.DefaultProfile {
		name += "-" + sanitizeProfileName(profile)
	}
	return name, true
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

Guided onboarding in two phases:
  1. host   — report provider keys, ensure memory, and (interactively) wire
              optional knowledge + Google Workspace account
  2. agent  — launch a sandbox and hand off to the agent, which proactively
              starts onboarding and lands you in a working session

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
