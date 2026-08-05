// setup_cmd.go — `pix setup` as a typed root child, plus the one thing that is
// deliberately NOT part of the provision loop: the agent handoff. The handoff
// is an exec into another command whose decision matrix is about a sandbox
// that may already be alive, and a step that cannot be re-probed does not
// belong in a loop whose contract is that the second check is authoritative.
//
// The host phase is handed the argv the flags COMPOSE TO, not the one the user
// typed: kong alone decides what a flag is.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/sandbox"
	"pix/host/sys"
	"pix/host/workflow/launch"
	"pix/host/workflow/onboard"
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
	Replace bool `help:"Recreate an existing sandbox for DIR so it picks up current pack/MCP/skills and gets the tour."`
	Verbose bool `help:"Show underlying sbx, Git, Docker and setup output, not just actions/results."`
	Apply   bool `help:"Apply a pending .pix/onboarding.json in DIR, under a confirmation gate."`

	Pack       []string `help:"Activate a pack through the host trust gate, then run its required setup hooks (repeatable)." placeholder:"PATH|URL"`
	With       []string `help:"Also run a named optional setup hook from --pack (repeatable; invalid without --pack)." placeholder:"ID"`
	Mcp        []string `help:"Enable an MCP server (repeatable; allowlisted)." placeholder:"NAME"`
	Model      string   `help:"Set the ollama-bridge model." placeholder:"MODEL"`
	Models     string   `help:"Restrict agents to these canonical catalog models." placeholder:"ID,ID"`
	PullModels bool     `help:"Pull any CONFIRMED-missing configured local Ollama model. The only download consent setup honors."`
	Yes        bool     `short:"y" aliases:"non-interactive" help:"Never prompt (CI)."`

	GoogleWorkspace bool   `hidden:"" help:"Route setup through the Google Workspace transaction."`
	Credentials     string `hidden:"" help:"OAuth client path for --google-workspace."`

	// The retired spellings stay DECLARED so they keep answering with the
	// sentence that says what replaced them. Deleting them would downgrade a
	// migration notice into "unknown flag".
	UseSbxKeys   bool   `hidden:"" name:"use-sbx-keys"`
	Use1Password bool   `hidden:"" name:"use-1password"`
	Knowledge    string `hidden:""`
}

// hostArgs recomposes the host phase's argv. Order is fixed so one invocation
// always produces the same argv (and receipt), and every value uses
// `--flag=value` so a value that looks like a flag cannot be re-split.
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
	if c.GoogleWorkspace {
		a = append(a, "--google-workspace")
	}
	add("--credentials", c.Credentials)
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

// retired answers a removed flag with the sentence that says what replaced it.
func (c *setupCmd) retired() error {
	switch {
	case c.UseSbxKeys:
		return cli.Usagef("--use-sbx-keys has been removed: 1Password (op) is now the only provider-key source; run `pix setup` with op installed + signed in")
	case c.Use1Password:
		return cli.Usagef("--use-1password has been removed: 1Password is now the only provider-key source, so `pix setup` always uses it")
	case c.Knowledge != "":
		return cli.Usagef("--knowledge was retired with the built-in OKF knowledge service (W2 U03A); use `pix pack use` for a pack's embedded knowledge/ dir")
	}
	return nil
}

func (c *setupCmd) Run(d *cli.Deps) error {
	if err := c.retired(); err != nil {
		return err
	}
	hostArgs := c.hostArgs()

	env := defaultShellEnv()
	env.Quiet = !c.Verbose
	if c.Verbose {
		_ = os.Setenv("PIX_SETUP_VERBOSE", "1")
	}
	parsed, err := onboard.ParseOnboardArgs(hostArgs)
	if err != nil {
		return cli.UsageError{Err: err}
	}
	// Validate every semantic flag/value before pack adoption or any mutation;
	// the host phase repeats the same pure validator.
	preflightCfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	if err := provision.ValidateSetupSemantics(parsed, preflightCfg, env, hostBinaryResolver); err != nil {
		return cli.UsageError{Err: err}
	}

	// `--apply` reconciles a pending <DIR>/.pix/onboarding.json and stops: it is
	// NOT provisioning, so it touches no pack, model or sandbox.
	if c.Apply {
		if err := launch.ValidateRunWorkspace(c.Dir, knownVerb); err != nil {
			return cli.UsageError{Err: err}
		}
		onboard.ReconcileOnboarding(c.Dir, env, d.In, d.Out, parsed.AssumeYes, d.Interactive, onboardDeps())
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
	// The published base kit comes from GitHub, but a fresh sbx install trusts
	// only docker.io: fill that one publisher allowlist entry (and the one-time
	// global network policy) before the first handoff.
	if err := provision.EnsureSetupSbxDefaults(env); err != nil {
		return err
	}
	// An unreleased launcher uses its local checkout kit: validate it with the
	// installed sbx parser before any mutation, so schema skew fails once.
	if err := launch.ValidateSetupKit(version, launch.ResolveRepoRoot, launch.ValidateSbxKit); err != nil {
		return err
	}

	// The host phase: check, apply the verified gaps, check again.
	if err := provision.RunSetup(env, hostArgs, d.In, d.Out, d.Interactive); err != nil {
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

	// Handoff: branch on the POSITIVE state. Existing without --replace is left
	// alone (never force-removed, never replayed into); --replace relaunches
	// through `run --replace` with the kickoff; an unprobeable sbx FAILS CLOSED.
	name, nameOK := provision.SetupSandboxName(c.Dir)
	state := launch.SbxUnknown
	if nameOK && name != "" {
		state = launch.ProbeTaskSandbox(env, name)
	}
	return runSetupHandoff(c.Dir, name, state, c.Replace, d.Out, func(argv []string) error {
		return dispatchRun(d, argv)
	})
}

// dispatchRun re-enters the ROOT for the handoff launch, so setup cannot
// acquire its own copy of run's grammar: it hands `run` an argv exactly as a
// user would type it.
func dispatchRun(d *cli.Deps, argv []string) error {
	if code := dispatch(append([]string{"run"}, argv...), d); code != 0 {
		return cli.SilentError{Code: code}
	}
	return nil
}

// runSetupHandoff is the pure post-host-phase decision + action, separate from
// setupCmd.Run so the state/replace matrix is testable without the provisioning
// loop or an sbx exec. Errors ONLY on the fail-closed unknown state (or a
// failed launch).
func runSetupHandoff(dir, name string, state sandbox.State, replace bool, out io.Writer, runFn func([]string) error) error {
	// kickoffArgs builds the run argv for a launch that gets the tour:
	// [DIR] [--replace] -- <OnboardingKickoff>. DIR is forwarded only when
	// explicit, so `pix setup` in a repo behaves exactly like `pix run` there.
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
	// retryArg carries the ORIGINAL --replace into the retry command below:
	// dropping it would downgrade a requested recreate into a reattach.
	retryArg := dirArg
	if replace {
		retryArg += " --replace"
	}

	switch state {
	case launch.SbxUnknown:
		// FAIL CLOSED: launching would re-attach a live session and replay the
		// kickoff into it. The host phase completed, so a retry is cheap.
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
			return runFn(kickoffArgs())
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
	return runFn(kickoffArgs())
}

// init supplies the composition provisioning declares but cannot perform.
func init() {
	provision.DefaultEnv = defaultShellEnv
	provision.HostBinary = hostBinaryResolver
	provision.Register = pack.RegisterFn(registerServers)
}
