// setup.go implements `pix setup` — the explicit, guided onboarding entry.
//
// Owner decision (supersedes the in-`run` auto-offer): onboarding is a TWO-PHASE
// thing the user opts into by NAME.
//
//  1. HOST phase (here, on the host): configure callable inference through
//     direct 1Password-backed APIs, a gateway, Ollama, or pack-provided
//     bindings; enable memory only when its local models are verified; and
//     seed first-name identity.
//     Host mode is NOT set up here — it's opt-in via `pix host setup`.
//     Host-config (gog/knowledge/mcp) comes from FLAGS, not interactive prompts;
//     Flag/non-TTY operation is CI-safe.
//  2. AGENT phase (handoff): launch a normal `pix run` whose FIRST pi
//     message kicks off the `onboarding` skill, so the agent PROACTIVELY starts
//     the conversation (identity, tone, a real first task) instead of sitting
//     silent — the passive system-prompt marker never spoke until the user
//     typed, which is the bug this replaces.
//
// `pix run` on its own NEVER onboards. `pix setup --no-agent` is the host-only,
// no-handoff path for CI.
package setup

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"pix/host/cli"
	"pix/host/hostenv"
	"pix/host/launcher"
	"pix/host/mcp"
	"pix/host/readiness"
	"pix/host/readiness/axis"
	"pix/host/routing"
	"pix/host/secret"
	"pix/host/sys"
	"pix/host/workflow/doctor"
	"pix/host/workflow/gworkspace"
	"pix/host/workflow/launch"
	"pix/host/workflow/onboard"
	"pix/host/workflow/pack"
	"slices"
	"sort"
	"strings"
	"time"

	"pix/host/config"
	"pix/host/inference"
	"pix/host/rpc"
	"pix/host/workspace"
)

// OnboardingKickoff is the first message `setup` hands the agent. It is
// DELIBERATELY short and human — it reads like something the user would type,
// not a machine directive wall. The rewritten `onboarding` skill owns the actual
// flow (guided teach, read host-state, land a task); the word "guided" is all it
// needs to pick GUIDED mode. (Making this fully invisible — agent greets with no
// visible prompt at all — needs a session-start extension + an image rebuild;
// tracked as a follow-up.) It carries launch.GeneratedInputMarker so memory-capture.ts
// can tell this was machine-generated, not typed by the user.
const OnboardingKickoff = launch.GeneratedInputMarker + "I just ran pix setup. Give me the upfront guide and help me get started."

// RunSetupCore validates DIR (reusing launch.ValidateRunWorkspace's exists-and-is-a-
// directory check — the same rule `pix run` enforces, so setup and run
// never disagree about what counts as a launchable DIR) and, ONLY if that
// passes, invokes hostPhase. Extracted as its own tiny function — rather than
// inlining the check in runSetupCmd — so a nonexistent/file DIR is provably
// caught BEFORE hostPhase (which mutates op-refs.env/hostmode.env/config.toml/
// the default pack/memory/host-mode) ever runs: a test can pass a hostPhase
// stub that fails the test if called, and assert on the returned error alone,
// without needing to exercise runSetupCmd's os.Exit calls.
func RunSetupCore(env hostenv.Env, dir string, hostArgs []string, in io.Reader, out io.Writer, tty bool, hostPhase func(hostenv.Env, []string, io.Reader, io.Writer, bool) error) error {
	if err := launch.ValidateRunWorkspace(dir); err != nil {
		return err
	}
	return hostPhase(env, hostArgs, in, out, tty)
}

// RunSetupHandoff is the pure post-host-phase decision + action, extracted so
// the state/replace matrix is testable without exercising os.Exit or actually
// exec'ing sbx (runFn is called instead of runRun directly; tests pass a stub
// that records the call). Returns an error ONLY for the fail-closed unknown
// state; the caller prints it and exits non-zero.
func RunSetupHandoff(dir, name string, state doctor.SbxState, replace bool, out io.Writer, runFn func([]string)) error {
	// kickoffArgs builds the runRun argv for a launch that should receive the
	// tour: [DIR] [--replace] -- <OnboardingKickoff>. DIR is forwarded only
	// when explicit so `pix setup` from inside a repo behaves exactly
	// like `pix run` there. --replace is harmless on an absent sandbox
	// (create path) and forces the recreate on an existing one.
	kickoffArgs := func() []string {
		args := []string{}
		if dir != "." {
			args = append(args, dir)
		}
		if replace {
			args = append(args, "--replace")
		}
		return append(args, "--", OnboardingKickoff)
	}
	dirArg := ""
	if dir != "." {
		dirArg = " " + sys.ShellQuote(dir)
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
	case launch.SbxUnknown:
		// FAIL CLOSED: we could not determine whether a sandbox exists (sbx
		// errored/missing, or the name could not be resolved). Never launch:
		// runRun would re-attach a live session and replay the kickoff into it.
		// The host phase already completed, so a retry is cheap.
		which := fmt.Sprintf("sandbox %q", name)
		if name == "" {
			which = fmt.Sprintf("the sandbox for %s", dir)
		}
		return fmt.Errorf("cannot determine the state of %s (`sbx ls` failed or sbx is unavailable). Host setup completed; install or fix sbx (`%s`) and retry with: pix setup%s", which, doctor.SbxInstallHint, retryArg)
	case launch.SbxRunning, launch.SbxStopped:
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
		fmt.Fprintf(out, "  pix run%s              # reattach as-is\n", dirArg)
		fmt.Fprintf(out, "  pix setup%s --replace  # recreate with current settings + get the tour\n", dirArg)
		return nil
	}

	// launch.SbxAbsent (positively confirmed): normal first launch — hand off to the
	// in-VM onboarding agent via an initial message. A --replace here is
	// harmless (the create path ignores it).
	fmt.Fprintln(out, "")
	if !setupTranscriptVerbose {
		fmt.Fprintln(out, "Launching Pix — the agent will take it from here.")
	} else {
		fmt.Fprintln(out, "Launching sandbox: pi will introduce itself, show you how it works,")
		fmt.Fprintln(out, "and get you into a real task. (You can quit any time; just run `pix run`.)")
	}
	runFn(kickoffArgs())
	return nil
}

// SetupHostPhase does the deterministic host configuration and reports what is
// (and is not) ready. The only interactive step is pasting op:// refs for
// providers missing one (TTY + op installed); with flags OR no TTY it is fully
// non-interactive (the CI path).
// SetupInteractivePrompts decides whether setup's key-collection/overwrite
// prompts fire: a real TTY, unless the caller explicitly opted out with
// --yes/-y/--non-interactive. Deliberately does NOT take the parsed flag list
// — ordinary value flags (--account/--knowledge/--mcp/--model) configure host
// settings and must never silently suppress the mandatory key prompts; only
// assumeYes (the explicit opt-out) does. Extracted as its own tiny function so
// this exact invariant has a direct regression test, not just end-to-end
// coverage through SetupProvisionKeys.
func SetupInteractivePrompts(tty, assumeYes bool) bool {
	return tty && !assumeYes
}

// ---------------------------------------------------------------------------
// The setup phase machine (AC-P0-301).
//
// `pix setup` runs as a NUMBERED TRANSCRIPT of eight phases, and each
// phase header is printed BEFORE that phase does any work — so a run that
// hangs names the phase it hung in instead of leaving the user staring at a
// blank terminal.
//
//	parse      read flags; argument mistakes exit 2 before any probe
//	inventory  read the current host state; NOTHING is written
//	gate       preconditions that must hold before the first mutation
//	mutate     the fixed-order, individually idempotent writes
//	consent    bounded interactive questions and what they authorize
//	verify     re-probe what was just changed
//	report     render, purely from the post-mutation evidence
//	handoff    launch the sandbox (skipped by --no-agent)
//
// Two invariants make the transcript trustworthy and are worth stating
// separately, because both were bugs before:
//
//   - The MUTATE phase returns no user-facing success strings at all — only
//     the set of readiness axes it touched (AC-P0-302). Every ✓ the user reads
//     comes from the report, which renders post-mutation probes. A mutation
//     that fails therefore cannot print a ✓ for its axis, because it never had
//     the ability to print one.
//   - Mutations run in a FIXED order with the riskiest last (AC-P0-303):
//     keys → config → pack → MCP → knowledge → identity → Google Workspace →
//     model pulls. Each step is individually idempotent, so an interrupted run
//     is resumed by simply re-running setup: the next run re-probes and
//     re-applies, and no journal file is consulted (a journal is state that
//     can itself be stale — trusting recorded over observed state is the exact
//     defect this command exists to remove).
const (
	setupPhaseParse     = "parse"
	setupPhaseInventory = "inventory"
	setupPhaseGate      = "gate"
	setupPhaseMutate    = "mutate"
	setupPhaseConsent   = "consent"
	setupPhaseVerify    = "verify"
	setupPhaseReport    = "report"
	SetupPhaseHandoff   = "handoff"
)

// SetupPhaseOrder is the transcript, in order. The index in this slice is the
// number the header prints, so the phases can never be renumbered by accident.
// SetupPhase pairs a phase's identifier with what it does. Named rather than
// anonymous because cmd/pix's transcript test reads both fields, and an
// anonymous struct cannot expose them across a package boundary.
type SetupPhase struct{ Name, What string }

var SetupPhaseOrder = []SetupPhase{
	{setupPhaseParse, "reading flags"},
	{setupPhaseInventory, "reading the current host state (nothing is written yet)"},
	{setupPhaseGate, "checking preconditions before anything is written"},
	{setupPhaseMutate, "applying host configuration"},
	{setupPhaseConsent, "the things that cost you something"},
	{setupPhaseVerify, "re-probing what changed"},
	{setupPhaseReport, "what is actually ready"},
	{SetupPhaseHandoff, "launching the sandbox"},
}

// SetupPhaseHeader prints `[n/8] <phase> — <what>` BEFORE the phase runs.
// Pass a non-empty override to say something more specific than the default
// (e.g. that the handoff was skipped on purpose).
func SetupPhaseHeader(out io.Writer, name, override string) {
	if !setupTranscriptVerbose {
		return
	}
	for i, p := range SetupPhaseOrder {
		if p.Name != name {
			continue
		}
		what := p.What
		if override != "" {
			what = override
		}
		fmt.Fprintf(out, "\n[%d/%d] %s — %s\n", i+1, len(SetupPhaseOrder), p.Name, what)
		return
	}
}

