package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"pix/host/cli"
	"pix/host/hostenv"
	"pix/host/mcp"
	"pix/host/monitor/tui"
	"pix/host/readiness"
	"pix/host/readiness/axis"
	"pix/host/secret"
	"pix/host/sys"
	"pix/host/workflow/onboard"
	"pix/host/workflow/pack"
	"pix/host/workflow/upgrade"
	"slices"
	"strings"

	"pix/host/config"
	"pix/host/monitor"
	"pix/host/workspace"
)

// runStatusCmd is the `status` verb AND the bare-`pix` landing screen: a
// fast, read-only control panel answering "what state am I in, what's my next
// move" — WITHOUT launching anything. It replaces the old footgun where bare
// `pix` spun up a sandbox.
func runStatusCmd(argv []string) {
	jsonOut, err := parseStatusArgs(argv)
	if err != nil {
		if err == cli.ErrHelpRequested {
			fmt.Print(statusUsage)
			return
		}
		fmt.Fprintf(os.Stderr, "pix status: %v\n\n%s", err, statusUsage)
		os.Exit(2)
	}
	cfg, name, err := workspace.LoadResolvedConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix status: %v\n", err)
		os.Exit(1)
	}
	// ONE exit contract (snapshot.ExitCode), with the 3 arm suppressed: status
	// is the landing screen and a JSON-scraping script must never fail merely
	// because a fact could not be checked from here (inside the sandbox, sbx is
	// absent and half the axes are unverifiable by construction). A POSITIVELY
	// verified core failure still exits 1, and the same integer is published as
	// the JSON `exit` sibling, so a reader of the rows and a reader of $? can
	// never disagree.
	if code := renderStatus(cfg, name, defaultShellEnv(), os.Stdout, jsonOut); code != readiness.ExitReady {
		os.Exit(code)
	}
}

// parseStatusArgs validates status flags: -h/--help returns cli.ErrHelpRequested,
// --json sets jsonOut, and any other token is a usage error (so a typo like
// --jsom fails loud instead of running silently as if no flag were given).
func parseStatusArgs(argv []string) (jsonOut bool, err error) {
	for _, a := range argv {
		switch a {
		case "-h", "--help":
			return false, cli.ErrHelpRequested
		case "--json":
			jsonOut = true
		default:
			return false, fmt.Errorf("unknown flag %q", a)
		}
	}
	return jsonOut, nil
}

// renderStatus is the testable core: it probes the environment via env and
// renders to out. Everything is best-effort and short-timeout so status never
// hangs on a down daemon.
func renderStatus(cfg *config.Config, profile string, env hostenv.Env, out io.Writer, jsonOut bool) int {
	st := gatherStatus(cfg, profile, env)
	if jsonOut {
		_ = cli.WriteJSONOut(out, st)
		return st.Exit
	}
	st.render(out)
	return st.Exit
}

// statusReport is the machine-readable status snapshot (also drives --json).
type statusReport struct {
	Version           string                  `json:"version"`
	ConfigPath        string                  `json:"config_path"`
	Profile           string                  `json:"profile"`
	Memory            bool                    `json:"memory_up"`
	Knowledge         bool                    `json:"knowledge_up"`
	Monitor           bool                    `json:"monitor_up"`
	EnabledServices   []string                `json:"enabled_services,omitempty"`
	Providers         map[string]bool         `json:"providers"`
	InferenceModels   int                     `json:"inference_models,omitempty"`
	InferenceBackends []string                `json:"inference_backends,omitempty"`
	Bundles           []bundleStatus          `json:"knowledge_bundles"`
	MCP               []string                `json:"mcp"`
	MCPServers        []mcpStatusLine         `json:"mcp_servers"`
	MCPRows           []mcpSandboxRow         `json:"mcp_sandbox_rows,omitempty"`
	Sandboxes         []workspace.SandboxLine `json:"sandboxes"`
	Tasks             int                     `json:"tasks"`
	ArtifactB         int64                   `json:"artifact_bytes"`
	Todos             []string                `json:"todos"`
	GogAccount        string                  `json:"gog_account,omitempty"`
	GogAuthed         bool                    `json:"gog_authed,omitempty"`
	InstallWarnings   []string                `json:"install_warnings,omitempty"`
	// Checks is the shared, flat readiness array (the SAME row type doctor
	// --json emits: axis/requirement/verdict/evidence/fix/duration_ms/
	// endpoint), and Exit is the process exit code this same data produced.
	// Both are ADDITIVE: every key above keeps its name and meaning.
	Checks []readinessCheckJSON `json:"checks"`
	Exit   int                  `json:"exit"`
	// Unverifiable is how many readiness checks could not be checked from
	// here. It never blocks (see Exit, which suppresses the 3 arm) but it does
	// stop the headline from claiming everything is fine.
	Unverifiable int `json:"unverifiable_checks"`
}

