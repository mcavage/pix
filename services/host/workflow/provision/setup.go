// setup.go — `pix setup`, expressed as the provision loop and nothing else.
//
// What used to be here: an eight-phase transcript, a pre-mutation inventory, a
// fixed-order mutation table, a two-question prompt budget, a per-axis readiness
// builder set, and a renderer that had to be tested for not reading the
// inventory. Seven notions of "done", six of which could disagree.
//
// What is here now: setup declares the capabilities it can both CHECK and
// REPAIR, and hands them to Run. The loop checks, applies only VERIFIED gaps,
// and checks again — and the second check is the only thing that may call
// anything ready. Every property the phase machine spent code enforcing falls
// out of that:
//
//   - idempotence: a step already ready is never applied, because the first
//     check said so. No journal, no receipt.
//   - no success prose from a mutation: applies return an error or nil; every
//     word the user reads comes from the second check.
//   - unknown is not a gap: a probe that could not see is skipped, not
//     "repaired" on a guess.
//
// SCOPE. Setup installs three things — the launchd agent, requested packs, and
// consented local models. Everything else it can only observe: sbx and provider
// keys are probe-only steps that name the exact command. The interactive
// key interview, the MCP registration step, identity seeding and the GitHub
// credential mirror are gone; `pix models add`, `pix mcp register` and the
// sandbox's own host-state injection own those, and each of them was a way for
// setup to claim something it had not proven.
package provision

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"pix/host/config"
	"pix/host/health"
	"pix/host/hostenv"
	"pix/host/launcher"
	"pix/host/secret"
	"pix/host/service"
	"pix/host/workflow/launch"
	"pix/host/workflow/onboard"
	"pix/host/workflow/pack"
	"pix/host/workspace"
)

// OnboardingKickoff is the first message `setup` hands the agent. It is
// DELIBERATELY short and human — it reads like something the user would type,
// not a machine directive wall. The rewritten `onboarding` skill owns the
// actual flow. It carries launch.GeneratedInputMarker so memory-capture.ts can
// tell this was machine-generated, not typed by the user.
const OnboardingKickoff = launch.GeneratedInputMarker + "I just ran pix setup. Give me the upfront guide and help me get started."

// ErrUsage marks an argument error, which exits 2 rather than 1.
type ErrUsage struct{ error }

// DefaultEnv, HostBinary and Register are the composition setup declares but
// cannot perform: building a real env, resolving the paired pix-host, and
// registering a pack's MCP servers with credentials resolved over secret.
//
// The env default PANICS rather than returning a half-wired one: a setup that
// silently probes nothing is the failure mode this whole refactor exists to
// delete.
var (
	DefaultEnv = func() hostenv.Env {
		panic("provision: DefaultEnv not wired — the composition root must set it")
	}
	HostBinary = launcher.FindHostBinary
	Register   pack.RegisterFn
)

// InstallLaunchd is the launchd apply. It is a variable so a test can point it
// at a recorder instead of the real LaunchAgent, and so the non-macOS build
// still has one definition of "there is nothing to install here".
var InstallLaunchd = service.Install

// SetupBudget bounds ONE probe in EACH of the two checks. Setup is allowed to
// be slower than `pix status`: it is the command a user runs once and watches.
const SetupBudget = 8 * time.Second