var setupTranscriptVerbose bool

// SetupMaxPrompts is the hard cap on interactive questions ONE setup run may
// ask (AC-P0-307). There are exactly two: model-pull consent and the Google
// Workspace route. Pasting a 1Password ref in the keys step is not counted —
// it is not a question with a default, it is the mandatory input to a hard
// precondition, and a run that reaches it has already failed closed without it.
const SetupMaxPrompts = 2

// SetupPromptBudget enforces the cap. Every setup-owned prompt site must take
// its slot from here BEFORE prompting; a site that cannot get one falls back to
// its non-interactive behavior (which is always the safe default: don't pull,
// don't authorize). Non-interactive runs hand out no slots at all, which is how
// "non-TTY never prompts" (AC-P0-306) is enforced in one place instead of at
// each call site.
type SetupPromptBudget struct {
	Interactive bool
	Spent       int
	Asked       []string
}

// reserve claims one prompt slot for what, reporting whether the caller may
// prompt. It is deliberately EAGER (claimed when the site is reached, not when
// the question is finally printed) so the budget is a static property of the
// run rather than something that depends on probe results.
func (b *SetupPromptBudget) Reserve(what string) bool {
	if b == nil || !b.Interactive || b.Spent >= SetupMaxPrompts {
		return false
	}
	b.Spent++
	b.Asked = append(b.Asked, what)
	return true
}

// setupInventory is the PRE-mutation read of the host: what setup found before
// it changed anything. It is consumed by the gate and by the mutation steps —
// and NEVER by the report, which is a pure function of post-mutation evidence
// (AC-P0-302, guarded by TestSetupReport_NeverReadsInventory).
type setupInventory struct {
	cfg      *config.Config
	proposal *onboard.OnboardingResult
	retired  []string
}

// TakeSetupInventory reads current state. It writes NOTHING: every call in
// here is a load, a parse, or a bounded probe.
func TakeSetupInventory(env hostenv.Env, opts onboard.Opts) (setupInventory, error) {
	cfg, err := config.Load()
	if err != nil {
		return setupInventory{}, fmt.Errorf("loading config: %w", err)
	}
	inv := setupInventory{
		cfg:      cfg,
		retired:  cfg.RetiredKeys(),
		proposal: setupProposal(opts),
	}
	return inv, nil
}

// setupProposal is the single flag -> proposal translation used by both the
// pre-adoption semantic validator and the later inventory/mutation phase.
// Keeping one constructor prevents the early safety boundary from accepting a
// value that the host phase interprets differently.
func setupProposal(opts onboard.Opts) *onboard.OnboardingResult {
	p := &onboard.OnboardingResult{
		Version:           1,
		MCP:               append([]string(nil), opts.Mcp...),
		OllamaBridgeModel: strings.TrimSpace(opts.Model),
	}
	if k := strings.TrimSpace(opts.Knowledge); k != "" {
		p.Knowledge = &onboard.Knowledge{Action: "use", Source: k}
	}
	return p
}

// ValidateSetupSemantics checks only built-in argument meaning. It performs no
// writes and opens no authorization flow, so runSetupCmd can call it before the
// first pack is adopted. External readiness (catalog OAuth, provider reachability,
// model pulls) remains in the later gate/verify phases.
// DefaultEnv, HostBinary, Register and Credentials are the composition setup
// cannot perform: building a real env, resolving the paired pix-host, and
// registering MCP servers with credentials resolved over secret. setup
// sequences nearly every other workflow, so it needs the widest set of these in
// the tree — which is what a top-level guided flow looks like, not a smell.
//
// The env default PANICS rather than returning a half-wired one, for the same
// reason launch.DefaultEnv does: a setup that silently probes nothing is the
// failure mode this refactor exists to delete.
var (
	DefaultEnv = func() hostenv.Env {
		panic("setup: DefaultEnv not wired — the composition root must set it")
	}
	HostBinary  = launcher.FindHostBinary
	Register    RegisterFn
	Credentials func(hostenv.Env) mcp.Credentials
)

// RegisterFn registers the named servers with the sbx gateway.
type RegisterFn func(cfg *config.Config, env hostenv.Env, out io.Writer, names []string,
	hostResolver func() (string, error), containers map[string]config.MCPContainer) error

func ValidateSetupSemantics(opts onboard.Opts, cfg *config.Config, env hostenv.Env, hostResolver func() (string, error)) error {
	if len(opts.WithSetup) > 0 && len(opts.Packs) == 0 {
		return ErrUsage{fmt.Errorf("--with requires --pack")}
	}
	if err := CheckGoogleWorkspaceFlags(opts); err != nil {
		return err
	}
	if err := onboard.ValidateOnboardingResult(setupProposal(opts), cfg, env, hostResolver); err != nil {
		return ErrUsage{err}
	}
	return nil
}

// setupGate is every precondition that must hold BEFORE the first mutation.
// Each failure names the exact command that fixes it and returns an error, so
// nothing is half-written when a run cannot succeed:
//
// Built-in semantic flag/value validation has already run before pack adoption
// in runSetupCmd (and immediately after inventory for direct callers). This gate
// owns only external readiness that cannot be established from argument meaning:
//   - a shipped-catalog MCP remote that is not registered AND auth-ready fails
//     here rather than being persisted on the promise of a later fix;
//
// The 1Password preconditions (op installed, op signed in, every provider ref
// resolvable, and the non-interactive "this needs a human" refusal) are NOT
// duplicated here: they belong to the keys step, which is the FIRST mutation
// and fails closed before it writes anything, so a gate copy would be a second
// implementation of the same rule that could drift from it.
func setupGate(env hostenv.Env, inv setupInventory, out io.Writer, interactive bool) error {
	// Shipped-catalog remotes (mcp.McpCatalogNames) must be registered AND
	// auth-ready BEFORE setup writes anything — setup must never claim success
	// for a server the gateway cannot spawn or that 401s on first use. The gate
	// covers both the new --mcp proposal and any catalog name already persisted
	// in cfg.MCP (the handoff would preload it too). It probes with bounded
	// native checks only and never opens an OAuth flow, so a non-interactive
	// setup can't trigger a browser grant.
	if err := VerifyCatalogMCPReady(env, append(append([]string{}, inv.proposal.MCP...), inv.cfg.MCP...)); err != nil {
		return err
	}
	return nil
}

// SetupMutationStep is one idempotent write, named for the transcript and
// tagged with the readiness axes it touches. run() returns an error or nil and
// writes NO success prose (see setupMutationOut).
type SetupMutationStep struct {
	Name string
	Axes []readiness.Axis
	// fatal marks a step whose failure aborts setup. A non-fatal step reports
	// its own failure and lets the run continue to the report, which will show
	// the axis as not ready — the failure is never swallowed, it just is not
	// worth throwing away the rest of a working host over.
	Fatal bool
	Run   func() error
}

// SetupMutationOrder is the FIXED order (AC-P0-303), riskiest last, named
// here so the order is a value a test can assert on rather than a property of
// the control flow. gworkspace, models and inference sit at the end because
// they are the only steps that talk to the user; models is second-to-last
// because it is the only step that can cost gigabytes, and inference is last
// because it can only judge what models left behind.
var SetupMutationOrder = []string{"keys", "config", "pack", "mcp", "knowledge", "identity", "gworkspace", "models", "inference"}

// RunSetupMutations executes steps in order and returns the axes it touched.
// It returns NO user-facing strings (AC-P0-302): the report is rendered from
// post-mutation probes, so a stubbed-to-fail mutation cannot print a ✓ for its
// axis. Steps that must talk to the user (the keys step's ref prompt, a
// non-fatal step's failure line) write diagnostics, never success claims.
func RunSetupMutations(steps []SetupMutationStep) (touched []readiness.Axis, err error) {
	for _, s := range steps {
		if e := s.Run(); e != nil {
			if s.Fatal {
				return touched, e
			}
			err = e
		}
		touched = append(touched, s.Axes...)
	}
	return touched, err
}