// mcpStatusLine is the per-server HOST-GLOBAL MCP summary: registered with
// the sbx gateway (from `sbx mcp ls`), plus the standing intent that every
// configured server preloads at sandbox create. It says NOTHING about any
// sandbox's current attachment — that is the per-sandbox join rows' job
// (mcpSandboxRow below), backed by the launcher receipt. Empty when sbx is
// unavailable, so status degrades to the bare MCP names.
type mcpStatusLine struct {
	Name       string `json:"name"`
	Registered bool   `json:"registered"`
	// Unverifiable is set when sbx is present but the bounded `sbx mcp ls`
	// listing itself failed/timed out (closure finding #1): registration is
	// genuinely UNKNOWN for this name, never a false "not registered", and
	// this entry must count toward the headline's unverifiable total so the
	// verdict can never read "all systems go" over unknown registration —
	// even with zero sandboxes/rows to otherwise carry that signal.
	Unverifiable bool `json:"unverifiable,omitempty"`
}

// mcpSandboxRow is one (server, sandbox) truth row from the shared join path
// (mcp.JoinMCPSandboxRow, mcpjoin.go — the same path doctor renders from):
// registered is the tri-state host registration evidence (yes|no|unknown),
// state one of preloaded|loaded|registered-not-attached|not-registered|
// unverifiable, evidence the concrete proof or degrade reason. Sandbox is
// empty when sandbox discovery itself was unavailable (state unverifiable).
type mcpSandboxRow struct {
	Name       string `json:"name"`
	Registered string `json:"registered"`
	Sandbox    string `json:"sandbox"`
	State      string `json:"state"`
	Evidence   string `json:"evidence"`
}

type bundleStatus struct {
	Path string `json:"path"`
	Git  string `json:"git"` // e.g. "clean", "3 ahead", "dirty", "no remote", ""
}

