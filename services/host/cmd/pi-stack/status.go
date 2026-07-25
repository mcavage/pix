package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pi-stack/host/config"
	"pi-stack/host/monitor"
)

// gogAuthTimeout bounds the `gog auth status` probe so the fast, read-only
// `status` command can never hang on a network round-trip (mirrors doctor's
// bounded probes). On timeout or error gog is treated as not-authed.
const gogAuthTimeout = 2 * time.Second

// runStatusCmd is the `status` verb AND the bare-`pi-stack` landing screen: a
// fast, read-only control panel answering "what state am I in, what's my next
// move" — WITHOUT launching anything. It replaces the old footgun where bare
// `pi-stack` spun up a sandbox.
func runStatusCmd(argv []string) {
	jsonOut, err := parseStatusArgs(argv)
	if err != nil {
		if err == errHelpRequested {
			fmt.Print(statusUsage)
			return
		}
		fmt.Fprintf(os.Stderr, "pi-stack status: %v\n\n%s", err, statusUsage)
		os.Exit(2)
	}
	cfg, name, err := loadResolvedConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack status: %v\n", err)
		os.Exit(1)
	}
	renderStatus(cfg, name, defaultShellEnv(), os.Stdout, jsonOut)
}

// parseStatusArgs validates status flags: -h/--help returns errHelpRequested,
// --json sets jsonOut, and any other token is a usage error (so a typo like
// --jsom fails loud instead of running silently as if no flag were given).
func parseStatusArgs(argv []string) (jsonOut bool, err error) {
	for _, a := range argv {
		switch a {
		case "-h", "--help":
			return false, errHelpRequested
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
func renderStatus(cfg *config.Config, profile string, env shellEnv, out io.Writer, jsonOut bool) {
	st := gatherStatus(cfg, profile, env)
	if jsonOut {
		_ = writeJSONOut(out, st)
		return
	}
	st.render(out)
}

// statusReport is the machine-readable status snapshot (also drives --json).
type statusReport struct {
	Version    string          `json:"version"`
	ConfigPath string          `json:"config_path"`
	Profile    string          `json:"profile"`
	Memory     bool            `json:"memory_up"`
	Knowledge  bool            `json:"knowledge_up"`
	Monitor    bool            `json:"monitor_up"`
	Providers  map[string]bool `json:"providers"`
	Bundles    []bundleStatus  `json:"knowledge_bundles"`
	MCP        []string        `json:"mcp"`
	MCPServers []mcpStatusLine `json:"mcp_servers"`
	MCPRows    []mcpSandboxRow `json:"mcp_sandbox_rows,omitempty"`
	Sandboxes  []sandboxLine   `json:"sandboxes"`
	Tasks      int             `json:"tasks"`
	ArtifactB  int64           `json:"artifact_bytes"`
	Todos      []string        `json:"todos"`
	GogAccount string          `json:"gog_account,omitempty"`
	GogAuthed  bool            `json:"gog_authed,omitempty"`
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
}

// mcpSandboxRow is one (server, sandbox) truth row from the shared join path
// (joinMCPSandboxRow, mcpjoin.go — the same path doctor renders from):
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

type sandboxLine struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

func gatherStatus(cfg *config.Config, profile string, env shellEnv) statusReport {
	memPort, knPort := memoryClient().Port, knowledgeClient().Port
	st := statusReport{
		Version:    version,
		ConfigPath: config.Path(),
		Profile:    profile,
		Providers:  map[string]bool{},
		MCP:        cfg.MCP,
	}
	if env.dial != nil {
		st.Memory = env.dial(memPort)
		st.Knowledge = env.dial(knPort)
		// monitor is an on-demand tool (`pi-stack monitor`), not a background
		// serve service, so its up/down state is reported but never feeds the
		// "serve: up/down" label or an outstanding-item TODO below.
		st.Monitor = env.dial(monitor.DefaultPort)
	}

	// Providers: probe `sbx secret ls` once (proxy-injected keys; never in VM).
	// Track PATH-presence (sbxOnPath) separately from probe success (sbxOK): sbx
	// being installed but `sbx secret ls` failing is a DIFFERENT state from sbx
	// missing entirely, and the two warrant different guidance.
	sbxOut, sbxOK := "", false
	sbxOnPath := false
	if env.lookPath != nil {
		if _, err := env.lookPath("sbx"); err == nil {
			sbxOnPath = true
			if env.run != nil {
				if o, err := env.run("sbx", "secret", "ls"); err == nil {
					sbxOut, sbxOK = o, true
				}
			}
		}
	}
	for _, key := range []string{"anthropic", "openai", "google", "github"} {
		set := sbxOK && grepWord(sbxOut, key)
		st.Providers[key] = set
		if sbxOK && !set {
			st.Todos = append(st.Todos, "sbx secret set -g "+key)
		}
	}
	// When sbx could NOT verify keys every provider renders ✗ but no per-key TODO
	// is added — so without an outstanding item the verdict would be falsely "all
	// systems go". Distinguish the two failure modes: sbx not installed at all vs
	// installed-but-the-probe-failed. Not emitted when sbxOK is true (the per-key
	// TODOs cover that case).
	switch {
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
	mcpLsOut, mcpLsOK := "", false
	if len(cfg.MCP) > 0 && sbxOnPath && env.run != nil {
		if o, err := env.run("sbx", "mcp", "ls"); err == nil {
			mcpLsOut, mcpLsOK = o, true
		}
	}
	regOf := func(name string) mcpRegEvidence {
		if !mcpLsOK {
			return mcpRegUnknown
		}
		if grepWord(mcpLsOut, name) {
			return mcpRegYes
		}
		return mcpRegNo
	}
	if mcpLsOK {
		registerTodoFor := statusRegisterTodoFn(cfg, env)
		for _, m := range cfg.MCP {
			reg := regOf(m) == mcpRegYes
			st.MCPServers = append(st.MCPServers, mcpStatusLine{Name: m, Registered: reg})
			// A POSITIVELY unregistered server can't be spawned by the gateway — an
			// outstanding item with the TYPE-CORRECT repair (the same classifier +
			// command table doctor uses). Only emitted when the listing succeeded
			// (otherwise registration is unknowable and no TODO is honest).
			if !reg {
				st.Todos = append(st.Todos, registerTodoFor(m))
			}
		}
	}

	// Integrations: gog is "account set, needs auth" until a real `gog auth status`
	// probe passes (an email alone is not completed OAuth). Best-effort.
	st.GogAccount = cfg.GogAccount
	if cfg.GogAccount != "" {
		st.GogAuthed = gogAuthed(env, cfg.GogAccount)
		// An account set but not authed is an outstanding item: setting an email is
		// not completed OAuth, so the verdict must not read "all systems go".
		if !st.GogAuthed {
			st.Todos = append(st.Todos, gogSetupHint)
		}
	}

	// Sandboxes: ONE `sbx ls`, filtered to pi-stack-* boxes. Discovery success
	// is tracked separately from emptiness: a failed listing must never render
	// as "no sandboxes".
	sbxLsOK := false
	var boxes []sbxBox
	if sbxOnPath && env.run != nil {
		if o, err := env.run("sbx", "ls"); err == nil {
			sbxLsOK = true
			st.Sandboxes = parseSandboxes(o)
			boxes = parsePiStackBoxes(o)
		}
	}

	// Per-sandbox MCP truth rows via the SHARED join path (mcpjoin.go — the
	// same one doctor renders from): each discovered pi-stack sandbox's rows
	// come from its launcher receipt joined with the registration evidence
	// above. Discovery unavailable -> one unverifiable row per configured
	// server (sandbox unknown), never a false "no sandboxes".
	if len(cfg.MCP) > 0 {
		if !sbxLsOK {
			for _, m := range cfg.MCP {
				st.MCPRows = append(st.MCPRows, mcpSandboxRow{
					Name: m, Registered: regOf(m).String(), Sandbox: "",
					State:    mcpJoinUnverifiable,
					Evidence: "sandbox discovery unavailable (`sbx ls`) — cannot enumerate pi-stack sandboxes",
				})
			}
		} else {
			for _, b := range boxes {
				receipt, rstatus := statusSandboxReceipt(env, b.Name)
				for _, row := range joinMCPSandboxRows(cfg.MCP, regOf, b.Name, receipt, rstatus) {
					st.MCPRows = append(st.MCPRows, mcpSandboxRow{
						Name: row.Name, Registered: row.Registered.String(),
						Sandbox: row.Sandbox, State: row.State, Evidence: row.Evidence,
					})
					// registered-not-attached is a POSITIVE gap (a valid receipt
					// lacks the entry): the exact live-attach command is the TODO.
					// Unverifiable rows get guidance in their evidence only — status
					// does not KNOW they are unattached, so no repair claim.
					if row.State == mcpJoinRegisteredNotAttached {
						td := "pi-stack mcp load " + row.Name + " [DIR]"
						if b.Dir != "" {
							td = "pi-stack mcp load " + row.Name + " " + b.Dir
						}
						st.Todos = append(st.Todos, td)
					}
				}
			}
		}
	}

	// Tasks + harvested artifacts: global, repo-agnostic counts so the pile is
	// visible without any per-repo git probing.
	st.Tasks, st.ArtifactB = taskStateSummary()
	return st
}

func (st statusReport) render(out io.Writer) {
	fmt.Fprintf(out, "pi-stack %s    config: %s\n\n", st.Version, st.ConfigPath)

	serve := "down"
	if st.Memory || st.Knowledge {
		serve = "up"
	}
	fmt.Fprintf(out, "  services    memory %s :%d    knowledge %s :%d    (serve: %s)\n",
		okGlyph(st.Memory), memoryClient().Port, okGlyph(st.Knowledge), knowledgeClient().Port, serve)
	fmt.Fprintf(out, "  monitor     %s :%d    (on-demand: `pi-stack monitor`)\n", okGlyph(st.Monitor), monitor.DefaultPort)

	var prov []string
	for _, k := range []string{"anthropic", "openai", "google", "github"} {
		prov = append(prov, fmt.Sprintf("%s %s", k, okGlyph(st.Providers[k])))
	}
	fmt.Fprintf(out, "  providers   %s\n", strings.Join(prov, "  "))

	if len(st.Bundles) == 0 {
		fmt.Fprintln(out, "  knowledge   (no bundle) — `pi-stack knowledge init`")
	} else {
		for i, b := range st.Bundles {
			label := "knowledge"
			if i > 0 {
				label = "         "
			}
			git := ""
			if b.Git != "" {
				git = " (" + b.Git + ")"
			}
			fmt.Fprintf(out, "  %s   %s%s\n", label, b.Path, git)
		}
	}

	if len(st.MCP) == 0 {
		fmt.Fprintln(out, "  mcp         (none)")
	} else if len(st.MCPServers) == 0 {
		// sbx unavailable (e.g. inside the sandbox): degrade to the bare names.
		fmt.Fprintf(out, "  mcp         %s\n", strings.Join(st.MCP, ", "))
	} else {
		// Host-global summary: registration + preload intent only — a sandbox's
		// current attachment is the per-sandbox rows' job below.
		for i, m := range st.MCPServers {
			label := "mcp"
			if i > 0 {
				label = "   "
			}
			reg := okGlyph(m.Registered) + " registered"
			if !m.Registered {
				reg = okGlyph(false) + " not registered"
			}
			fmt.Fprintf(out, "  %-9s   %-8s %s · preloads at sandbox create\n", label, m.Name, reg)
		}
	}

	// Per-sandbox MCP truth rows (receipt-backed, from the shared join path).
	for i, r := range st.MCPRows {
		label := "mcp/box"
		if i > 0 {
			label = "       "
		}
		box := r.Sandbox
		if box == "" {
			box = "(sandboxes unknown)"
		}
		fmt.Fprintf(out, "  %-9s   %-24s %-8s %s\n", label, box, r.Name, mcpRowText(r))
	}

	if st.GogAccount != "" {
		label := "account set, needs auth (run " + gogSetupHint + ")"
		if st.GogAuthed {
			label = "authed"
		}
		fmt.Fprintf(out, "  integrations  gog %s\n", label)
	}

	if len(st.Sandboxes) > 0 {
		for i, s := range st.Sandboxes {
			label := "sandboxes"
			if i > 0 {
				label = "         "
			}
			fmt.Fprintf(out, "  %s   %-24s %s\n", label, s.Name, s.State)
		}
	}

	// Show the line when there are tasks OR retained artifacts — harvested docs
	// can outlive the last task clone, and they still cost disk.
	if st.Tasks > 0 || st.ArtifactB > 0 {
		fmt.Fprintf(out, "  tasks       %s   artifacts %s   `pi-stack task gc` to prune\n",
			plural(st.Tasks, "clone"), humanBytes(st.ArtifactB))
	}

	fmt.Fprintln(out)
	if len(st.Todos) > 0 {
		fmt.Fprintf(out, "  ⚠ %s outstanding.   `pi-stack doctor` for fix commands.\n", plural(len(st.Todos), "item"))
	} else {
		fmt.Fprintln(out, "  ✓ all systems go.")
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Next:  pi-stack serve     start the knowledge service")
	fmt.Fprintln(out, "       pi-stack run       launch a sandbox and start working")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Everything ok? run `pi-stack doctor`.   Full command list: `pi-stack help`.")
}

// statusRegisterTodoFn returns a memoized picker for the TYPE-CORRECT
// registration repair of a positively-unregistered configured server, reusing
// doctor's classifier + command table (classifyMCPServer / mcpRegisterTodo)
// so the two verbs can never recommend different commands. The classification
// inputs (pack integrations, the bounded `pi-stack-host mcp --list` probe)
// are resolved at most ONCE, on first use — status never pays them when every
// server is registered. When classification itself is unavailable NO command
// is safe to recommend — the TODO points at doctor instead of guessing one.
func statusRegisterTodoFn(cfg *config.Config, env shellEnv) func(name string) string {
	var (
		resolved   bool
		containers map[string]packContainer
		localSet   map[string]bool
		localKnown bool
	)
	return func(name string) string {
		if !resolved {
			resolved = true
			containers = activeContainerMCP(cfg)
			localSet, localKnown = localMCPNames(env, env.hostBinary)
		}
		kind := classifyMCPServer(name, containers, localSet, localKnown)
		if td := mcpRegisterTodo(name, kind); td != "" {
			return td
		}
		return "mcp " + name + " is not registered but could not be classified — run `pi-stack doctor`"
	}
}

// statusSandboxReceipt reads one discovered sandbox's launcher MCP receipt
// through the stateDir seam. An unresolvable state dir yields
// sandboxMCPStateUnreadable so the join renders UNVERIFIABLE — never a
// guessed empty receipt.
func statusSandboxReceipt(env shellEnv, sandbox string) (*sandboxMCPReceipt, sandboxMCPStateStatus) {
	if env.stateDir == nil {
		return nil, sandboxMCPStateUnreadable
	}
	sd, err := env.stateDir()
	if err != nil || strings.TrimSpace(sd) == "" {
		return nil, sandboxMCPStateUnreadable
	}
	receipt, rstatus, _ := readSandboxMCPReceipt(sd, sandbox)
	return receipt, rstatus
}

// mcpRowText renders one per-sandbox join row for humans: a glyph class per
// state plus the row's evidence (which already carries the exact commands for
// the states that have one).
func mcpRowText(r mcpSandboxRow) string {
	switch r.State {
	case mcpJoinPreloaded, mcpJoinLoaded:
		return "✓ " + r.State + " (" + r.Evidence + ")"
	case mcpJoinNotRegistered, mcpJoinRegisteredNotAttached:
		return "✗ " + r.State + " — " + r.Evidence
	default: // unverifiable
		return "? " + r.State + " — " + r.Evidence
	}
}

// glyph renders a bool as the shared ✓/✗ status glyphs.
func okGlyph(ok bool) string {
	if ok {
		return "✓"
	}
	return "✗"
}

// bundleGitStatus returns a short git-drift summary for a bundle dir: "clean",
// "dirty", "N ahead", "no remote", or "" when it isn't a git repo / git is
// absent. Best-effort and short — never blocks status.
func bundleGitStatus(env shellEnv, dir string) string {
	if env.statFile == nil || env.run == nil {
		return ""
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return ""
	}
	dirty := false
	if o, err := env.run("git", "-C", dir, "status", "--porcelain"); err == nil {
		dirty = strings.TrimSpace(o) != ""
	}
	remote, rerr := env.run("git", "-C", dir, "remote")
	if rerr != nil || strings.TrimSpace(remote) == "" {
		if dirty {
			return "dirty, no remote"
		}
		return "no remote"
	}
	ahead := ""
	if o, err := env.run("git", "-C", dir, "rev-list", "--count", "@{upstream}..HEAD"); err == nil {
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

// parseSandboxes extracts pi-stack-* sandbox lines from `sbx ls` output. It is
// lenient about column layout: it takes the first token as the name and the
// last token as the state, keeping only names starting with "pi-stack-".
func parseSandboxes(sbxLsOut string) []sandboxLine {
	var out []sandboxLine
	for _, ln := range strings.Split(sbxLsOut, "\n") {
		fields := strings.Fields(ln)
		if len(fields) < 2 {
			continue
		}
		name := fields[0]
		if !strings.HasPrefix(name, "pi-stack-") {
			continue
		}
		out = append(out, sandboxLine{Name: name, State: fields[len(fields)-1]})
	}
	return out
}