// SetupMutationSteps builds the ordered step table. Every closure here writes
// to io.Discard unless it is reporting a failure or collecting mandatory input.
func SetupMutationSteps(env hostenv.Env, inv setupInventory, opts onboard.Opts, in io.Reader, out io.Writer, interactive bool, models *SetupModelsOutcome, prompts *SetupPromptBudget) []SetupMutationStep {
	cfg := inv.cfg
	return []SetupMutationStep{{
		Name:  "keys",
		Axes:  []readiness.Axis{readiness.AxisProviders, readiness.AxisSecrets},
		Fatal: true,
		Run: func() error {
			selected, err := SetupChooseInference(cfg, env, in, out, interactive)
			if err != nil {
				return err
			}
			// GitHub is not an inference provider, but gh is a core sandbox CLI.
			// Reuse an existing host login without another prompt/browser flow.
			// It remains optional: an unauthenticated host does not block setup.
			if err := SyncGitHubCredentialFromHost(env); err != nil {
				fmt.Fprintf(out, "  github: host credential was not synced (%v)\n", err)
			}
			if selected {
				// The roster moved to the `inference` step: an Ollama binding is only a
				// CANDIDATE here (its weights may not be pulled until the models step),
				// so choosing a roster now would either offer an unproven model or
				// hard-fail a user whose first setup has not downloaded anything yet.
				return cfg.Save()
			}
			if err := EnsureSetupPrereqsFor(env, in, out, interactive, true); err != nil {
				return err
			}
			// The ONLY mutation that may write to the real terminal: on a TTY
			// it collects the mandatory op:// refs, and on failure it prints
			// exactly what is wrong. It prints no ✓ — the keys row in the
			// report comes from secret.HostModeProviderKeys AFTER this ran.
			if !SetupProvisionKeysFn(env, in, out, interactive, opts.AssumeYes) {
				return fmt.Errorf("provider keys not fully configured — follow the fix printed above")
			}
			// Bind -> verify -> save -> judge -> roster, all of it in
			// ReconcileDirectInference so `pix models add` runs the IDENTICAL
			// sequence. It living only here is why a key added any other way stayed
			// inert: the ref was written and nothing ever rebuilt the bindings.
			res, err := ReconcileDirectInference(cfg, env, in, out, interactive, opts.Models, "")
			if err != nil {
				return err
			}
			if res.Verified > 0 && len(res.Failures) > 0 {
				fmt.Fprintf(out, "  inference: %d model(s) verified; %d candidate(s) unavailable or unauthorized (%s)\n",
					res.Verified, len(res.Failures), strings.Join(res.Failures, "; "))
			}
			return nil
		},
	}, {
		Name:  "config",
		Fatal: true,
		Run: func() error {
			// Retired config keys (mcp_static/mcp_dynamic) are dropped by the
			// sparse encode whenever the config is saved; do it here so the
			// migration is deterministic even if a later step fails.
			if len(inv.retired) > 0 {
				if err := cfg.Save(); err != nil {
					return fmt.Errorf("dropping retired config keys: %w", err)
				}
			}
			// Config only: the knowledge half of the proposal is its own,
			// later step so the fixed order is real and not an illusion of one
			// combined call.
			cfgOnly := *inv.proposal
			cfgOnly.Knowledge = nil
			_, err := onboard.ApplyOnboardingResult(&cfgOnly, cfg, env, io.Discard, func(c *config.Config) error { return c.Save() })
			if err != nil {
				return err
			}
			if SetupSelectRunnableIntent(cfg, env) {
				return cfg.Save()
			}
			return nil
		},
	}, {
		Name:  "pack",
		Axes:  nil,
		Fatal: true,
		Run: func() error {
			// Packs are explicit (`pix setup --pack ...`). Personal AGENTS.md and
			// skills live in XDG_DATA_HOME/pix/context, so default setup must not
			// manufacture a git repo or introduce the pack concept.
			return nil
		},
	}, {
		Name: "mcp",
		Axes: mcpAxes(cfg.MCP),
		Run: func() error {
			if len(cfg.MCP) == 0 {
				return nil
			}
			var buf bytes.Buffer
			if err := Register(cfg, env, &buf, nil, HostBinary, pack.ActiveContainerMCP(cfg)); err != nil {
				fmt.Fprintf(out, "  mcp register skipped: %v (finish later: pix mcp register)\n", err)
				return err
			}
			return nil
		},
	}, {
		Name:  "knowledge",
		Axes:  []readiness.Axis{readiness.AxisServiceKnowledge},
		Fatal: true,
		Run: func() error {
			if inv.proposal.Knowledge == nil {
				return nil
			}
			only := &onboard.OnboardingResult{Version: 1, Knowledge: inv.proposal.Knowledge}
			_, err := onboard.ApplyOnboardingResult(only, cfg, env, io.Discard, func(c *config.Config) error { return c.Save() })
			return err
		},
	}, {
		Name: "identity",
		Run: func() error {
			// Read the user's first name from the HOST's git config (the
			// sandbox cannot see ~/.gitconfig) and seed it into memory so
			// onboarding can greet by name. Best-effort and SILENT: the report
			// re-reads git config itself, so nothing here needs to claim
			// anything.
			SeedIdentity(env, io.Discard)
			return nil
		},
	}, {
		Name: "gworkspace",
		Axes: []readiness.Axis{readiness.AxisGworkspace},
		Run: func() error {
			// Google Workspace is OFF unless --google-workspace. It runs the
			// SAME transaction `pix gworkspace setup` runs, through the
			// same façade, so there is exactly one writer. It returns no
			// success text: the row in the report is rendered from a
			// post-mutation probe, so a half-finished authorization can never
			// print a ✓.
			if !opts.GoogleWorkspace {
				return nil
			}
			ask := prompts.Reserve("google ws route")
			if err := setupGoogleWorkspaceFn(env, gworkspace.GogSetupOpts{
				Account:     strings.TrimSpace(opts.Account),
				Credentials: strings.TrimSpace(opts.Credentials),
				AssumeYes:   opts.AssumeYes,
			}, in, out, ask); err != nil {
				return fmt.Errorf("google ws: %w", err)
			}
			return nil
		},
	}, {
		Name: "models",
		Axes: []readiness.Axis{readiness.AxisModelWatcher, readiness.AxisModelEmbed, readiness.AxisModelBridge},
		Run: func() error {
			// The riskiest step, therefore last: probe Ollama once, classify on
			// the shared axis.ModelReadiness axes, pull confirmed-missing tags only
			// under explicit consent (--pull-models, or the one default-No
			// prompt), verify once after the pulls, receipt the outcome. Never
			// installs Ollama; never pulls a tag it could not positively verify
			// as missing.
			// Local models are progressive enhancement. When Ollama is already
			// healthy, interactive setup may offer a default-No pull for positively
			// missing memory models; unattended setup requires --pull-models.
			ask := prompts.Reserve("enable local memory")
			*models = SetupLocalModels(cfg, env, in, out, ask, opts.PullModels)
			if setupMemoryModelsReady(cfg, *models) {
				cfg.AddService("memory")
				if err := cfg.Save(); err != nil {
					return fmt.Errorf("enabling verified memory: %w", err)
				}
			}
			receiptSetupModels(env, out, *models)
			return nil
		},
	}, {
		Name:  "inference",
		Axes:  []readiness.Axis{readiness.AxisProviders},
		Fatal: false,
		Run: func() error {
			// LAST, and non-fatal: it can only judge what the models step left
			// behind, and a probe failure must not stop the report from rendering
			// the axes the run did touch. It prints no routine success line —
			// success words come from the post-mutation probe (AC-P0-302).
			return RunSetupInferenceStep(cfg, env, in, out, interactive, *models)
		},
	}}
}

