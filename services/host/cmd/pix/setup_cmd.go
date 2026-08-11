// setup_cmd.go — `pix setup` as a typed root child, plus the one thing deliberately NOT
// part of the provision loop: the agent handoff. It execs another command whose decision
// matrix is about a sandbox that may already be alive, and a step that cannot be re-probed
// does not belong in a loop whose contract is that the second check is authoritative. The
// host phase is handed the argv the flags COMPOSE TO, not the one the user typed: kong
// alone decides what a flag is. This file also binds the pack authority provision declares.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"pix/host/cli"
	"pix/host/mcp"
	"pix/host/sandbox"
	"pix/host/sys"
	"pix/host/workflow/launch"
	"pix/host/workflow/pack"
	"pix/host/workflow/provision"
)

func (c *setupCmd) Help() string { return provision.Description }

// setupCmd is the guided host provisioner. Its own flags (--no-agent,
// --replace, --verbose) never reach the host phase; the rest are recomposed
// into provision's argv.
type setupCmd struct {
	Dir string `arg:"" optional:"" default:"." help:"Workspace to provision and launch in (default: .)."`

	NoAgent bool `help:"Run the HOST phase only: no sandbox, no handoff. The scripted/CI path."`
	Verbose bool `help:"Show underlying sbx, Git, Docker and setup output, not just actions/results."`
	Apply   bool `help:"Apply a pending .pix/onboarding.json in DIR, under a confirmation gate."`

	Pack       []string `help:"Activate a pack through the host trust gate, then run its required setup hooks (repeatable)." placeholder:"PATH|URL"`
	With       []string `help:"Also run a named optional setup hook from --pack (repeatable; invalid without --pack)." placeholder:"ID"`
	Mcp        []string `help:"Enable an MCP server (repeatable; allowlisted)." placeholder:"NAME"`
	Model      string   `help:"Set the ollama-bridge model." placeholder:"MODEL"`
	Models     string   `help:"Restrict agents to these canonical catalog models." placeholder:"ID,ID"`
	PullModels bool     `help:"Pull any CONFIRMED-missing configured local Ollama model. The only download consent setup honors."`
	Yes        bool     `short:"y" aliases:"non-interactive" help:"Never prompt (CI)."`
}

// hostArgs recomposes the host phase's argv. Order is fixed so one invocation
// always produces the same argv (and receipt), and every value uses `--flag=value`
// so a value that looks like a flag cannot be re-split.
func (c *setupCmd) hostArgs() []string {
	var a []string
	add := func(flag, v string) {
		if v != "" {
			a = append(a, flag+"="+v)
		}
	}
	if c.Apply {
		a = append(a, "--apply")
	}
	if c.Yes {
		a = append(a, "--yes")
	}
	if c.PullModels {
		a = append(a, "--pull-models")
	}
	add("--model", c.Model)
	add("--models", c.Models)
	for _, v := range c.Mcp {
		add("--mcp", v)
	}
	for _, v := range c.Pack {
		add("--pack", v)
	}
	for _, v := range c.With {
		add("--with", v)
	}
	return a
}

func (c *setupCmd) Run(d *cli.Deps) error {
	hostArgs := c.hostArgs()

	env := defaultShellEnv()
	env.Quiet = !c.Verbose
	if c.Verbose {
		_ = os.Setenv("PIX_SETUP_VERBOSE", "1")
	}
	parsed, err := provision.ParseSetupArgs(hostArgs)
	if err != nil {
		return cli.UsageError{Err: err}
	}
	// Validate every semantic flag/value before pack adoption or any mutation; the
	// host phase repeats the same pure validator.
	if err := provision.ValidateSetupSemantics(parsed); err != nil {
		return cli.UsageError{Err: err}
	}

	// `--apply` reconciles a pending <DIR>/.pix/onboarding.json and stops: NOT
	// provisioning, so it touches no pack, model or sandbox.
	if c.Apply {
		if err := launch.ValidateRunWorkspace(c.Dir, knownVerb); err != nil {
			return cli.UsageError{Err: err}
		}
		provision.ReconcileOnboarding(c.Dir, env, d.In, d.Out, parsed.AssumeYes, d.Interactive)
		return nil
	}
	// DIR must exist AND be a directory BEFORE the host phase mutates real host
	// state, so a typo'd DIR fails with nothing touched.
	if err := launch.ValidateRunWorkspace(c.Dir, knownVerb); err != nil {
		return cli.UsageError{Err: err}
	}
	if err := provision.EnsureSetupSbxSession(env, d.Out, d.Interactive && !parsed.AssumeYes); err != nil {
		return err
	}
	// The published base kit comes from GitHub, but a fresh sbx install trusts only
	// docker.io: fill that one publisher allowlist entry (and the one-time global
	// network policy) before the first handoff.
	if err := provision.EnsureSetupSbxDefaults(env); err != nil {
		return err
	}
	// An unreleased launcher uses its local checkout kit: validate it with the
	// installed sbx parser before any mutation, so schema skew fails once.
	if err := launch.ValidateSetupKit(version, launch.ResolveRepoRoot, launch.ValidateSbxKit); err != nil {
		return err
	}

	// The host phase: check, apply the verified gaps, check again.
	if err := provision.RunSetup(env, hostArgs, d.Out); err != nil {
		var usage provision.ErrUsage
		if errors.As(err, &usage) {
			// an argument mistake, caught before any probe or mutation
			return cli.UsageError{Err: err}
		}
		return err
	}
	// --no-agent stops here: the host phase is the whole command.
	if c.NoAgent {
		return nil
	}

	// Handoff: branch on the POSITIVE state. An existing sandbox is left alone
	// (never force-removed, never replayed into); an unprobeable sbx FAILS CLOSED.
	name, nameOK := provision.SetupSandboxName(c.Dir)
	state := launch.SbxUnknown
	if nameOK && name != "" {
		state = launch.ProbeTaskSandbox(env, name)
	}
	return runSetupHandoff(c.Dir, name, state, d.Out, func(argv []string) error {
		return dispatchRun(d, argv)
	})
}

