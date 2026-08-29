// setup.go — `pix setup`, expressed as the provision loop and nothing else:
// setup declares the capabilities it can both CHECK and REPAIR and hands them
// to Run. Why that is the whole design, and which incident each rule is, is
// provision.go's package comment; this file is the step table.
//
// SCOPE. Setup installs four things — the launchd agent, requested packs,
// consented local models, and (on a terminal) ONE provider key, by running the
// `pix models add` interview rather than reimplementing it. MCP registration and
// identity seeding stay out: they belong to `pix mcp add` and the sandbox
// itself, each having been a way for setup to claim something it had not proven.
// sbx stays probe-only for the same reason, and names the exact brew line.
//
// The key interview was out of scope once, and that was wrong in the one way
// this package exists to prevent. `providers` is a REQUIRED capability, so a
// host with no pack could not reach a passing `pix setup` at all: the command
// ended by printing a repair it knew exactly how to perform. It read as correct
// for as long as it did because a pack with managed inference satisfies that row
// KEYLESSLY, so on the hosts being tested the step never ran. Delegating is what
// keeps the old rule intact — `pix models add` stores the ref, rebuilds the
// bindings, probes them live and syncs into sbx, and setup's second check is
// still the only thing that reports readiness.
package provision

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"pix/host/cli"
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
	// AddProvider is `pix models add <provider>`, injected for the same reason
	// PackApply is: provision may not import the sibling workflow that performs
	// the step. Unwired, the providers step FAILS rather than quietly going back
	// to printing a command it could have run.
	AddProvider func(env hostenv.Env, in io.Reader, out io.Writer, interactive bool, provider string) error
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
// config, or a SECOND CHECK that still finds a required gap.
//
// interactive is a terminal the loop may ask ON, and nothing more: it gates the
// provider-key interview and is ANDed with --yes inside, so every scripted path
// (no tty, or --yes) still gets the printed command and never a prompt.
func RunSetup(env hostenv.Env, flags []string, in io.Reader, out io.Writer, interactive bool) error {
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

	// Narrate to a terminal only. Setup's budget is 8s PER probe and it checks
	// twice, so an unnarrated run can sit silent for sixteen seconds — long
	// enough that the honest reading is "it hung". A redirected stream gets the
	// report alone, unchanged, so nothing that parses this output has to learn a
	// new preamble.
	progress := io.Writer(nil)
	if interactive {
		progress = out
	}
	o := Run(context.Background(), Options{Budget: setupBudget, Progress: progress},
		setupSteps(cfg, env, opts, in, out, interactive)...)
	o.Render(out)
	if len(o.Failed) > 0 {
		// A failed apply is a hard abort even for an optional probe (pack): a
		// pack the user asked for and setup failed to adopt is never success.
		//
		// The cause is carried, so nothing is lost and errors.Is still works.
		// The duplication that made this unreadable is fixed in Render, which now
		// prints only the first line of a multi-line failure — the full block
		// appears exactly once, here, at the end, which is where the exit code
		// points and the last thing a user sees.
		return fmt.Errorf("setup could not apply %s: %w", o.Failed[0].Name, o.Failed[0].Err)
	}
	if o.ExitCode() != health.ExitOK {
		return fmt.Errorf("setup could not prove every required capability — the rows above are the second check, not what setup tried")
	}
	return nil
}