// RunSetupInferenceStep verifies ollama bindings with real requests, picks the
// roster from what actually answered, and then branches on the ONE question
// that matters: is there anything callable, and if not, whose decision was it.
//
// Declining a multi-gigabyte download is a decision, not a failure: it returns
// nil and setup exits 0 with an honest `✗ inference` summary. A non-zero exit
// stays reserved for probes that were dispatched and refused, and for a pull
// that was consented to and then failed (which the models step already reports
// — a second error would double-report one cause).
func RunSetupInferenceStep(cfg *config.Config, env hostenv.Env, in io.Reader, out io.Writer, interactive bool, models SetupModelsOutcome) error {
	probe, err := VerifyOllamaInference(cfg, env, out)
	if err != nil {
		return fmt.Errorf("verifying ollama models: %w", err)
	}
	attempted, verified, failures, notProbed := probe.Attempted, probe.Verified, probe.Failures, probe.NotProbed
	callable, _ := axis.ConfiguredInferenceSummary(cfg)
	if callable > 0 {
		// Deviation from the design: the roster prompt is NOT taken from
		// prompts.reserve. SetupMaxPrompts is 2 and both slots are already claimed
		// (gworkspace, models), so reserving here would deny the prompt and
		// silently auto-select every candidate — a regression, not a budget fix.
		if err := ConfigureModelRoster(cfg, in, out, interactive, ""); err != nil {
			return fmt.Errorf("choosing models: %w", err)
		}
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	if verified > 0 {
		if len(failures) > 0 {
			fmt.Fprintf(out, "  inference: %d model(s) verified; %d candidate(s) unavailable or unauthorized (%s)\n",
				verified, len(failures), strings.Join(failures, "; "))
		}
		return nil
	}
	// Cloud was selected but nothing on the plan answered: that is a hard
	// failure, because a silent "configured" for an account that can call
	// nothing is the exact class of claim this whole path exists to delete.
	if cloud := doctor.OllamaCloudCandidates(cfg); len(cloud) > 0 && attempted > 0 {
		return fmt.Errorf("Ollama Cloud was selected, but no cloud model answered a request: %s. Sign in with `ollama signin`, then re-run `pix setup`",
			strings.Join(failures, "; "))
	}
	// A DECLINED (or never-offered) pull explains the failure completely: the
	// weights are not on disk, so the probe had nothing to answer with. That is
	// the documented contract of this whole step — "declining a multi-gigabyte
	// download is a decision, not a failure" — and it belongs ahead of the
	// generic hard error below, which would otherwise exit non-zero for a user
	// who simply said no.
	//
	// This was live and invisible: the test covering it wired NO probe, so
	// `attempted` was 0 and control fell through to the consent switch by
	// accident. With a probe that actually refuses — which is what a real host
	// does when the tag was never pulled — `pix setup`, choose Ollama local,
	// decline the download, exited non-zero.
	//
	// Cloud is deliberately excluded (handled above): an entitlement refusal is
	// not explained by a download nobody started.
	declinedPull := models.Consent == "none" || models.Consent == "prompt-no"
	if attempted > 0 && !declinedPull {
		return fmt.Errorf("ollama models are bound, but none answered a request: %s", strings.Join(failures, "; "))
	}
	if len(axis.UnverifiedOllamaCandidates(cfg)) == 0 {
		return nil // nothing ollama-shaped here; the keys step owns this host
	}
	switch models.Consent {
	case "--pull-models", "prompt-yes":
		// The pull was consented to and did not produce a callable model. The
		// models step already failed with the exact retry command and owns the
		// non-zero exit; repeating it here would report one cause twice.
		return nil
	default:
		// Declined or never asked. Print the truth, claim nothing, exit 0.
		if len(notProbed) > 0 {
			fmt.Fprintf(out, "  inference: %d candidate(s) not probed (the local budget ran out) — re-run: pix setup\n", len(notProbed))
		}
		fmt.Fprintf(out, "  inference: no model has passed a probe yet — pull one: %s\n", axis.PullModelsFixCmd)
		return nil
	}
}

// SyncGitHubCredentialFromHost mirrors the current host gh login into sbx's
// global github service. The token exists only in process memory and the child
// argv accepted by sbx (the same unavoidable boundary used by provider-key
// sync); output/errors are redacted before they can reach a transcript.
func SyncGitHubCredentialFromHost(env hostenv.Env) error {

	if _, err := env.LookPath("gh"); err != nil {
		return nil
	}
	if _, err := env.LookPath("sbx"); err != nil {
		return nil
	}
	token, err := env.Run("gh", "auth", "token")
	if err != nil || strings.TrimSpace(token) == "" {
		return nil // optional: no host login to reuse
	}
	token = strings.TrimSpace(token)
	out, err := env.Run("sbx", "secret", "set", "github", "-f", "-t", token)
	if err == nil {
		return nil
	}
	detail := secret.RedactSecretValue(strings.TrimSpace(secret.FirstLine(out)), token)
	if detail == "" {
		detail = secret.RedactSecretValue(err.Error(), token)
	}
	return fmt.Errorf("sbx secret set github failed: %s", detail)
}

// mcpAxes maps configured server names to their readiness axes.
func mcpAxes(servers []string) []readiness.Axis {
	var out []readiness.Axis
	for _, s := range servers {
		if strings.TrimSpace(s) == "" {
			continue
		}
		out = append(out, readiness.MCPAxis(s))
	}
	return out
}

// SetupHostPhase runs the host half of `pix setup` as the eight-phase
// transcript documented above. The only interactive steps are the mandatory
// op:// ref collection (TTY + op installed) and bounded consent questions;
// with --yes/--non-interactive or no TTY it is fully
// non-interactive (the CI path).
func SetupHostPhase(env hostenv.Env, flags []string, in io.Reader, out io.Writer, tty bool) error {
	setupTranscriptVerbose = !env.Quiet
	if !env.Quiet {
		fmt.Fprintln(out, "pix setup — configuring the host")
	}

	// PHASE 1 — parse. Argument mistakes are caught here, before any probe or
	// mutation, and map to exit 2 at the call site.
	SetupPhaseHeader(out, setupPhaseParse, "")
	opts, perr := onboard.ParseOnboardArgs(flags)
	if perr != nil {
		return ErrUsage{perr}
	}
	if opts.Apply {
		// --apply is intercepted by runSetupCmd (it reconciles a pending
		// onboarding.json and stops). Reaching the host phase with it set means
		// a caller bypassed that route, which would silently ignore the flag.
		return ErrUsage{fmt.Errorf("--apply is handled before the host phase; run `pix setup [DIR] --apply`")}
	}
	// Interactive prompts fire on any real TTY unless the caller explicitly opted
	// out with --yes/-y/--non-interactive (opts.AssumeYes). Ordinary VALUE flags
	// (--account/--knowledge/--mcp/--model) configure host settings; they say
	// nothing about whether pasting a 1Password ref should still prompt, so their
	// mere presence must NOT silently suppress the key-collection/overwrite
	// prompts — only an explicit non-interactive opt-out does.
	interactive := SetupInteractivePrompts(tty, opts.AssumeYes)
	prompts := &SetupPromptBudget{Interactive: interactive}

	// PHASE 2 — inventory. Reads only.
	SetupPhaseHeader(out, setupPhaseInventory, "")
	inv, err := TakeSetupInventory(env, opts)
	if err != nil {
		return err
	}
	if err := ValidateSetupSemantics(opts, inv.cfg, env, HostBinary); err != nil {
		return err
	}
	if len(inv.retired) > 0 {
		fmt.Fprintf(out, "note: dropping retired config key(s) %s on save (no longer read); every configured MCP server preloads at sandbox create\n", strings.Join(inv.retired, ", "))
	}

	// PHASE 3 — gate. Nothing has been written yet; a failure here leaves the
	// host exactly as it was found.
	SetupPhaseHeader(out, setupPhaseGate, "")
	if err := setupGate(env, inv, out, interactive); err != nil {
		return err
	}

	// PHASE 4 — mutate, and PHASE 5 — consent. One ordered step table
	// (SetupMutationOrder), split at the point where the steps start asking
	// permission: the first six are unattended, the last three (gworkspace,
	// models, inference) are the consented, riskiest-last group.
	var models SetupModelsOutcome
	steps := SetupMutationSteps(env, inv, opts, in, out, interactive, &models, prompts)
	split := len(steps) - 3
	SetupPhaseHeader(out, setupPhaseMutate, "")
	if _, err := RunSetupMutations(steps[:split]); err != nil {
		return err
	}
	SetupPhaseHeader(out, setupPhaseConsent, "")
	if _, err := RunSetupMutations(steps[split:]); err != nil {
		return err
	}

	// PHASE 6 — verify. Re-probe, from scratch, everything the mutations
	// touched. Nothing recorded by the mutate phase is trusted here.
	SetupPhaseHeader(out, setupPhaseVerify, "")
	postCfg, cerr := config.Load()
	if cerr != nil {
		postCfg = inv.cfg
	}
	req := readiness.RequestAll(postCfg.MCP, SetupRequestedAxes(opts)...)
	snap := readiness.Build(req, setupReadinessAxes(postCfg, env, models))

	// PHASE 7 — report. A pure function of the post-mutation snapshot: it
	// takes no inventory, no mutation log, and no "what we meant to do".
	SetupPhaseHeader(out, setupPhaseReport, "")
	PrintSetupSummary(postCfg, env, out, models)

	if !env.Quiet {
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "host mode (optional, UNSANDBOXED: runs `pi` directly on the host): not enabled.")
		fmt.Fprintln(out, "  set it up only if you need it:  pix host setup")
	}

	// A partial pull failure is a real, verified gap the user consented to
	// closing: fail setup (non-zero) with the exact retry commands. The summary
	// above already reported it truthfully.
	if len(models.failed) > 0 {
		return fmt.Errorf("local model pull failed for %s — retry by hand: ollama pull %s, then re-run pix setup",
			strings.Join(models.failed, ", "), strings.Join(models.failed, "; ollama pull "))
	}
	// An axis the user explicitly ASKED for on this invocation and that did not
	// end ready is a failed request, not a shrug: exit 1 (AC-P0-210). Stale
	// optional config never blocks unrelated repair, because only the axes this
	// invocation's flags promoted are consulted.
	if short := snap.RequestedShortfall(req); len(short) > 0 {
		return fmt.Errorf("%s — see the rows above; nothing else was left half-done", requestedShortfallMessage(short, snap))
	}
	return nil
}

// SetupRequestedAxes maps THIS invocation's flags to the axes they promote from
// optional to blocking (AC-P0-209). Promotion itself lives in the readiness
// type (build); this is only the flag→axis mapping, so no command
// re-implements the rule.
//
// `--mcp X` promotes `mcp:X`, which setup additionally enforces in the gate
// (VerifyCatalogMCPReady) — a requested server that cannot come up fails before
// anything is written, which is strictly earlier than an exit code.
func SetupRequestedAxes(opts onboard.Opts) []readiness.Axis {
	var out []readiness.Axis
	if opts.PullModels {
		out = append(out, readiness.AxisOllamaHost, readiness.AxisModelWatcher, readiness.AxisModelEmbed, readiness.AxisModelBridge)
	}
	if opts.GoogleWorkspace {
		out = append(out, readiness.AxisGworkspace)
	}
	out = append(out, mcpAxes(opts.Mcp)...)
	return out
}

// requestedShortfallMessage names the requested axes that did not end ready,
// in snapshot order, with the verdict word for each — so the exit-1 line says
// which request failed and how, never just "setup failed".
func requestedShortfallMessage(short []readiness.Axis, s readiness.Snapshot) string {
	parts := make([]string, 0, len(short))
	for _, a := range short {
		_, v, ok := s.AxisVerdict(a)
		if !ok {
			continue
		}
		parts = append(parts, string(a)+": "+readiness.VerdictWord(v))
	}
	return "you asked for " + strings.Join(parts, ", ")
}

// setupReadinessAxes is the builder set for setup's VERIFY phase: the shared
// Ollama/model and service builders doctor uses (so setup and doctor can never
// disagree), plus the three axes only setup's own post-mutation reads can speak
// to. Every builder here probes; none reads the inventory.
func setupReadinessAxes(cfg *config.Config, env hostenv.Env, models SetupModelsOutcome) map[readiness.Axis]readiness.AxisBuilder {
	builders := map[readiness.Axis]readiness.AxisBuilder{}
	for a, b := range doctor.OllamaReadinessAxes(cfg, env, "", nil) {
		builders[a] = b
	}
	if env.IdentityProbe != nil {
		for a, b := range axis.ServiceReadinessAxes(env, config.ServiceEnabled(cfg, "memory"), config.ServiceEnabled(cfg, "knowledge"), env.IdentityProbe) {
			builders[a] = b
		}
	}
	builders[readiness.AxisProviders] = func() []readiness.Check { return SetupProvidersAxis(cfg, env) }
	if strings.TrimSpace(cfg.Pack) != "" {
		builders[readiness.AxisPack] = func() []readiness.Check { return setupPackAxis(cfg) }
	}
	if strings.TrimSpace(cfg.GogAccount) != "" || slices.Contains(cfg.MCP, config.GWServerName) {
		// Absent by default (AC-P0-319): with no opt-in there is no axis at
		// all, so the report says nothing about Google Workspace.
		builders[readiness.AxisGworkspace] = func() []readiness.Check { return setupGworkspaceAxis(cfg, env) }
	}
	return builders
}