// dispatchRun re-enters the ROOT for the handoff launch, so setup cannot acquire
// its own copy of run's grammar: it hands `run` an argv as a user would type it.
func dispatchRun(d *cli.Deps, argv []string) error {
	if code := dispatch(append([]string{"run"}, argv...), d); code != 0 {
		return cli.SilentError{Code: code}
	}
	return nil
}

// runSetupHandoff is the pure post-host-phase decision + action, separate from
// setupCmd.Run so the state matrix is testable without the provisioning loop or an
// sbx exec. Errors ONLY on the fail-closed unknown state (or a failed launch).
// setup has no shape that removes a sandbox: an existing one is ALWAYS left alone
// and the user is handed the two commands (attach, or remove-then-run).
func runSetupHandoff(dir, name string, state sandbox.State, out io.Writer, runFn func([]string) error) error {
	// kickoffArgs builds the run argv for a launch that gets the tour: [DIR] --
	// <marker><OnboardingKickoff>; the marker tells memory-capture.ts this was composed,
	// not typed. DIR is forwarded only when explicit, so setup matches `pix run` there.
	kickoffArgs := func() []string {
		args := []string{}
		if dir != "." {
			args = append(args, dir)
		}
		return append(args, "--", launch.GeneratedInputMarker+provision.OnboardingKickoff)
	}
	dirArg := ""
	if dir != "." {
		dirArg = " " + sys.ShellQuote(dir)
	}

	switch state {
	case launch.SbxUnknown:
		// FAIL CLOSED: launching would re-attach a live session and replay the kickoff
		// into it. The host phase completed, so a retry is cheap.
		which := fmt.Sprintf("sandbox %q", name)
		if name == "" {
			which = fmt.Sprintf("the sandbox for %s", dir)
		}
		return fmt.Errorf("cannot determine the state of %s (`sbx ls` failed or sbx is unavailable). Host setup completed; install or fix sbx and retry with: pix setup%s", which, dirArg)
	case launch.SbxRunning, launch.SbxStopped:
		fmt.Fprintln(out, "")
		fmt.Fprintf(out, "Host configuration reconciled. Existing sandbox %q was left alone.\n", name)
		fmt.Fprintln(out, "Attaching keeps the sandbox exactly as it was created (its pack, MCP")
		fmt.Fprintln(out, "servers, and skills were attached at create time). To pick up current")
		fmt.Fprintln(out, "settings instead, remove it first — removal is proof-gated, so it refuses")
		fmt.Fprintln(out, "while another shell is still attached. Choose one:")
		fmt.Fprintf(out, "  pix run%s                 # attach as-is\n", dirArg)
		fmt.Fprintf(out, "  pix rm %s && pix setup%s  # recreate with current settings + get the tour\n", sys.ShellQuote(name), dirArg)
		return nil
	}

	// launch.SbxAbsent (positively confirmed): normal first launch.
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Launching Pix — the agent will take it from here.")
	return runFn(kickoffArgs())
}

// setupProbeWrap makes a pack's setup probe run the way the MCP gateway will
// actually spawn the server: through `op run --env-file`, with the same argv
// grammar registration uses. Without it, setup verified a command in a shell
// that already had what it needed and reported ready for an integration the
// gateway could not start.
//
// Returns argv unchanged when 1Password is not configured, which is exactly what
// OpRunWrap does and a legitimate no-credential host.
func setupProbeWrap(argv []string) []string {
	env := defaultShellEnv()
	creds := mcpCredentials(env)
	return mcp.OpRunWrap(creds.OpPath, creds.OpRefsPath, argv)
}

// init supplies the composition provisioning declares but cannot perform.
func init() {
	provision.DefaultEnv = defaultShellEnv
	provision.HostBinary = hostBinaryResolver
	// setupProbeWrap is the third caller of the one op-run grammar, alongside
	// registration and doctor. It is supplied HERE because the composition root
	// is the only place allowed to know both `pack` and `mcp`.
	provision.Injected = provision.Composition{
		Register:  registerServers,
		PackApply: pack.SetupAdopter(registerServers, setupProbeWrap),
	}
}