func gatherStatus(cfg *config.Config, profile string, env hostenv.Env) statusReport {
	// currentIntent is the "current config/pack" universe — cfg.MCP plus any
	// active-pack integration name not already there — the host-global
	// baseline BOTH the summary list and the per-sandbox rows start from
	// before a sandbox's own receipt extends the latter (mcp.McpConfiguredUniverse
	// below). Without a sandbox receipt, status/doctor stay current
	// config/pack only.
	currentIntent := mcp.McpCurrentIntentNames(cfg.MCP, pack.ActiveContainerMCP(cfg), nil)
	st := statusReport{
		Version:         version,
		ConfigPath:      config.Path(),
		Profile:         profile,
		Providers:       map[string]bool{},
		MCP:             currentIntent,
		EnabledServices: append([]string(nil), cfg.Services...),
	}
	self, err := env.Executable()
	if err == nil {
		if warning := upgrade.PathShadowIssue("pix", self, env.Getenv); warning != "" {
			st.InstallWarnings = append(st.InstallWarnings, warning)
		}
		if env.HostBinary != nil {
			host, err := env.HostBinary()
			if err == nil {
				if warning := upgrade.PathShadowIssue("pix-host", host, env.Getenv); warning != "" {
					st.InstallWarnings = append(st.InstallWarnings, warning)
				}
			}
		}
	}
	// monitor is an on-demand tool (`pix monitor`), not a background
	// serve service, so its up/down state is reported but never feeds the
	// "serve: up/down" label or an outstanding-item TODO below. It is the
	// only remaining bare dial here: memory and knowledge come from the
	// identity-verified readiness axes below, because a held port is not
	// proof that the process holding it is ours.
	st.Monitor = env.DialLocal(monitor.DefaultPort)

	// Providers: probe `sbx secret ls` ONCE (proxy-injected keys; never in VM)
	// and reuse that one result for the per-provider booleans below AND for the
	// shared readiness snapshot, which re-probes nothing. Track PATH-presence
	// (sbxOnPath) separately from probe success (sbxOK): sbx being installed but
	// `sbx secret ls` failing is a DIFFERENT state from sbx missing entirely,
	// and the two warrant different guidance.
	keyEvidence := axis.ProbeSbxKeyEvidence(env)
	sbxOut, sbxOK := keyEvidence.Out, keyEvidence.Ok()
	sbxOnPath := keyEvidence.State != secret.SbxSecretsAbsent

	// The SHARED lazy snapshot (readiness_launch.go): the one core launch
	// requirement plus the two host services, identity-verified. status renders
	// the same rows, in the same words, that doctor and run do.
	snap := axis.FastReadinessSnapshot(cfg, env, keyEvidence)
	st.Memory = axis.AxisReady(snap, readiness.AxisServiceMemory)
	st.Knowledge = axis.AxisReady(snap, readiness.AxisServiceKnowledge)
	st.Checks = readinessChecksJSON(snap.All())
	st.Exit = snap.ExitCodeSuppressingUnverifiable()
	unverifiableAxes := snap.UnverifiableCount()
	// Providers: doctor/launch parity (finding #3) — ONE core model-readiness
	// TODO, never a per-key TODO for a missing alternate. pix only needs
	// ONE of anthropic/openai/google to launch a model (axis.AnyModelKeyInOutput,
	// the exact same tri-state definition doctor's modelKeyCoreCheck and
	// run's launch gate use), so:
	//   - sbxOK and at least one present -> ready, no core TODO (a missing
	//     alternate is expected, never itself outstanding);
	//   - sbxOK and POSITIVELY zero present -> exactly ONE exact fix command;
	//   - !sbxOK -> unverifiable, no false core TODO (the sbx-reachability
	//     TODO below already covers "status can't verify anything here").
	// github is optional infrastructure (authorizes git ops, not the model)
	// and is NEVER itself outstanding, whether set, absent, or unverifiable.
	// Per-provider booleans are still populated for informational display.
	for _, key := range []string{"anthropic", "openai", "google", "github"} {
		st.Providers[key] = sbxOK && cli.GrepWord(sbxOut, key)
	}
	st.InferenceModels, st.InferenceBackends = axis.ConfiguredInferenceSummary(cfg)
	// Every repair the snapshot's axes verified is taken FROM the snapshot, so
	// status can never print a different fix command than doctor for the same
	// fact. Unverifiable axes contribute no TODO by construction (a repair we
	// cannot prove is needed is a guess), and are counted separately below.
	for _, c := range snap.All() {
		if c.Note {
			continue
		}
		if v := c.Result(); (v == readiness.VerdictTodo || v == readiness.VerdictDenied) && c.Todo != "" {
			st.Todos = append(st.Todos, c.Todo)
		}
	}
	// When sbx could NOT verify keys, no per-key/core TODO above fires — so
	// without an outstanding item the verdict would be falsely "all systems
	// go". Distinguish the two failure modes: sbx not installed at all vs
	// installed-but-the-probe-failed. Not emitted when sbxOK is true (the core
	// TODO above covers that case).
	switch {
	case st.InferenceModels > 0:
	case !sbxOnPath:
		st.Todos = append(st.Todos, "install the Docker Sandboxes CLI (sbx) to verify provider keys")
	case !sbxOK:
		st.Todos = append(st.Todos, "could not verify provider keys (sbx secret ls failed); check sbx")
	}

	// Knowledge bundles + git drift.
	for _, b := range cfg.KnowledgeBundles {
		st.Bundles = append(st.Bundles, bundleStatus{Path: b, Git: bundleGitStatus(env, b)})
	}

	// MCP registration evidence: ONE bounded `sbx mcp ls`, best-effort. The
	// host-global summary reports configured-to-preload intent + registration
	// only — NEVER any sandbox's current attachment (registration is a host
	// fact; `sbx mcp get <name>` inspects the registered definition and sbx has
	// no per-sandbox inspect API). When the listing is unavailable MCPServers
	// stays nil and render falls back to the bare names.
	//
	// Probed whenever sbx is on PATH — NOT gated on currentIntent being
	// non-empty (finding #3): a discovered sandbox's own receipt can name a
	// server that current cfg/pack intent no longer does (a transient `run
	// --pack` mix-in, or a since-switched pack's historical MCP), and that
	// receipt-only name still needs registration evidence for its per-sandbox
	// row below even when the host-global summary has nothing to show.
	mcpLsOut, mcpLsOK := "", false
	if sbxOnPath {
		// BOUNDED (probeRun): timeout/failure -> registration unknowable
		// (mcp.McpRegUnknown everywhere below), never a hang or a false "no".
		if o, timedOut, err := env.RunTimed("sbx", "mcp", "ls"); err == nil && !timedOut {
			mcpLsOut, mcpLsOK = o, true
		}
	}
	regOf := func(name string) mcp.McpRegEvidence { return mcp.McpRegEvidenceFrom(mcpLsOut, mcpLsOK, name) }
	switch {
	case mcpLsOK:
		registerTodoFor := statusRegisterTodoFn(cfg, env)
		for _, m := range currentIntent {
			reg := regOf(m) == mcp.McpRegYes
			st.MCPServers = append(st.MCPServers, mcpStatusLine{Name: m, Registered: reg})
			// A POSITIVELY unregistered server can't be spawned by the gateway — an
			// outstanding item with the TYPE-CORRECT repair (the same classifier +
			// command table doctor uses). Only emitted when the listing succeeded
			// (otherwise registration is unknowable and no TODO is honest).
			if !reg {
				st.Todos = append(st.Todos, registerTodoFor(m))
			}
		}
	case sbxOnPath && len(currentIntent) > 0:
		// Closure finding #1: sbx IS present (the listing was attempted) but
		// `sbx mcp ls` failed/timed out — registration is UNKNOWN for every
		// current-intent name, never a false "not registered" and never a
		// TODO (doctor/status never invent a repair from a probe that answered
		// nothing). Tracked here so the headline can't read "all systems go"
		// over unknown registration, even with zero sandboxes/rows below. When
		// sbx is off PATH entirely, this stays empty on purpose — that case
		// already surfaces via the per-sandbox MCPRows discovery-unavailable
		// path (TestGatherStatusMCPSbxAbsent).
		for _, m := range currentIntent {
			st.MCPServers = append(st.MCPServers, mcpStatusLine{Name: m, Unverifiable: true})
		}
	}

	// Integrations: Google Workspace is "account set, needs auth" until a real auth
	// probe passes (an email alone is not completed OAuth). Best-effort.
	st.GogAccount = cfg.GogAccount
	if cfg.GogAccount != "" {
		st.GogAuthed = onboard.GogAuthed(env, cfg.GogAccount)
		// An account set but not authed is an outstanding item: setting an email is
		// not completed OAuth, so the verdict must not read "all systems go".
		if !st.GogAuthed {
			st.Todos = append(st.Todos, gogSetupHint)
		}
	}

	// Sandboxes: ONE `sbx ls`, filtered to pix-* boxes. Discovery success
	// is tracked separately from emptiness: a failed listing must never render
	// as "no sandboxes".
	sbxLsOK := false
	var boxes []workspace.SbxBox
	if sbxOnPath {
		// BOUNDED (probeRun): a hung `sbx ls` leaves discovery unavailable —
		// the rows below render unverifiable, never a false "no sandboxes".
		if o, timedOut, err := env.RunTimed("sbx", "ls"); err == nil && !timedOut {
			sbxLsOK = true
			st.Sandboxes = workspace.ParseSandboxes(o)
			boxes = workspace.ParsePixBoxes(o)
		}
	}

	// Per-sandbox MCP truth rows via the SHARED join path (mcpjoin.go — the
	// same one doctor renders from): each discovered pix sandbox's rows
	// come from its launcher receipt joined with the registration evidence
	// above. Discovery unavailable -> one unverifiable row per configured
	// server (sandbox unknown), never a false "no sandboxes". NOT gated on
	// currentIntent being non-empty (finding #3): with nothing configured and
	// discovery unavailable the loop below is simply a no-op (nothing to
	// render as unverifiable), but with discovery AVAILABLE every discovered
	// box's own receipt is still consulted — a receipt-only name surfaces even
	// when current cfg/pack intent is empty.
	if !sbxLsOK {
		for _, m := range currentIntent {
			st.MCPRows = append(st.MCPRows, mcpSandboxRow{
				Name: m, Registered: regOf(m).String(), Sandbox: "",
				State:    mcp.McpJoinUnverifiable,
				Evidence: "sandbox discovery unavailable (`sbx ls`); cannot enumerate pix sandboxes",
			})
		}
	} else {
		for _, b := range boxes {
			receipt, rstatus := statusSandboxReceipt(env, b.Name)
			// Per-sandbox universe (finding #2): currentIntent extended with
			// any name THIS sandbox's own receipt independently proves
			// provenance for — a transient `run --pack` mix-in or a
			// since-switched pack's historical MCP stays visible here even
			// after cfg.MCP/the active pack moved on (including when
			// currentIntent itself is EMPTY — finding #3). receiptOnly names
			// are labeled in evidence as sandbox provenance, never current
			// intent.
			names, receiptOnly := mcp.McpConfiguredUniverse(currentIntent, receipt, nil)
			for _, row := range mcp.JoinMCPSandboxRows(names, regOf, b.Name, receipt, rstatus) {
				evidence := row.Evidence
				if receiptOnly[row.Name] {
					evidence += "; sandbox provenance only (from this sandbox's receipt); " + row.Name + " is not part of the current cfg.MCP/pack"
				}
				st.MCPRows = append(st.MCPRows, mcpSandboxRow{
					Name: row.Name, Registered: row.Registered.String(),
					Sandbox: row.Sandbox, State: row.State, Evidence: evidence,
				})
				// registered-not-attached is a POSITIVE gap (a valid receipt
				// lacks the entry): the exact live-attach command is the TODO.
				// Unverifiable rows get guidance in their evidence only — status
				// does not KNOW they are unattached, so no repair claim.
				if row.State == mcp.McpJoinRegisteredNotAttached {
					// mcpLoadCommand shell-quotes both name and workspace via
					// sys.ShellQuote (closure finding #3), so this repair command
					// round-trips a workspace with spaces/apostrophe/shell
					// metacharacters safely when copy-pasted.
					td := "pix mcp load " + sys.ShellQuote(row.Name) + " [DIR]"
					switch {
					case receipt != nil && strings.TrimSpace(receipt.Workspace) != "":
						// The receipt's own canonical workspace is the most exact
						// DIR (a custom-named box may not match sbx's dir column).
						td = mcpLoadCommand(row.Name, receipt.Workspace)
					case b.Dir != "":
						td = mcpLoadCommand(row.Name, b.Dir)
					}
					st.Todos = append(st.Todos, td)
				}
			}
		}
	}

	// Tasks + harvested artifacts: global, repo-agnostic counts so the pile is
	// visible without any per-repo git probing.
	st.Tasks, st.ArtifactB = taskStateSummary()
	st.Unverifiable = unverifiableAxes
	return st
}