// SetupProvidersAxis is the post-mutation provider-key fact: ready when at
// least one model-provider ref resolves (any one key launches a sandbox).
func SetupProvidersAxis(cfg *config.Config, env hostenv.Env) []readiness.Check {
	if cfg != nil && len(cfg.Inference.Models) > 0 {
		callable := 0
		candidates := 0
		for _, b := range cfg.Inference.Models {
			if b.Available && inference.Allowed(cfg, b) {
				candidates++
				if b.Verified {
					callable++
				}
			}
		}
		if callable > 0 {
			detail := fmt.Sprintf("%d callable model(s)", callable)
			if candidates > callable {
				detail += fmt.Sprintf("; %d candidate(s) did not pass live verification", candidates-callable)
			}
			return []readiness.Check{{Label: "inference", Requirement: readiness.RequirementCore, Verdict: readiness.VerdictReady,
				Detail: detail, Evidence: "model-specific live inference probes"}}
		}
		if candidates > 0 {
			return []readiness.Check{{Label: "inference", Requirement: readiness.RequirementCore, Verdict: readiness.VerdictUnverifiable,
				Detail: fmt.Sprintf("%d configured model candidate(s)", candidates), Evidence: "first sandbox inference is the live probe"}}
		}
		return []readiness.Check{{Label: "inference", Requirement: readiness.RequirementCore, Verdict: readiness.VerdictTodo,
			Detail: "no callable model", Evidence: "configured bindings have no successful probe"}}
	}
	names, err := secret.HostModeProviderKeys(env)
	switch {
	case err != nil:
		return []readiness.Check{{Label: "provider keys", Requirement: readiness.RequirementCore, Verdict: readiness.VerdictUnverifiable,
			Detail: "could not read hostmode.env (" + err.Error() + ")", Evidence: "hostmode.env unreadable: " + err.Error()}}
	case len(names) == 0:
		return []readiness.Check{{Label: "provider keys", Requirement: readiness.RequirementCore, Verdict: readiness.VerdictTodo,
			Detail: "no provider key configured", Evidence: "hostmode.env lists no provider key",
			Todo: "pix models add anthropic"}}
	default:
		return []readiness.Check{{Label: "provider keys", Requirement: readiness.RequirementCore, Verdict: readiness.VerdictReady,
			Detail: strings.Join(names, ", "), Evidence: "hostmode.env lists " + strings.Join(names, ", ")}}
	}
}

// SetupSelectRunnableIntent prevents a successful one-provider setup from
// immediately selecting a model whose provider has no key. It changes only
// the shipped OpenAI-specific default. Explicit non-default user choices and
// multi-provider installations are left untouched.
func SetupSelectRunnableIntent(cfg *config.Config, env hostenv.Env) bool {
	if cfg == nil || cfg.RunIntent != config.DefaultRunIntent {
		return false
	}
	names, err := secret.HostModeProviderKeys(env)
	if err != nil || len(names) != 1 {
		return false
	}
	switch names[0] {
	case "anthropic":
		cfg.RunIntent = "strategy"
		return true
	case "google":
		cfg.RunIntent = "review"
		return true
	default:
		return false
	}
}

// setupPackAxis is the post-mutation pack fact: an ACTIVE but EMPTY pack is a
// TODO, never green.
func setupPackAxis(cfg *config.Config) []readiness.Check {
	p := launch.ResolveHostStatePack(cfg, "")
	switch {
	case p.Active && p.Exists && (p.Skills || p.Knowledge):
		return []readiness.Check{{Label: "pack", Requirement: readiness.RequirementCore, Verdict: readiness.VerdictReady,
			Detail: p.Path + " (active)", Evidence: "active pack " + p.Path + " has content"}}
	case p.Active && p.Exists:
		return []readiness.Check{{Label: "pack", Requirement: readiness.RequirementCore, Verdict: readiness.VerdictTodo,
			Detail: "active but empty (" + p.Path + ")", Evidence: "active pack " + p.Path + " has no skills or knowledge",
			Todo: "pix pack add skill <name>"}}
	default:
		return []readiness.Check{{Label: "pack", Requirement: readiness.RequirementCore, Verdict: readiness.VerdictTodo,
			Detail: "no active pack", Evidence: "no pack is active", Todo: "pix pack new"}}
	}
}

// setupGworkspaceAxis is the post-mutation Google Workspace fact, probed the
// same way `pix gworkspace status` probes it.
func setupGworkspaceAxis(cfg *config.Config, env hostenv.Env) []readiness.Check {
	acct := strings.TrimSpace(cfg.GogAccount)
	switch {
	case acct == "":
		return []readiness.Check{{Label: "google ws", Requirement: readiness.RequirementOptional, Verdict: readiness.VerdictTodo,
			Detail: "enabled but no account authorized", Evidence: "google_workspace_account is empty",
			Todo: "pix gworkspace setup"}}
	case gworkspace.GogSetupAccountHealthy(env, acct):
		return []readiness.Check{{Label: "google ws", Requirement: readiness.RequirementOptional, Verdict: readiness.VerdictReady,
			Detail: acct + " authorized (read-only)", Evidence: "authorization probe passed for " + acct}}
	default:
		return []readiness.Check{{Label: "google ws", Requirement: readiness.RequirementOptional, Verdict: readiness.VerdictTodo,
			Detail: acct + " not verified", Evidence: "authorization probe failed for " + acct,
			Todo: "pix gworkspace setup"}}
	}
}

// providerKeyPromptAttempts caps how many times SetupProvisionKeys reprompts
// for a single provider's ref before giving up, since a human who keeps
// mistyping (or an unattended TTY feeding garbage) must not hang setup
// forever.
const providerKeyPromptAttempts = 3

// SetupProvisionKeys sources one or more model provider keys from 1Password
// and reconciles them into the sbx secret store. This is the ONLY
// provider-key path: op is required, and the removed --use-sbx-keys flag,
// persisted provider_key_mode, and the "already in sbx?" convenience prompt are
// gone. Returns whether all keys ended up usable.
//
// Step 0 (hard preconditions): `op` must be installed AND signed in. Without
// either there is nothing to source keys from — fail setup with the exact fix,
// before pack/host/onboarding ever run.
//
// Step 1 (collect + validate configured refs): existing provider refs are all
// validated. When none exists, an interactive setup asks which ONE provider to
// configure and then collects that provider's ref. One provider is enough to
// run Pix; additional providers can be added later with `pix secret set`.
//   - a ref already configured (op-refs.env OR hostmode.env, via secret.CurrentOpRef)
//     is CONFIRMED, not re-solicited — but it still must resolve via `op read`
//     to a non-empty value; a broken existing ref fails setup outright.
//   - a ref with NO configuration yet, on an interactive TTY, is prompted for
//     one at a time. Empty input or EOF is NOT "skip" — a key is mandatory, so
//     that fails setup. An invalid/unresolvable ref reprompts (capped at
//     providerKeyPromptAttempts).
//   - a ref with NO configuration and NO interactive TTY prints the exact
//     `pix secret set` command per missing provider and fails setup.
//
// Step 2 (mirror + verify): every validated ref is written to BOTH op-refs.env
// (sandbox) and hostmode.env (host mode); setup then verifies they all landed
// in hostmode.env.
//
// Step 3 (reconcile sbx): secret.ReconcileProviderKeysWithSbx brings sbx to the same
// state as the validated refs, fed the Step-1 snapshot. A reconcile failure
// fails setup. Step 4 (final probe) requires every configured key usable in sbx.
//
// Steps 1-3 run holding the provider-refs transaction lock so a concurrent
// `pix secret set`/`secret rm` cannot interleave. Never persists or prints
// a resolved secret value.
//
// SetupProvisionKeysFn is the seam SetupHostPhase calls through (a package var
// so a test can replace it with a stub).
var SetupProvisionKeysFn = SetupProvisionKeys

// SetupProvisionKeys resolves provider keys from 1Password (the strict, and now
// only, flow) and returns whether it succeeded.
func SetupProvisionKeys(env hostenv.Env, in io.Reader, out io.Writer, interactive, assumeYes bool) bool {
	return runStrictProviderKeyFlow(env, bufio.NewScanner(in), out, interactive, assumeYes)
}

// runStrictProviderKeyFlow resolves the configured provider keys from 1Password and
// reconciles it into sbx (Steps 0-4 documented on SetupProvisionKeys). It is the
// only provider-key path now that the sbx-keys shortcut is gone.
func runStrictProviderKeyFlow(env hostenv.Env, sc *bufio.Scanner, out io.Writer, interactive, assumeYes bool) bool {
	fmt.Fprintln(out, "")

	if !secret.OpInstalled(env) {
		fmt.Fprintln(out, "1Password provider setup requires the `op` CLI, but it isn't installed.")
		fmt.Fprintln(out, "  fix: brew install 1password-cli   (or https://developer.1password.com/docs/cli/)")
		fmt.Fprintln(out, "then re-run the same setup command.")
		return false
	}
	if !secret.OpSignedIn(env) {
		if interactive {
			fmt.Fprintln(out, "1Password needs authorization. Continuing with the official `op signin` flow.")
			if err := env.RunInteractive("op", "signin"); err == nil && secret.OpSignedIn(env) {
				// Continue directly into provider selection. No separate user command
				// or Pix identity is introduced.
			} else {
				fmt.Fprintln(out, "`op signin` did not establish a usable 1Password session.")
				fmt.Fprintln(out, "  fix: op signin   (or add an account in the 1Password app)")
				fmt.Fprintln(out, "then re-run the same setup command.")
				return false
			}
		} else {
			fmt.Fprintln(out, "`op` is installed but no 1Password account is configured.")
			fmt.Fprintln(out, "  fix: op signin   (or add an account in the 1Password app)")
			fmt.Fprintln(out, "then re-run the same setup command.")
			return false
		}
	}

	// Hold the provider-refs transaction lock across the WHOLE flow: initial
	// ref reads/validation, the canonical both-file writes, the hostmode
	// verification, and the sbx reconciliation + synced-ref metadata. A lock
	// acquisition failure fails setup honestly — never proceed unlocked.
	ok := false
	if lerr := secret.WithProviderRefsLock(env, func() error {
		ok = strictProviderKeyFlowLocked(env, sc, out, interactive, assumeYes)
		return nil
	}); lerr != nil {
		fmt.Fprintf(out, "  \u2717 could not lock provider refs (%s): %v — another pix credential operation may hold it; fix that and re-run the same setup command.\n", secret.ProviderRefsLockPath(env), lerr)
		return false
	}
	return ok
}