// RunSetup is the host phase of `pix setup`: apply the declared host config
// from the flags, then run the provision loop over the capabilities setup owns.
// It returns an error only for a usage mistake, a failure to write the declared
// config, or a SECOND CHECK that still finds a required gap.
func RunSetup(env hostenv.Env, flags []string, in io.Reader, out io.Writer, tty bool) error {
	opts, perr := onboard.ParseOnboardArgs(flags)
	if perr != nil {
		return ErrUsage{perr}
	}
	if opts.Apply {
		// --apply is intercepted by the command layer (it reconciles a pending
		// onboarding.json and stops). Reaching the host phase with it set means
		// a caller bypassed that route, which would silently ignore the flag.
		return ErrUsage{fmt.Errorf("--apply is handled before the host phase; run `pix setup [DIR] --apply`")}
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	if err := ValidateSetupSemantics(opts, cfg, env, HostBinary); err != nil {
		return err
	}
	// The DECLARED half: --mcp/--model are host configuration the user stated,
	// not a gap anything probes. Writing it is not a provisioning step and must
	// happen before the first check, or the check would grade the old config.
	// The sparse encode also drops retired keys on this save.
	if _, err := onboard.ApplyOnboardingResult(setupProposal(opts), cfg, env, io.Discard, func(c *config.Config) error { return c.Save() }); err != nil {
		return err
	}

	o := Run(context.Background(), Options{Budget: SetupBudget}, SetupSteps(cfg, env, opts, out)...)
	o.Render(out)
	if o.ExitCode() != health.ExitOK {
		return fmt.Errorf("setup could not prove every required capability — the rows above are the second check, not what setup tried")
	}
	return nil
}

// SetupSteps is the whole of what `pix setup` provisions, as data. Three steps
// carry an Apply (launchd, packs, models); the rest are probe-only, and the
// report names their exact command. Keeping this a function of (cfg, env,
// opts) rather than control flow is what makes the scope auditable: a reader
// can see that setup cannot write a provider key or register an MCP server,
// because there is no step here that could.
func SetupSteps(cfg *config.Config, env hostenv.Env, opts onboard.Opts, out io.Writer) []Step {
	steps := []Step{{
		// sbx is required and setup does NOT install it: a package manager
		// invocation is not something to run behind a user's back, and the
		// probe already knows the exact brew line.
		Name:  "sbx",
		Probe: health.SbxProbe{},
	}, {
		Name:  "launchd",
		Probe: health.LaunchdProbe{Label: service.LaunchdLabel, UID: os.Getuid()},
		Apply: func(context.Context) error { return InstallLaunchd(out) },
	}, {
		Name:  "pack",
		Probe: health.PackProbe{Root: launch.ResolveHostStatePack(cfg, "").Path},
		Apply: packApply(env, opts, out),
	}, {
		Name:  "models",
		Probe: ollamaModelsProbe{Env: env, Tags: localModelTags(cfg)},
		Apply: modelsApply(env, cfg, opts),
	}, {
		// Tri-state on purpose, and probe-only on purpose. A key store that did
		// not ANSWER is unknown, never "no key"; and the one place a credential
		// is solicited is `pix models add`, so setup can neither prompt for one
		// nor claim to have written one.
		Name:  "providers",
		Probe: providerKeysProbe{Env: env},
	}}
	return steps
}

// packApply adopts the packs THIS invocation asked for, through the ordinary
// pack trust transaction (same BoM review, fingerprint and rollback as `pix
// pack use`). With no --pack it is nil: a host with no pack is a fine host, and
// setup must never manufacture one to turn a row green.
func packApply(env hostenv.Env, opts onboard.Opts, out io.Writer) func(context.Context) error {
	if len(opts.Packs) == 0 {
		return nil
	}
	return func(context.Context) error {
		var activated []string
		for _, requested := range opts.Packs {
			useArgs := []string{NormalizeSetupPackArg(requested)}
			if opts.AssumeYes {
				useArgs = append([]string{"--yes"}, useArgs...)
			}
			pack.RunPackUse(env, out, useArgs, Register)
			if cfg, err := config.Load(); err == nil && strings.TrimSpace(cfg.Pack) != "" {
				activated = append(activated, cfg.Pack)
			}
		}
		activated = pack.UniquePackRoots(activated)
		if len(activated) > 0 {
			if err := pack.PersistPackStack(activated); err != nil {
				return fmt.Errorf("composing packs: %w", err)
			}
		}
		// A pack's own required setup hooks own its interactive authorization
		// flows, and they run as part of adopting it — a pack that is adopted
		// but not set up is exactly the half-state the second check would then
		// report as a gap with no way to close it.
		requests, err := pack.PlanPackSetupRequests(activated, opts.WithSetup)
		if err != nil {
			return err
		}
		for _, root := range activated {
			if err := pack.RunPackSetup(env, out, root, requests[root], false); err != nil {
				return err
			}
		}
		return nil
	}
}

// modelsApply pulls the confirmed-missing local model tags — and ONLY under
// explicit `--pull-models` consent. Without the flag there is no Apply at all,
// so a multi-gigabyte download can never be a side effect of running setup; the
// report still names `ollama pull <tag>`. A broad `--yes` is not consent: it
// suppresses questions, it does not answer them.
func modelsApply(env hostenv.Env, cfg *config.Config, opts onboard.Opts) func(context.Context) error {
	if !opts.PullModels {
		return nil
	}
	return func(ctx context.Context) error {
		missing, err := missingLocalModels(ctx, env, localModelTags(cfg))
		if err != nil {
			return err
		}
		for _, tag := range missing {
			if err := env.RunInteractive("ollama", "pull", tag); err != nil {
				return fmt.Errorf("ollama pull %s: %w", tag, err)
			}
		}
		return nil
	}
}

// localModelTags is the deduplicated set of local model tags this host is
// configured to use.
func localModelTags(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	var tags []string
	seen := map[string]bool{}
	for _, t := range []string{cfg.MemoryWatcherModel, cfg.MemoryEmbedModel, cfg.OllamaBridgeModel} {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		tags = append(tags, t)
	}
	return tags
}

// setupProposal is the single flag -> proposal translation used by both the
// pre-adoption semantic validator and the config write. Keeping one constructor
// prevents the early safety boundary from accepting a value the host phase
// would interpret differently.
func setupProposal(opts onboard.Opts) *onboard.OnboardingResult {
	return &onboard.OnboardingResult{
		Version:           1,
		MCP:               append([]string(nil), opts.Mcp...),
		OllamaBridgeModel: strings.TrimSpace(opts.Model),
	}
}

// ValidateSetupSemantics checks only built-in argument meaning. It performs no
// writes and opens no authorization flow, so the command layer can call it
// before the first pack is adopted.
func ValidateSetupSemantics(opts onboard.Opts, cfg *config.Config, env hostenv.Env, hostResolver func() (string, error)) error {
	if len(opts.WithSetup) > 0 && len(opts.Packs) == 0 {
		return ErrUsage{fmt.Errorf("--with requires --pack")}
	}
	if err := onboard.ValidateOnboardingResult(setupProposal(opts), cfg, env, hostResolver); err != nil {
		return ErrUsage{err}
	}
	return nil
}

// SetupInteractivePrompts decides whether an interactive question may be asked
// at all: a real TTY, unless the caller explicitly opted out with
// --yes/-y/--non-interactive. Ordinary VALUE flags (--mcp/--model/--models)
// configure host settings and must never silently suppress a prompt.
func SetupInteractivePrompts(tty, assumeYes bool) bool { return tty && !assumeYes }

// FlagTakesValue reports whether an onboard flag consumes a following token
// (only the space-separated form; `--flag=value` is self-contained).
func FlagTakesValue(a string) bool {
	switch a {
	case "--account", "--credentials", "--knowledge", "--mcp", "--model", "--models", "--pack", "--with":
		return true
	}
	return false
}

// NormalizeSetupPackArg expands the `owner/repo` shorthand to a clone URL.
func NormalizeSetupPackArg(arg string) string {
	arg = strings.TrimSpace(arg)
	if strings.Count(arg, "/") == 1 && !strings.Contains(arg, ":") && !strings.HasPrefix(arg, ".") && !strings.HasPrefix(arg, "~") {
		return "https://github.com/" + arg + ".git"
	}
	return arg
}

// SetupSandboxName derives the sandbox name `pix run` would use for dir (base
// name + active-profile suffix), so setup's handoff guard probes the SAME
// sandbox run would attach to. ok=false when the name cannot be resolved; the
// caller then fails closed rather than launching blind.
func SetupSandboxName(dir string) (string, bool) {
	if _, err := config.Load(); err != nil {
		return "", false
	}
	return workspace.DeriveSandboxName(dir), true
}

// ProviderKeyEnvVars is the set of provider credentials any ONE of which makes
// this host able to call a model.
func ProviderKeyEnvVars() []string {
	out := make([]string, 0, len(secret.ProviderKeyRefOrder))
	for _, p := range secret.ProviderKeyRefOrder {
		out = append(out, p.EnvVar)
	}
	return out
}

// Description is the prose above setup's GENERATED usage: what the loop
// guarantees and how a repeat behaves. The flag list is not here — the command
// struct's tags are the flag list.
const Description = `Provisions this host, then starts Pix. Setup CHECKS every capability it owns,
applies only the gaps it verified, and CHECKS AGAIN — the second check is the
only thing that reports readiness, so nothing is called ready because a step
said so.

It installs exactly three things: the launchd agent, the packs you asked for,
and (only with --pull-models) local model weights. Anything else it can only
observe; a gap it cannot repair is reported with the exact command that does.

DIR defaults to the current directory (like ` + "`pix run`" + `). Repeat semantics:
the host phase ALWAYS reconciles again, even when a sandbox already exists for
DIR. If one exists and you did not pass --replace, setup leaves it alone (never
force-removes it, never replays the tour into a live session) and prints your
choices: 'pix run [DIR]' to reattach, or 'pix setup [DIR] --replace' to recreate
it with your current settings and get the tour. Only a POSITIVELY absent sandbox
gets the first-launch handoff; if the sandbox state cannot be determined at all
(sbx errored), setup fails closed after the host phase — fix sbx and re-run.

Provider keys are NOT collected here: run ` + "`pix models add <provider>`" + `, which
is the one place a 1Password ref is solicited. Setup reports the key store
tri-state — a store that did not answer is 'unknown', never 'no key'.
`
