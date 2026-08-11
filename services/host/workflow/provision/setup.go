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
	"pix/host/inference"
	"pix/host/launcher"
	"pix/host/packinfo"
	"pix/host/sandbox"
	"pix/host/secret"
	"pix/host/service"
)

// OnboardingKickoff is the first message `setup` hands the agent: DELIBERATELY
// short and human, because the `onboarding` skill owns the actual flow. The
// handoff itself belongs to the command layer, which prefixes the
// generated-input marker and execs `run` — provision only authors the words.
const OnboardingKickoff = "I just ran pix setup. Give me the upfront guide and help me get started."

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
	// Injected is the rest of that composition, as ONE value the command layer
	// fills in one statement. Register is MCP registration; PackApply is pack
	// adoption, which is a TRUST decision (Tier-1 bill of materials, fingerprint,
	// rollback) that belongs to workflow/pack — provision declares the step and
	// may not import the sibling workflow that can perform it. Unwired, PackApply
	// FAILS rather than silently turning setup's pack row green.
	Injected = Composition{}
)

// Composition is the shape of what provision declares but cannot perform. The
// func types are written out rather than named because naming them here would
// hand every caller a second name for the command layer's own registrar.
type Composition struct {
	Register func(cfg *config.Config, env hostenv.Env, out io.Writer, names []string,
		servers map[string]config.MCPServer) error
	PackApply func(env hostenv.Env, out io.Writer, packs, with []string, assumeYes bool) error
}

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
	if err := ValidateSetupSemantics(opts); err != nil {
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
		Name: "pack",
		// Resolve, not a static Root: the pack step's own Apply (packApply, below)
		// mutates the config on disk through a SEPARATE config.Load() (pack
		// adoption is a trust decision this package may not perform, so it hands
		// off to the injected adopter, which loads and saves its own *Config).
		// That write never reaches the cfg pointer captured here, so a probe that
		// resolved the root once at THIS call would still see the pre-adoption
		// root on the second check even though the pack was adopted. Reloading
		// fresh on every Check is what makes the second check see what the apply
		// actually wrote.
		Probe: health.PackProbe{Resolve: func() string { return currentPackRoot() }},
		Apply: packApply(env, opts, out),
		// PackProbe proves the pack is active; the apply also runs the pack's
		// required setup hooks, which the probe never looks at. See
		// Step.ProbeProvesSubset.
		ProbeProvesSubset: true,
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
		// of the same classification. Keyless is part of that classification: a
		// host whose backends carry their own credential is not missing a key, and
		// setup must not open a row whose only repair is a key nothing reads.
		// ResolveKeyless for the same reason PackProbe resolves its root — the
		// pack step's Apply is what writes those backends to disk, so only a
		// fresh read sees the host the second check is actually grading.
		Name: "providers",
		Probe: health.ProviderKeyProbe{Bin: "sbx", Args: []string{"secret", "ls"},
			Want: providerKeyEnvVars(), AnyOf: true, ResolveKeyless: currentKeylessBackends},
	}}
}

// currentPackRoot re-derives the active pack root from a FRESH config.Load(),
// never the cfg loaded once at the top of RunSetup: PackApply's own adoption
// saves a different *Config to disk, so only a reload sees it. An unreadable
// config resolves to "" (no active pack) rather than panicking a probe.
func currentPackRoot() string {
	c, err := config.Load()
	if err != nil {
		return ""
	}
	return packinfo.Resolve(c, "").Path
}

// currentKeylessBackends is currentPackRoot's twin for the providers row, and
// reloads for the same reason: an adopted pack's inference backends land on
// disk, not in the cfg captured when the steps were built. An unreadable config
// resolves to "" — no claim of keylessness — so the key store still answers.
func currentKeylessBackends() string {
	c, err := config.Load()
	if err != nil {
		return ""
	}
	return inference.KeylessBackends(c)
}

// packApply is the `--pack` step, and it is deliberately thin: with no --pack it
// is nil, so setup can never manufacture a pack to turn a row green; with one it
// defers to the injected adoption authority, which runs the same BoM review,
// fingerprint and rollback as `pix pack use`.
func packApply(env hostenv.Env, opts Opts, out io.Writer) func(context.Context) error {
	if len(opts.Packs) == 0 {
		return nil
	}
	return func(context.Context) error {
		if Injected.PackApply == nil {
			return fmt.Errorf("pack adoption is not wired — the composition root must set provision.Injected")
		}
		return Injected.PackApply(env, out, opts.Packs, opts.WithSetup, opts.AssumeYes)
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
func ValidateSetupSemantics(opts Opts) error {
	if len(opts.WithSetup) > 0 && len(opts.Packs) == 0 {
		return ErrUsage{fmt.Errorf("--with requires --pack")}
	}
	// Shape only: no pack has been adopted yet, so no server name is knowable.
	if err := validateOnboardingShape(setupProposal(opts)); err != nil {
		return ErrUsage{err}
	}
	return nil
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