// strictProviderKeyFlowLocked is runStrictProviderKeyFlow's transaction body
// (Steps 1-4). Caller MUST hold the provider-refs lock; every refs-file write
// in here goes through a *Locked variant for exactly that reason.
func strictProviderKeyFlowLocked(env hostenv.Env, sc *bufio.Scanner, out io.Writer, interactive, assumeYes bool) bool {
	// refs is the validated snapshot reconcile (STEP 3) works from (envVar ->
	// op:// ref — every entry validated AND canonical-written to both files
	// below); resolved caches each provider's validated op-read value so
	// reconcile never pays for a second `op read` of the same ref.
	refs := make(map[string]string, len(secret.ProviderKeyRefOrder))
	resolved := make(map[string]string, len(secret.ProviderKeyRefOrder))
	configured := make([]secret.ProviderKeyRef, 0, len(secret.ProviderKeyRefOrder))
	for _, p := range secret.ProviderKeyRefOrder {
		if _, ok := secret.CurrentOpRef(env, p.EnvVar); ok {
			configured = append(configured, p)
		}
	}
	if len(configured) == 0 {
		if !interactive {
			fmt.Fprintln(out, "No model provider is configured. Add any ONE provider:")
			for _, p := range secret.ProviderKeyRefOrder {
				// No terminal here, so `pix models add` cannot collect a ref: it would
				// exit 2 pointing back at this command. Lead with the scripted form.
				fmt.Fprintf(out, "  pix secret set %s op://Vault/Item/field  # %s\n", p.EnvVar, p.Name)
			}
			fmt.Fprintln(out, "then re-run: pix setup   (or, on a terminal: pix models add <provider>)")
			return false
		}
		chosen, ok := promptProviderChoice(sc, out)
		if !ok {
			return false
		}
		configured = append(configured, chosen)
	}

	for _, p := range configured {
		ref, hasRef := secret.CurrentOpRef(env, p.EnvVar)
		switch {
		case hasRef:
			val, ok := secret.OpReadNonEmpty(env, ref)
			if !ok {
				fmt.Fprintf(out, "  %s \u2717 configured 1Password ref does not resolve (op read failed or empty)\n", p.Name)
				fmt.Fprintf(out, "    fix it: pix secret set %s op://Vault/Item/field\n", p.EnvVar)
				return false
			}
			// secret.CurrentOpRef may have found this ref in EITHER file (op-refs.env OR
			// hostmode.env), not necessarily both. Idempotently upsert it into BOTH
			// here — a no-op where it already matches — and FAIL setup if either
			// write errors, rather than silently backfilling one file and calling
			// that success (the bug: a ref found only in hostmode.env must not be
			// allowed to leave op-refs.env permanently missing it).
			if err := secret.WriteOpRefQuietLocked(env, p.EnvVar, ref); err != nil {
				fmt.Fprintf(out, "  %s \u2717 could not write ref to op-refs.env: %v\n", p.Name, err)
				return false
			}
			if err := secret.WriteOpRefFileQuietLocked(env, secret.HostModeRefsPath(env), p.EnvVar, ref); err != nil {
				fmt.Fprintf(out, "  %s \u2717 could not write ref to hostmode.env: %v\n", p.Name, err)
				return false
			}
			// No ✓ here on purpose (AC-P0-302): the keys step runs in the
			// mutate phase, which prints no success claims. The keys row in
			// setup's report is rendered from a post-mutation read of
			// hostmode.env, so a run whose key writes fail cannot have
			// printed a green line for them earlier.
			refs[p.EnvVar] = ref
			resolved[p.EnvVar] = val
		case interactive:
			ref, val, ok := promptProviderRef(env, sc, out, p)
			if !ok {
				return false
			}
			if err := secret.WriteOpRefQuietLocked(env, p.EnvVar, ref); err != nil {
				fmt.Fprintf(out, "  %s \u2717 could not save ref: %v\n", p.Name, err)
				return false
			}
			if err := secret.WriteOpRefFileQuietLocked(env, secret.HostModeRefsPath(env), p.EnvVar, ref); err != nil {
				fmt.Fprintf(out, "  %s \u2717 could not save host-mode ref: %v\n", p.Name, err)
				return false
			}
			refs[p.EnvVar] = ref
			resolved[p.EnvVar] = val
		default:
			// Only reachable for the one provider selected above.
			return false
		}
	}

	// Every validated ref was already canonical-written to BOTH files above, so
	// there is no reread-and-remirror pass here — just the final membership
	// verification that every configured provider landed in hostmode.env (host mode reads ONLY
	// hostmode.env via `op run --env-file`, never op-refs.env).
	got, kerr := secret.HostModeProviderKeys(env)
	if kerr != nil {
		fmt.Fprintf(out, "  \u2717 credential state unreadable: %v\n", kerr)
		return false
	}
	gotSet := map[string]bool{}
	for _, name := range got {
		gotSet[name] = true
	}
	for _, p := range configured {
		if !gotSet[p.Name] {
			fmt.Fprintf(out, "  \u2717 hostmode.env is missing configured provider %s after mirroring\n", p.Name)
			return false
		}
	}

	if !secret.ReconcileProviderKeysWithSbx(env, sc, out, interactive, assumeYes, refs, resolved) {
		return false
	}

	// Tri-state: only abort setup when we can POSITIVELY confirm sbx is missing
	// a key (secret.SbxSecretsOK). sbx being entirely ABSENT is portability — fail
	// open, we can't tell. sbx being installed but the check command FAILING is
	// a real, diagnosable problem — fail CLOSED with a message, never silently
	// pass a box whose completeness we couldn't actually verify.
	sbxOut, state := secret.ProbeSbxSecrets(env)
	switch state {
	case secret.SbxSecretsAbsent:
		return true
	case secret.SbxSecretsError:
		fmt.Fprintln(out, "  \u2717 could not verify sbx has the configured provider keys (`sbx secret ls` failed) \u2014 check sbx and re-run the same setup command")
		return false
	}
	for _, p := range secret.ProviderKeyRefOrder {
		if _, configured := refs[p.EnvVar]; configured && !cli.GrepWord(sbxOut, p.Name) {
			fmt.Fprintf(out, "  \u2717 sbx is missing configured provider %s after reconciliation\n", p.Name)
			return false
		}
	}
	return true
}

// promptProviderChoice keeps first-run setup to one decision and one ref. It
// accepts either the displayed number or provider name and defaults to OpenAI,
// matching Pix's default overlord route.
func promptProviderChoice(sc *bufio.Scanner, out io.Writer) (secret.ProviderKeyRef, bool) {
	empty := secret.ProviderKeyRef{}
	fmt.Fprintln(out, "One model provider is enough to start.")
	// Name the literal command. "You can add others later" was true and useless:
	// the only later path was `pix secret set`, which stores a ref and stops, so
	// the second key stayed inert and there was nothing to search for.
	fmt.Fprintln(out, "Add the others whenever you like with: pix models add <provider>")
	fmt.Fprintln(out, "  1. openai (default)")
	fmt.Fprintln(out, "  2. anthropic")
	fmt.Fprintln(out, "  3. google")
	fmt.Fprint(out, "Choose a provider [1]: ")
	if !sc.Scan() {
		fmt.Fprintln(out, "\n  no input; setup cannot continue")
		return empty, false
	}
	choice := strings.ToLower(strings.TrimSpace(sc.Text()))
	var envVar string
	switch choice {
	case "", "1", "openai":
		envVar = "OPENAI_API_KEY"
	case "2", "anthropic":
		envVar = "ANTHROPIC_API_KEY"
	case "3", "google", "gemini":
		envVar = "GEMINI_API_KEY"
	default:
		fmt.Fprintf(out, "  unknown provider %q; choose 1, 2, or 3 and re-run setup\n", choice)
		return empty, false
	}
	for _, p := range secret.ProviderKeyRefOrder {
		if p.EnvVar == envVar {
			return p, true
		}
	}
	return empty, false
}

