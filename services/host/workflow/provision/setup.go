// setup.go — `pix setup`, expressed as the provision loop and nothing else:
// setup declares the capabilities it can both CHECK and REPAIR and hands them
// to Run. Why that is the whole design, and which incident each rule is, is
// provision.go's package comment; this file is the step table.
//
// SCOPE. Setup installs three things — the launchd agent, requested packs, and
// consented local models. Everything else it can only observe: sbx and provider
// keys are probe-only steps that name the exact command. The key interview, MCP
// registration and identity seeding belong to `pix models add`, `pix mcp
// register` and the sandbox itself — each was a way for setup to claim something
// it had not proven.
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
	"pix/host/sandbox"
	"pix/host/secret"
	"pix/host/service"
	"pix/host/workflow/launch"
	"pix/host/workflow/pack"
)

// OnboardingKickoff is the first message `setup` hands the agent: DELIBERATELY
// short and human, because the `onboarding` skill owns the actual flow. It
// carries launch.GeneratedInputMarker so memory-capture.ts can tell this was
// machine-generated, not typed by the user.
const OnboardingKickoff = launch.GeneratedInputMarker + "I just ran pix setup. Give me the upfront guide and help me get started."

// ErrUsage marks an argument error, which exits 2 rather than 1.
type ErrUsage struct{ error }

// DefaultEnv, HostBinary and Register are the composition this package declares
// but cannot perform: building a real env, resolving the paired pix-host, and
// registering MCP servers with credentials resolved over secret. Both the setup
// loop and the onboarding reconcile use these — one wiring, one place the
// composition root fills it. The env default PANICS rather than returning a
// half-wired one: a setup that silently probes nothing is the failure mode this
// whole design exists to delete.
var (
	DefaultEnv = func() hostenv.Env {
		panic("provision: DefaultEnv not wired — the composition root must set it")
	}
	HostBinary = launcher.FindHostBinary
	Register   pack.RegisterFn
)

// installLaunchd is the launchd apply. It is a variable so a test can point it
// at a recorder instead of the real LaunchAgent.
var installLaunchd = service.Install

// setupBudget bounds ONE probe in EACH of the two checks. Setup is allowed to
// be slower than `pix status`: it is the command a user runs once and watches.
const setupBudget = 8 * time.Second

// RunSetup is the host phase of `pix setup`: apply the declared host config
// from the flags, then run the provision loop over the capabilities setup owns.
// It returns an error only for a usage mistake, a failure to write the declared
// config, or a SECOND CHECK that still finds a required gap. It asks nothing, so
// it takes no reader and no tty flag: the one question setup can ask belongs to
// the --apply reconcile, which the command layer runs before the host phase.
func RunSetup(env hostenv.Env, flags []string, out io.Writer) error {
	opts, perr := ParseSetupArgs(flags)
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
	if err := ValidateSetupSemantics(opts, env, HostBinary); err != nil {
		return err
	}
	// The DECLARED half: --mcp/--model are host configuration the user stated,
	// not a gap anything probes. Writing it is not a provisioning step and must
	// happen before the first check, or the check would grade the old config.
	// The sparse encode also drops retired keys on this save.
	applyOnboarding(setupProposal(opts), cfg)
	if err := cfg.Save(); err != nil {
		return fmt.Errorf("saving the declared host config: %w", err)
	}

	o := Run(context.Background(), Options{Budget: setupBudget}, setupSteps(cfg, env, opts, out)...)
	o.Render(out)
	if len(o.Failed) > 0 {
		// A failed apply is a hard abort even for an optional probe (pack): a
		// pack the user asked for and setup failed to adopt is never success.
		return fmt.Errorf("setup could not apply %s: %w", o.Failed[0].Name, o.Failed[0].Err)
	}
	if o.ExitCode() != health.ExitOK {
		return fmt.Errorf("setup could not prove every required capability — the rows above are the second check, not what setup tried")
	}
	return nil
}