func (st statusReport) render(out io.Writer) {
	for _, warning := range st.InstallWarnings {
		fmt.Fprintf(out, "  %s install     %s\n",
			readiness.VerdictGlyph(readiness.RequirementOptional, readiness.VerdictTodo, false),
			strings.ReplaceAll(warning, "\n", "\n                "))
	}
	fmt.Fprintf(out, "pix %s\n\n", st.Version)

	// The landing screen reports only services the user enabled. Disabled
	// optional capabilities are absence, not failures, and must not paint the
	// dashboard red. Doctor/--json retain the full probe detail.
	var services []string
	for _, name := range st.EnabledServices {
		switch name {
		case "memory":
			services = append(services, compactReady("memory", st.Memory))
		case "knowledge":
			services = append(services, compactReady("knowledge", st.Knowledge))
		default:
			services = append(services, name)
		}
	}
	if len(services) > 0 {
		fmt.Fprintf(out, "  services     %s\n", strings.Join(services, " · "))
	}
	if st.Monitor {
		fmt.Fprintf(out, "  monitor      active · :%d\n", monitor.DefaultPort)
	}

	if st.InferenceModels > 0 {
		fmt.Fprintf(out, "  inference    %d model(s) via %s\n", st.InferenceModels, strings.Join(st.InferenceBackends, ", "))
	} else {
		var prov []string
		for _, k := range []string{"anthropic", "openai", "google", "github"} {
			prov = append(prov, fmt.Sprintf("%s %s", k, okGlyph(st.Providers[k])))
		}
		fmt.Fprintf(out, "  providers    %s\n", strings.Join(prov, "  "))
	}

	if len(st.Bundles) > 0 {
		fmt.Fprintf(out, "  knowledge    %s\n", cli.Plural(len(st.Bundles), "bundle"))
	}

	st.renderIntegrations(out)

	if st.GogAccount != "" {
		label := "account set, needs auth (run " + gogSetupHint + ")"
		if st.GogAuthed {
			label = "authed"
		}
		// A configured Workspace account is already included in the MCP summary.
		// Only render it separately when it needs action or is not an MCP.
		if !st.GogAuthed || !slices.Contains(st.MCP, config.GWServerName) {
			fmt.Fprintf(out, "  ws    %s\n", label)
		}
	}

	if len(st.Sandboxes) > 0 {
		states := map[string]int{}
		for _, s := range st.Sandboxes {
			states[s.State]++
		}
		var summary []string
		for _, state := range []string{"running", "stopped", "exited", "created", "paused", "restarting", "dead", "?"} {
			if n := states[state]; n > 0 {
				summary = append(summary, fmt.Sprintf("%d %s", n, state))
			}
		}
		fmt.Fprintf(out, "  sandboxes    %s · `pix ls` for details\n", strings.Join(summary, ", "))
	}

	// Show the line when there are tasks OR retained artifacts — harvested docs
	// can outlive the last task clone, and they still cost disk.
	if st.Tasks > 0 || st.ArtifactB > 0 {
		taskText := cli.Plural(st.Tasks, "task")
		if st.ArtifactB > 0 {
			taskText += " · " + tui.HumanBytes(st.ArtifactB) + " artifacts"
		}
		fmt.Fprintf(out, "  tasks        %s · `pix task ls` for details\n", taskText)
	}

	fmt.Fprintln(out)
	// Headline honesty: an UNVERIFIABLE MCP row (corrupt/absent receipt, a
	// failed listing) is not a verified failure — it earns no TODO — but it
	// also means status does NOT know everything is fine, so it must never
	// read "all systems go" over it. The JSON rows above stay the row truth.
	// The shared snapshot's own unverifiable axes count first: inside the
	// sandbox (no sbx) the provider axis is unverifiable, and the headline must
	// say so rather than claim everything is fine.
	unverifiable := st.Unverifiable
	for _, r := range st.MCPRows {
		if r.State == mcp.McpJoinUnverifiable {
			unverifiable++
		}
	}
	// Closure finding #1: a failed host-global registration listing is its own
	// unverifiable signal — independent of any per-sandbox row — so it must
	// count toward the headline even with zero sandboxes/rows to otherwise
	// carry it.
	for _, m := range st.MCPServers {
		if m.Unverifiable {
			unverifiable++
		}
	}
	switch {
	case len(st.Todos) > 0:
		fmt.Fprintf(out, "  %s %s outstanding.   `%s` for fix commands.\n",
			readiness.VerdictGlyph(readiness.RequirementOptional, readiness.VerdictTodo, false), cli.Plural(len(st.Todos), "item"), readiness.Footer("status", readiness.Snapshot{}))
	case unverifiable > 0:
		fmt.Fprintf(out, "  %s nothing outstanding, but %s unverifiable (not failed; run `%s` for details).\n",
			readiness.VerdictGlyph(readiness.RequirementCore, readiness.VerdictReady, false), cli.Plural(unverifiable, "check"), readiness.Footer("status", readiness.Snapshot{}))
	default:
		fmt.Fprintf(out, "  %s all systems go.\n", readiness.VerdictGlyph(readiness.RequirementCore, readiness.VerdictReady, false))
	}
	fmt.Fprintln(out)
	if len(st.Todos) > 0 {
		fmt.Fprintln(out, "Next:  pix doctor    show the fix")
		fmt.Fprintln(out, "       pix run       launch or resume Pix")
	} else {
		fmt.Fprintln(out, "Next:  pix run       launch or resume Pix")
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Help:  pix help")
}

func compactReady(name string, ready bool) string {
	if ready {
		return name + " ready"
	}
	return name + " needs attention"
}

// renderIntegrations turns the host registration + per-sandbox receipt join
// into one human sentence. The row-level evidence remains available in JSON
// and doctor; healthy N×M joins do not belong on the landing screen.
func (st statusReport) renderIntegrations(out io.Writer) {
	names := map[string]bool{}
	readyNames := map[string]bool{}
	unknownNames := map[string]bool{}
	for _, name := range st.MCP {
		names[name] = true
	}
	for _, m := range st.MCPServers {
		names[m.Name] = true
		if m.Registered {
			readyNames[m.Name] = true
		}
		if m.Unverifiable {
			unknownNames[m.Name] = true
		}
	}
	for _, r := range st.MCPRows {
		names[r.Name] = true
		if r.Registered == mcp.McpRegYes.String() {
			readyNames[r.Name] = true
		}
		if r.Registered == mcp.McpRegUnknown.String() {
			unknownNames[r.Name] = true
		}
	}
	if len(names) == 0 {
		return
	}
	if len(st.MCPServers) == 0 && len(st.MCPRows) == 0 {
		fmt.Fprintf(out, "  integrations  %s configured\n", cli.Plural(len(names), "integration"))
		return
	}
	boxReady := map[string]bool{}
	boxSeen := map[string]bool{}
	for _, r := range st.MCPRows {
		if r.Sandbox == "" {
			continue
		}
		if !boxSeen[r.Sandbox] {
			boxReady[r.Sandbox] = true
		}
		boxSeen[r.Sandbox] = true
		if r.State != mcp.McpJoinPreloaded && r.State != mcp.McpJoinLoaded {
			boxReady[r.Sandbox] = false
		}
	}
	attached := 0
	for box := range boxSeen {
		if boxReady[box] {
			attached++
		}
	}
	status := fmt.Sprintf("%d/%d ready", len(readyNames), len(names))
	if len(readyNames) == len(names) {
		status = fmt.Sprintf("%d ready", len(readyNames))
	}
	if len(unknownNames) > 0 {
		status += fmt.Sprintf(" · %d unverifiable", len(unknownNames))
	}
	if attached > 0 {
		word := "sandboxes"
		if attached == 1 {
			word = "sandbox"
		}
		status += fmt.Sprintf(" · available in %d %s", attached, word)
	}
	fmt.Fprintf(out, "  integrations  %s\n", status)
}

// statusRegisterTodoFn returns a memoized picker for the TYPE-CORRECT
// registration repair of a positively-unregistered configured server, reusing
// doctor's classifier + command table (classifyMCPServer / mcpRegisterTodo)
// so the two verbs can never recommend different commands. The classification
// inputs (pack integrations, the bounded `pix-host mcp --list` probe)
// are resolved at most ONCE, on first use — status never pays them when every
// server is registered. When classification itself is unavailable NO command
// is safe to recommend — the TODO points at doctor instead of guessing one.
func statusRegisterTodoFn(cfg *config.Config, env hostenv.Env) func(name string) string {
	var (
		resolved   bool
		containers map[string]config.MCPContainer
		localSet   map[string]bool
		localKnown bool
	)
	return func(name string) string {
		if !resolved {
			resolved = true
			containers = pack.ActiveContainerMCP(cfg)
			localSet, localKnown = mcp.LocalMCPNames(env, env.HostBinary)
		}
		kind := classifyMCPServer(name, containers, localSet, localKnown)
		if td := mcpRegisterTodo(name, kind); td != "" {
			return td
		}
		return "mcp " + name + " is not registered but could not be classified; run `pix doctor`"
	}
}

// statusSandboxReceipt reads one discovered sandbox's launcher MCP receipt
// through the stateDir seam. An unresolvable state dir yields
// workspace.MCPStateUnreadable so the join renders UNVERIFIABLE — never a
// guessed empty receipt.
func statusSandboxReceipt(env hostenv.Env, sandbox string) (*workspace.MCPReceipt, workspace.MCPStateStatus) {

	sd, err := env.StateDir()
	if err != nil || strings.TrimSpace(sd) == "" {
		return nil, workspace.MCPStateUnreadable
	}
	receipt, rstatus, _ := workspace.ReadMCPReceipt(sd, sandbox)
	return receipt, rstatus
}

// mcpRowText renders one per-sandbox join row for humans: a glyph class per
// state plus the row's evidence (which already carries the exact commands for
// the states that have one).
func mcpRowText(r mcpSandboxRow) string {
	switch r.State {
	case mcp.McpJoinPreloaded, mcp.McpJoinLoaded:
		return readiness.VerdictGlyph(readiness.RequirementCore, readiness.VerdictReady, false) + " " + r.State + " (" + r.Evidence + ")"
	case mcp.McpJoinNotRegistered, mcp.McpJoinRegisteredNotAttached:
		return readiness.VerdictGlyph(readiness.RequirementCore, readiness.VerdictTodo, false) + " " + r.State + ": " + r.Evidence
	default: // unverifiable
		return readiness.VerdictGlyph(readiness.RequirementCore, readiness.VerdictUnverifiable, false) + " " + r.State + ": " + r.Evidence
	}
}

// glyph renders a bool as the shared ✓/✗ status glyphs.
func okGlyph(ok bool) string {
	if ok {
		return readiness.VerdictGlyph(readiness.RequirementCore, readiness.VerdictReady, false)
	}
	return readiness.VerdictGlyph(readiness.RequirementCore, readiness.VerdictTodo, false)
}

// bundleGitStatus returns a short git-drift summary for a bundle dir: "clean",
// "dirty", "N ahead", "no remote", or "" when it isn't a git repo / git is
// absent. Best-effort and short — never blocks status.
func bundleGitStatus(env hostenv.Env, dir string) string {

	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return ""
	}
	dirty := false
	if o, err := env.Run("git", "-C", dir, "status", "--porcelain"); err == nil {
		dirty = strings.TrimSpace(o) != ""
	}
	remote, rerr := env.Run("git", "-C", dir, "remote")
	if rerr != nil || strings.TrimSpace(remote) == "" {
		if dirty {
			return "dirty, no remote"
		}
		return "no remote"
	}
	ahead := ""
	if o, err := env.Run("git", "-C", dir, "rev-list", "--count", "@{upstream}..HEAD"); err == nil {
		if n := strings.TrimSpace(o); n != "" && n != "0" {
			ahead = n + " ahead"
		}
	}
	switch {
	case dirty && ahead != "":
		return "dirty, " + ahead
	case dirty:
		return "dirty"
	case ahead != "":
		return ahead
	default:
		return "clean"
	}
}