// promptProviderRef prompts (once at a time, on a real TTY) for a NEW op://
// ref for a provider with none configured yet. It validates the ref resolves
// via `op read` to a non-empty value BEFORE returning it, and never echoes
// the resolved value. Empty input or EOF is a hard failure (a key is
// mandatory, not optional to skip); an invalid or unresolvable ref explains
// why and reprompts, up to providerKeyPromptAttempts, then fails.
func promptProviderRef(env hostenv.Env, sc *bufio.Scanner, out io.Writer, p secret.ProviderKeyRef) (ref, value string, ok bool) {
	for attempt := 1; attempt <= providerKeyPromptAttempts; attempt++ {
		fmt.Fprintf(out, "  %s: paste a 1Password ref (op://Vault/Item/field): ", p.Name)
		if !sc.Scan() {
			fmt.Fprintln(out, "")
			fmt.Fprintf(out, "  %s: no input — a 1Password ref is required; setup cannot continue.\n", p.Name)
			return "", "", false
		}
		ref = secret.NormalizeOpRef(sc.Text())
		if ref == "" {
			fmt.Fprintf(out, "    a ref is required for %s (it is not optional) — try again.\n", p.Name)
			continue
		}
		if !validOpRefSyntax(ref) {
			fmt.Fprintln(out, "    not a valid op:// ref (want op://Vault/Item/field) — try again.")
			continue
		}
		val, resolves := secret.OpReadNonEmpty(env, ref)
		if !resolves {
			fmt.Fprintf(out, "    could not resolve that ref for %s via `op read` (check the vault/item/field) — try again.\n", p.Name)
			continue
		}
		return ref, val, true
	}
	fmt.Fprintf(out, "  %s: too many invalid attempts — aborting setup.\n", p.Name)
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
	if secret.HasPlaceholder(ref) {
		return false
	}
	for _, r := range ref {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// IdentityMemory is the slice of the memory client SeedIdentity needs,
// injectable via NewIdentityMemory so tests can simulate per-call RPC
// failures without a live daemon.
type IdentityMemory interface {
	Up() bool
	Call(method string, params map[string]any) (map[string]any, error)
}

// NewIdentityMemory is SeedIdentity's seam to the memory daemon.
var NewIdentityMemory = func() IdentityMemory { return rpc.MemoryClient() }

// rememberPersistedID extracts the "id" field from a remember RPC result,
// returning "" for anything that does not prove a durable write actually
// happened: a nil/empty result map, a missing "id" key, an "id" of the wrong
// type, or the daemon's own no-error-but-nothing-persisted response for empty
// content ({"id": "", "reaffirmed": false} — memory.go's remember handler).
// SeedIdentity must count a save only when this returns non-empty; err == nil
// alone is not proof anything landed.
func rememberPersistedID(res map[string]any) string {
	id, _ := res["id"].(string)
	return id
}

// SeedIdentity greets the user by name (from git config) and stores durable
// identity facts in memory (best-effort, only if the daemon is up), so their
// first session isn't anonymous. The trusted host-state payload injected into
// the onboarding kickoff prompt (see hoststate.go's launch.InjectTrustedHostState)
// also carries identity, built fresh at every launch, so "available to
// sessions via host state" is true regardless of the memory outcome. Each
// remember RPC is tracked individually: the output claims a memory save ONLY
// for writes that actually succeeded, is honest about a partial or failed
// batch, and never promises recall it can't guarantee.
func SeedIdentity(env hostenv.Env, out io.Writer) {
	id := launch.ReadGitIdentity(env)
	if id.Name == "" {
		return
	}
	who := id.Name
	// Store ONLY the first name (launch.ReadGitIdentity already reduces to it). No
	// surname, no email: this fact is recalled into every session's context, so
	// it carries the minimum needed to greet, not a pile of PII. The warm
	// greeting itself belongs to the in-session agent, not this log.
	facts := []string{"The user's first name is " + id.Name + "."}
	attempted, saved := 0, 0
	if c := NewIdentityMemory(); c.Up() {
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

// SetupSandboxName derives the sandbox name `pix run` would use for dir
// (base name + active-profile suffix), so setup's guard can probe the SAME
// sandbox run would attach to. ok=false when the name can't be resolved (e.g. a
// unresolvable config) — the caller then skips the guard rather than blocking
// setup.
func SetupSandboxName(dir string) (string, bool) {
	if _, err := config.Load(); err != nil {
		return "", false
	}
	return workspace.DeriveSandboxName(dir), true
}

// FlagTakesValue reports whether an onboard flag consumes a following token
// (only the space-separated form; `--flag=value` is self-contained).
func FlagTakesValue(a string) bool {
	switch a {
	case "--account", "--credentials", "--knowledge", "--mcp", "--model", "--models", "--pack", "--with":
		return true
	}
	return false
}

func NormalizeSetupPackArg(arg string) string {
	arg = strings.TrimSpace(arg)
	if strings.Count(arg, "/") == 1 && !strings.Contains(arg, ":") && !strings.HasPrefix(arg, ".") && !strings.HasPrefix(arg, "~") {
		return "https://github.com/" + arg + ".git"
	}
	return arg
}

const Usage = `usage: pix setup [DIR] [host-config flags]

Sets up callable inference, then starts Pix. Ordinary setup begins with one
model-runtime choice. The selected path may then ask only for the credentials
or consent it actually needs. API keys are the default; a healthy existing
Ollama is offered; a custom gateway can use the current sbx login or no auth.
1Password is required only for the API-key path. Ollama is never installed
automatically.

Memory is progressive enhancement: it is enabled only when Ollama is healthy
and its watcher and embedding models are verified. Without Ollama, Pix still
runs normally with memory off. Host mode is separate and opt-in via
'pix host setup'.

DIR defaults to the current directory (like ` + "`pix run`" + `). Repeat semantics:
the host phase ALWAYS reconciles again, even when a sandbox
already exists for DIR. If one exists and you did not pass --replace, setup
leaves it alone (never force-removes it, never replays the tour into a live
session) and prints your choices: 'pix run [DIR]' to reattach, or
'pix setup [DIR] --replace' to recreate it with your current settings and
get the tour. Only a POSITIVELY absent sandbox gets the first-launch handoff;
if the sandbox state cannot be determined at all (sbx errored), setup fails
closed after the host phase — fix sbx and re-run.

Setup flags:
  --no-agent               run the HOST phase only: no sandbox, no handoff.
                           This is the scripted/CI path (it replaces the
                           removed ` + "`pix onboard`" + ` verb); --yes and
                           --non-interactive stay orthogonal to it
  --apply                  apply a pending .pix/onboarding.json in DIR
                           (the control-plane proposal an in-sandbox onboarding
                           agent wrote), under a confirmation gate
  --replace                recreate an existing sandbox for DIR (sbx rm -f +
                           create) so it picks up current pack/MCP/skills and
                           receives the guided tour; harmless when absent
  --verbose                show underlying sbx, Git, Docker, and setup command
                           output; ordinary setup prints only actions/results
  --pull-models            pull any CONFIRMED-missing configured local Ollama
                           models (watcher/embed/bridge, deduplicated); the
                           ONLY download consent a non-interactive setup honors
                           (a broad --yes never downloads). Interactive setup
                           may offer a default-No pull when an existing Ollama
                           positively lacks required memory models. Setup never installs
                           Ollama itself, and never pulls a tag it could not
                           positively verify as missing.
                           pix setup --pull-models with Ollama down exits
                           1. pix setup with the same Ollama down exits 0
                           with an optional ⚠ row. Stale optional config never
                           blocks unrelated repair.

Host-config flags (all optional):
  --pack <path|git+https-url#ref=branch|tag|sha>
                           activate a pack through the normal host trust gate,
                           then run its required, resumable setup hooks;
                           repeatable, composed in command order (collections
                           union; later scalar declarations win)
  --with <setup-id>        also run a named optional setup hook from --pack;
                           repeatable, and invalid without --pack
  --google-workspace       opt in to Google Workspace (absent otherwise): runs
                           the same transaction as 'pix gworkspace setup'
                           (may open a browser). Requires --account, and
                           --credentials unless the client was already imported
  --account <email>        the Google Workspace account to authorize; valid
                           ONLY with --google-workspace
  --credentials <path>     your Desktop OAuth client JSON; valid ONLY with
                           --google-workspace
  --knowledge <path|url>   scaffold/point the global knowledge base
  --mcp <name>             enable an MCP server (repeatable; allowlisted)
  --model <ollama-model>   set the ollama-bridge model
  --models <id,id,...>     restrict agents to these canonical catalog models;
                           interactive first setup otherwise offers every
                           model available through the selected runtime(s)
  --yes | --non-interactive  never prompt (CI); callable inference must already
                           be configured through provider refs, a pack/session
                           gateway, a no-auth gateway, or verified Ollama
  -h | --help              this help

Ordinary setup prints a short action-oriented transcript. --verbose exposes the
underlying eight phases — parse, inventory, gate, mutate, consent, verify,
report, handoff — and the commands they run for diagnosis. Setup sequences
prompts one at a time and never prompts at all without a TTY.
Mutations run in a fixed order with the
riskiest last (keys, config, pack, MCP, knowledge, identity, Google Workspace,
model pulls) and each one is individually idempotent, so an interrupted run is
resumed by re-running the same command: setup re-probes what is actually there
rather than reading back a journal of what it once intended.
`

// CheckGoogleWorkspaceFlags enforces AC-P0-312: --account and --credentials are
// Google Workspace inputs, valid ONLY alongside --google-workspace. The error
// is deliberately in the standard grammar (invoked path, lowercase, no trailing
// period) and maps to exit 2 at the call site, because it is an argument
// mistake, not a failed probe.
func CheckGoogleWorkspaceFlags(opts onboard.Opts) error {
	if opts.GoogleWorkspace {
		return nil
	}
	if strings.TrimSpace(opts.Account) != "" {
		return ErrUsage{fmt.Errorf("--account requires --google-workspace")}
	}
	if strings.TrimSpace(opts.Credentials) != "" {
		return ErrUsage{fmt.Errorf("--credentials requires --google-workspace")}
	}
	return nil
}

// ErrUsage marks an argument error, which exits 2 rather than 1.
type ErrUsage struct{ error }

// setupGoogleWorkspaceFn is the seam tests stub so setup's phases can be
// exercised without a browser or an installed dependency CLI. Production wires
// the real façade over the unchanged transaction.
var setupGoogleWorkspaceFn = func(env hostenv.Env, opts gworkspace.GogSetupOpts, in io.Reader, out io.Writer, interactive bool) error {
	return gworkspace.Setup(env, opts, in, out, interactive, Credentials)
}

// SetupChooseInference owns the single ordinary-user inference question. It
// is skipped when a pack or prior setup already supplied a backend. Ollama is
// shown only after both binary and daemon probes succeed.
func SetupChooseInference(cfg *config.Config, env hostenv.Env, in io.Reader, out io.Writer, interactive bool) (bool, error) {
	if cfg == nil {
		return false, nil
	}
	if len(cfg.Inference.Backends) > 0 {
		if inference.InferenceNeedsOnePassword(cfg) {
			return false, nil
		}
		if err := EnableDeclaredInferenceBindings(cfg); err != nil {
			return false, err
		}
		return true, nil
	}
	if !interactive {
		return false, nil
	}
	ollamaReady := false
	if _, err := env.LookPath("ollama"); err == nil {
		_, timedOut, runErr := env.RunTimed("ollama", "list")
		ollamaReady = runErr == nil && !timedOut
	}
	fmt.Fprintln(out, "How should Pix run models? (choose one or more, comma-separated)")
	fmt.Fprintln(out, "  1. API key (default)     Anthropic / OpenAI / Google keys, resolved from 1Password")
	if ollamaReady {
		fmt.Fprintln(out, "  2. Ollama local          models that run on this machine")
	}
	fmt.Fprintln(out, "  3. Custom gateway        an OpenAI-compatible endpoint you host")
	if ollamaReady {
		fmt.Fprintln(out, "  4. Ollama Cloud          large models on your ollama.com subscription")
		// A HINT, not entitlement. A `:cloud` row in the listing appears on every
		// signed-in machine and proves nothing about what the plan may call — that
		// inference is exactly how a gated model got bound and 401'd at call time.
		if n := listedCloudTagCount(env); n > 0 {
			fmt.Fprintf(out, "  (this machine lists %d cloud model(s); Pix proves which ones your plan can call)\n", n)
		}
	}
	fmt.Fprint(out, "Choose [1]: ")
	choice, ok := readSetupLine(in)
	if !ok {
		return false, fmt.Errorf("no inference choice; setup cannot continue")
	}
	if strings.TrimSpace(choice) == "" {
		return false, nil
	}
	selected := map[string]bool{}
	for _, raw := range strings.FieldsFunc(strings.ToLower(choice), func(r rune) bool { return r == ',' || r == ' ' }) {
		switch raw {
		case "1", "api":
			selected["api"] = true
		case "2", "ollama", "ollama-local", "local":
			selected["ollama"] = true
		case "3", "gateway":
			selected["gateway"] = true
		case "4", "ollama-cloud", "cloud":
			selected["ollama-cloud"] = true
		default:
			return false, fmt.Errorf("unknown inference choice %q", raw)
		}
	}
	if selected["ollama"] || selected["ollama-cloud"] {
		if !ollamaReady {
			return false, fmt.Errorf("Ollama is not installed and healthy, so it is not an available inference choice")
		}
		sel := OllamaSelection{Local: selected["ollama"], Cloud: selected["ollama-cloud"]}
		if _, err := ConfigureOllamaInference(cfg, env, sel, out); err != nil {
			return false, err
		}
	}
	if selected["gateway"] {
		if _, err := configureCustomGateway(cfg, in, out); err != nil {
			return false, err
		}
	}
	// false means the caller must continue through the direct-key transaction.
	return !selected["api"], nil
}

// VerifyOllamaInference earns Verified for ollama bindings with an actual
// model-specific request through the RESOLVED endpoint. Every binding is
// checked independently. CLOUD probes run concurrently (they are network round
// trips and hold no local resource). LOCAL probes are SERIALIZED and unload
// after themselves: two concurrent generates make Ollama co-load two sets of
// weights, which either exhausts the memory budget readiness_hardware.go just
// computed or serializes the loads anyway behind timers that started at
// dispatch — so the second reports a timeout it never got a turn to spend, and
// un-binds a model that works. Mirrors VerifyDirectInference in structure, not
// in concurrency.
//
// The local set runs LARGEST RUNG FIRST under its own wall budget, and a probe
// is never STARTED unless its full timeout fits what remains: the budget can
// therefore never manufacture a timeout. A candidate the budget never reached
// is `not probed` — a THIRD state that is neither verified nor failed, excluded
// from attempted, and never rendered as a rejection.
//
// Deviation from the design's signature: it takes an io.Writer (the design's
// own output spec prints a live line per local probe, which cannot be done
// without one) and returns the notProbed set (the third state has to be
// observable to be assertable).
func VerifyOllamaInference(cfg *config.Config, env hostenv.Env, out io.Writer) (res probeOutcome, err error) {
	if cfg == nil {
		return res, fmt.Errorf("verify ollama inference: no config")
	}
	if env.OllamaInference == nil {
		return res, ErrNoProbeSeam
	}
	if out == nil {
		out = io.Discard
	}
	reg, err := routing.LoadRegistry()
	if err != nil {
		return res, fmt.Errorf("verify ollama inference: %w", err)
	}
	endpoint := strings.TrimRight(axis.EffectiveOllamaEndpoint(cfg, env).URL, "/")
	type candidate struct {
		index  int
		label  string
		Tag    string
		numCtx int
		minRAM float64
	}
	var local, cloud []candidate
	for i := range cfg.Inference.Models {
		binding := &cfg.Inference.Models[i]
		backend, ok := cfg.Inference.Backends[binding.Backend]
		if !ok || backend.Driver != "ollama" || !binding.Available || !inference.Allowed(cfg, *binding) {
			continue
		}
		if binding.Source != "" {
			// A pack's authority is the sandbox smoke test; a host probe must not
			// demote what it cannot faithfully replay.
			continue
		}
		// Demote first: a stale claim (including a pre-provenance listing-derived
		// one) must never survive a run that could not re-earn it.
		binding.Verified, binding.VerifiedBy, binding.VerifiedAt = false, "", ""
		c := candidate{index: i, label: binding.Model, Tag: binding.Upstream}
		m, found := reg.Get(binding.Model)
		if found && m.Local {
			// num_ctx is the rung's DECLARED context budget, so the probe allocates
			// the same KV cache the RAM gate priced. A rung that cannot hold its own
			// declared context fails here, which is exactly when we want to find out.
			c.numCtx, c.minRAM = m.ContextWindow, m.MinRAMGB
			local = append(local, c)
			continue
		}
		cloud = append(cloud, c)
	}

	promote := func(index int) {
		cfg.Inference.Models[index].Verified = true
		cfg.Inference.Models[index].VerifiedBy = config.VerifiedByProbe
		cfg.Inference.Models[index].VerifiedAt = time.Now().UTC().Format(time.RFC3339)
		res.Verified++
	}

	// Cloud: concurrent. Nothing local is held, so N probes cost one timeout.
	type result struct {
		index int
		label string
		err   error
	}
	results := make(chan result, len(cloud))
	for _, c := range cloud {
		res.Attempted++
		go func(c candidate) {
			results <- result{index: c.index, label: c.label, err: env.OllamaInference(endpoint, c.Tag, 0, ollamaCloudProbeTimeout)}
		}(c)
	}

	// Local: strictly serial, largest rung first, each unloading after itself.
	sort.Slice(local, func(i, j int) bool {
		if local[i].minRAM != local[j].minRAM {
			return local[i].minRAM > local[j].minRAM
		}
		return local[i].label < local[j].label
	})
	if len(local) > 0 {
		fmt.Fprintf(out, "  verifying %d local ollama model(s), one at a time (each is loaded and unloaded) ...\n", len(local))
	}
	remaining := OllamaLocalProbeBudget
	for _, c := range local {
		if remaining < OllamaLocalProbeTimeout {
			// NOT a failure: this candidate never got a turn. Reporting it as broken
			// would let a budget un-bind a healthy model.
			res.NotProbed = append(res.NotProbed, c.label)
			fmt.Fprintf(out, "    %-14s not probed — %.0fs left of the %.0fs local budget, less than one probe's %.0fs\n",
				c.Tag, remaining.Seconds(), OllamaLocalProbeBudget.Seconds(), OllamaLocalProbeTimeout.Seconds())
			continue
		}
		res.Attempted++
		start := time.Now()
		err := env.OllamaInference(endpoint, c.Tag, c.numCtx, OllamaLocalProbeTimeout)
		elapsed := time.Since(start)
		if remaining -= elapsed; remaining < 0 {
			remaining = 0
		}
		if err != nil {
			res.Failures = append(res.Failures, c.label+": "+err.Error())
			fmt.Fprintf(out, "    %-14s failed (%.0fs): %v\n", c.Tag, elapsed.Seconds(), err)
			continue
		}
		promote(c.index)
		fmt.Fprintf(out, "    %-14s ok (%.0fs)\n", c.Tag, elapsed.Seconds())
	}

	for range cloud {
		r := <-results
		if r.err != nil {
			res.Failures = append(res.Failures, r.label+": "+r.err.Error())
			continue
		}
		promote(r.index)
	}
	sort.Strings(res.Failures)
	sort.Strings(res.NotProbed)
	return res, nil
}

// ConfigureModelRoster turns the broad set of backend bindings into the small,
// explicit catalog-model surface agents may use. The router continues to pick
// by intent, but it can never escape this roster. A mandatory pack is already
// an explicit policy decision and therefore skips the personal roster prompt.
func ConfigureModelRoster(cfg *config.Config, in io.Reader, out io.Writer, interactive bool, requested string) error {
	return configureModelRosterFrom(cfg, in, out, interactive, requested, nil)
}

// ReconcileDirectInference turns the provider keys that exist on this host into
// callable model bindings. It is the sequence that used to live only inside
// setup's keys step, which is why adding a key any other way left it inert:
// `pix secret set` wrote the ref, and nothing ever rebuilt the bindings, probed
// them, or widened the roster.
//
// Order is load-bearing and matches the step it was extracted from:
//
//		capture prior providers -> bind -> verify -> save -> judge -> roster
//
//	  - The prior set is captured BEFORE binding, or widening cannot see what is
//	    new (see rosterSeenProviders).
//	  - cfg.Save() happens BEFORE the verified==0 verdict, so a partial success is
//	    never thrown away by the error path.
//	  - verified == 0 with something attempted is a hard error; verified > 0 with
//	    some failures is not.
//
// requestedProvider is the provider the USER named on the command line
// (`pix models add google`), or "" for setup's own reconcile. It is the only
// thing that can override the roster's already-offered stamp — see
// WidenRosterForProvider.
func ReconcileDirectInference(cfg *config.Config, env hostenv.Env, in io.Reader, out io.Writer, interactive bool, requestedModels, requestedProvider string) (reconcileResult, error) {
	var res reconcileResult
	if cfg == nil {
		return res, fmt.Errorf("no config")
	}
	if cfg.Inference.ExclusiveSource != "" {
		return res, ErrInferenceExclusive
	}
	if out == nil {
		out = io.Discard
	}
	prior := inference.BoundNativeProviders(cfg)

	providers, err := secret.HostModeProviderKeys(env)
	if err != nil {
		return res, fmt.Errorf("reading configured providers: %w", err)
	}
	res.Providers = providers
	for _, p := range providers {
		if !prior[p] {
			res.Added = append(res.Added, p)
		}
	}
	sort.Strings(res.Added)

	if err := ConfigureDirectInference(cfg, providers); err != nil {
		return res, fmt.Errorf("configuring direct inference: %w", err)
	}
	// Widen BEFORE probing. VerifyDirectInference only probes bindings the roster
	// allows (inferenceBindingAllowed), so on a config with a non-empty roster
	// the newly added provider would otherwise never be probed, never become
	// callable, and be pruned straight back out of the roster for not being
	// callable — the key stays inert and the command still reports success.
	widenRosterForNewProviders(cfg, prior)
	WidenRosterForProvider(cfg, requestedProvider)
	outcome, verr := VerifyDirectInference(cfg, env)
	if verr != nil {
		return res, fmt.Errorf("verifying provider keys: %w", verr)
	}
	res.probeOutcome = outcome
	if err := cfg.Save(); err != nil {
		return res, err
	}
	if res.Verified == 0 && (res.Attempted > 0 || len(res.Failures) > 0) {
		detail := strings.Join(res.Failures, "; ")
		if detail == "" {
			detail = "no provider accepted a model-specific request"
		}
		return res, fmt.Errorf("provider keys resolved, but live inference verification failed: %s", detail)
	}
	if callable, _ := axis.ConfiguredInferenceSummary(cfg); callable > 0 || strings.TrimSpace(requestedModels) != "" {
		if err := configureModelRosterFrom(cfg, in, out, interactive, requestedModels, prior); err != nil {
			return res, fmt.Errorf("choosing models: %w", err)
		}
	}
	return res, cfg.Save()
}