// setupSteps is the whole of what `pix setup` provisions, as data. Four steps
// carry an Apply (launchd, packs, models, providers); sbx is probe-only and the
// report names its exact command. Being data rather than control flow is what
// makes the scope auditable: setup cannot register an MCP server, because there
// is no step here that could.
func setupSteps(cfg *config.Config, env hostenv.Env, opts Opts, in io.Reader, out io.Writer, interactive bool) []Step {
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
		// Tri-state on purpose: a key store that did not ANSWER is unknown, never
		// "no key", and the loop applies only VERIFIED gaps — so an sbx that could
		// not be read is never answered with a prompt for a key that may already be
		// there. ANY-OF, because one key is enough to launch — the same probe `pix
		// doctor` reports from, never a second implementation of the same
		// classification. Keyless is part of that classification: a host whose
		// backends carry their own credential is not missing a key, and setup must
		// not open a row whose only repair is a key nothing reads. That is also why
		// a managed-inference pack never sees the interview below — this probe is
		// already OK there, so the step is skipped as already proven.
		//
		// secret.ModelProviders, which is what doctor and the launch gate already
		// pass, and NOT the ANTHROPIC_API_KEY/GEMINI_API_KEY spelling setup used to
		// build for itself. `sbx secret ls` lists a secret under the name it was
		// STORED as, and the writer (secret.setSbxSecret) stores provider keys by
		// PROVIDER name — so searching that listing for env-var names matched
		// nothing on any host, ever. Setup's own comment already claimed this was
		// "the same probe pix doctor reports from, never a second implementation";
		// the second implementation was this argument, and it made the row
		// permanently red on a host with all three keys wired, synced and
		// answering live requests.
		// ResolveKeyless for the same reason PackProbe resolves its root — the
		// pack step's Apply is what writes those backends to disk, so only a
		// fresh read sees the host the second check is actually grading.
		Name: "providers",
		Probe: health.ProviderKeyProbe{Bin: "sbx", Args: []string{"secret", "ls"},
			Want: secret.ModelProviders, AnyOf: true, ResolveKeyless: currentKeylessBackends},
		Apply: providerKeyApply(env, opts, in, out, interactive),
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

// providerKeyApply is the step that turns "none of ANTHROPIC_API_KEY,
// OPENAI_API_KEY, GEMINI_API_KEY is set" from a dead end into a question. It
// delegates the whole transaction to the injected `pix models add`, which is
// still the one place a credential is solicited; setup only decides WHICH
// provider to hand it.
//
// nil — no Apply, the report names the command and nothing is asked — whenever
// there is no terminal to ask on or the user said --yes. A broad --yes
// suppresses questions, it does not answer them, and a key is the one thing
// setup must never pick on someone's behalf.
func providerKeyApply(env hostenv.Env, opts Opts, in io.Reader, out io.Writer, interactive bool) func(context.Context) error {
	if !interactive || opts.AssumeYes {
		return nil
	}
	return func(context.Context) error {
		// Re-read keylessness HERE, not from the first check. Every probe in a
		// round runs before any apply, so `setup --pack <managed-inference pack>`
		// on a fresh host graded providers while the pack was still unadopted:
		// a real gap at 09:00:00 and a non-question by the time the pack step
		// finished, two steps later in the same run. Asking for a key on the
		// strength of that stale grade is asking for a credential nothing would
		// read — the exact fault the Keyless carve-out exists to prevent.
		if backends := currentKeylessBackends(); backends != "" {
			return ErrSkipped{"inference here is keyless (" + backends + "), so no provider key is needed"}
		}
		if Injected.AddProvider == nil {
			return fmt.Errorf("the provider-key interview is not wired — the composition root must set provision.Injected")
		}
		name, ok := askProviderName(in, out)
		if !ok {
			return ErrSkipped{"you chose to wire a key later: pix models add <provider>"}
		}
		return Injected.AddProvider(env, in, out, interactive, name)
	}
}

// askProviderName offers the providers a model can be routed to and reads one.
// The list is secret.ProviderKeyRefOrder — the same order `pix models add`
// offers, never a copy — so setup cannot get out of step with the command it
// hands the answer to. ok=false is a deliberate decline, which is a Skip and not
// a failure; an unrecognized answer is passed through, because the receiving
// command owns the "unknown provider" message and already lists the valid ones.
func askProviderName(in io.Reader, out io.Writer) (string, bool) {
	def := secret.ProviderKeyRefOrder[0].Name
	fmt.Fprintln(out, "\nNo model provider is wired, and Pix cannot call a model without one.")
	fmt.Fprintln(out, "Wiring one stores a 1Password ref, probes every model it can reach, and")
	fmt.Fprintln(out, "syncs the key into sbx. Nothing is written until you answer the next prompt.")
	for _, p := range secret.ProviderKeyRefOrder {
		fmt.Fprintf(out, "  %-10s %s\n", p.Name, p.EnvVar)
	}
	ans := cli.AskLine(in, out, fmt.Sprintf("Which provider? [%s, or 'skip']: ", def), def)
	if ans == "skip" || ans == "none" || ans == "n" || ans == "no" {
		return "", false
	}
	return ans, true
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

// Description is the prose above setup's GENERATED usage: what the loop
// guarantees and how a repeat behaves. The flag list is not here — the command
// struct's tags are the flag list.
const Description = `Provisions this host, then starts Pix. Setup CHECKS every capability it owns,
applies only the gaps it verified, and CHECKS AGAIN — the second check is the
only thing that reports readiness, so nothing is called ready because a step
said so.

It installs exactly four things: the launchd agent, the packs you asked for,
(only with --pull-models) local model weights, and one provider key — asked for
on a terminal, never under --yes. A gap it cannot repair is reported with the
exact command that does.

DIR defaults to the current directory (like ` + "`pix run`" + `). The host phase
ALWAYS reconciles again; an existing sandbox for DIR is left alone (never
removed, never replayed into) and setup prints your two choices. Only a
POSITIVELY absent sandbox gets the first-launch handoff — an undeterminable
sandbox state fails closed after the host phase.

When no provider key is present, setup runs the ` + "`pix models add`" + ` interview
rather than only naming it — that command is still the one place a 1Password ref
is solicited, and setup just asks which provider. It is skipped entirely without
a terminal, under --yes, and on a host whose inference is keyless (a pack that
brings its own credential). Setup reports the key store tri-state — a store that
did not answer is 'unknown', never 'no key', and unknown is never prompted on.
`