// setupSteps is the whole of what `pix setup` provisions, as data. Three steps
// carry an Apply (launchd, packs, models); the rest are probe-only and the
// report names their exact command. Being data rather than control flow is what
// makes the scope auditable: setup cannot write a provider key or register an
// MCP server, because there is no step here that could.
func setupSteps(cfg *config.Config, env hostenv.Env, opts Opts, out io.Writer) []Step {
	return []Step{{
		// sbx is required and setup does NOT install it: a package manager
		// invocation is not something to run behind a user's back, and the
		// probe already knows the exact brew line.
		Name:  "sbx",
		Probe: health.SbxProbe{},
	}, {
		Name:  "launchd",
		Probe: health.LaunchdProbe{Label: service.LaunchdLabel, UID: os.Getuid()},
		Apply: func(context.Context) error { return installLaunchd(out) },
	}, {
		Name:  "pack",
		Probe: health.PackProbe{Root: launch.ResolveHostStatePack(cfg, "").Path},
		Apply: packApply(env, opts, out),
	}, {
		Name:  "models",
		Probe: ollamaModelsProbe{Env: env, Tags: localModelTags(cfg)},
		Apply: modelsApply(env, cfg, opts),
	}, {
		// Tri-state on purpose, and probe-only on purpose: a key store that did
		// not ANSWER is unknown, never "no key", and the one place a credential is
		// solicited is `pix models add`, so setup can neither prompt for one nor
		// claim to have written one. ANY-OF, because one key is enough to launch —
		// the same probe `pix doctor` reports from, never a second implementation
		// of the same classification.
		Name: "providers",
		Probe: health.ProviderKeyProbe{Bin: "sbx", Args: []string{"secret", "ls"},
			Want: providerKeyEnvVars(), AnyOf: true},
	}}
}

// packApply adopts the packs THIS invocation asked for, through the ordinary pack
// trust transaction (same BoM review, fingerprint and rollback as `pix pack
// use`). With no --pack it is nil: setup must never manufacture a pack to turn a
// row green.
func packApply(env hostenv.Env, opts Opts, out io.Writer) func(context.Context) error {
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
			if err := pack.RunPackUse(env, out, useArgs, Register); err != nil {
				return fmt.Errorf("adopting pack %s: %w", requested, err)
			}
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

// modelsApply pulls the confirmed-missing local model tags, ONLY under explicit
// `--pull-models` consent. Without the flag there is no Apply at all, so a
// multi-gigabyte download can never be a side effect of setup; the report still
// names `ollama pull <tag>`. A broad `--yes` is not consent: it suppresses
// questions, it does not answer them.
func modelsApply(env hostenv.Env, cfg *config.Config, opts Opts) func(context.Context) error {
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
func setupProposal(opts Opts) *OnboardingResult {
	return &OnboardingResult{
		Version:           1,
		MCP:               append([]string(nil), opts.Mcp...),
		OllamaBridgeModel: strings.TrimSpace(opts.Model),
	}
}

// ValidateSetupSemantics checks only built-in argument meaning. It performs no
// writes and opens no authorization flow, so the command layer can call it
// before the first pack is adopted.
func ValidateSetupSemantics(opts Opts, env hostenv.Env, hostResolver func() (string, error)) error {
	if len(opts.WithSetup) > 0 && len(opts.Packs) == 0 {
		return ErrUsage{fmt.Errorf("--with requires --pack")}
	}
	if err := validateOnboarding(setupProposal(opts), env, hostResolver); err != nil {
		return ErrUsage{err}
	}
	return nil
}

// NormalizeSetupPackArg expands the `owner/repo` shorthand to a clone URL.
func NormalizeSetupPackArg(arg string) string {
	arg = strings.TrimSpace(arg)
	if strings.Count(arg, "/") == 1 && !strings.Contains(arg, ":") && !strings.HasPrefix(arg, ".") && !strings.HasPrefix(arg, "~") {
		return "https://github.com/" + arg + ".git"
	}
	return arg
}

// SetupSandboxName is the sandbox name `pix run` would use for dir — ONE shared
// derivation, so setup's handoff guard probes the SAME box run attaches to.
// ok=false when unresolvable; the caller fails closed.
func SetupSandboxName(dir string) (string, bool) {
	if _, err := config.Load(); err != nil {
		return "", false
	}
	return sandbox.Name(dir), true
}

// providerKeyEnvVars is the set of provider credentials any ONE of which makes
// this host able to call a model.
func providerKeyEnvVars() []string {
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
and (only with --pull-models) local model weights. A gap it cannot repair is
reported with the exact command that does.

DIR defaults to the current directory (like ` + "`pix run`" + `). The host phase
ALWAYS reconciles again; an existing sandbox for DIR is left alone (never
removed, never replayed into) and setup prints your two choices. Only a
POSITIVELY absent sandbox gets the first-launch handoff — an undeterminable
sandbox state fails closed after the host phase.

Provider keys are NOT collected here: run ` + "`pix models add <provider>`" + `, the
one place a 1Password ref is solicited. Setup reports the key store tri-state —
a store that did not answer is 'unknown', never 'no key'.
`
