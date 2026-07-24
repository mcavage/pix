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
	Sandboxes  []sandboxLine   `json:"sandboxes"`
	Tasks      int             `json:"tasks"`
	ArtifactB  int64           `json:"artifact_bytes"`
	Todos      []string        `json:"todos"`
	GogAccount string          `json:"gog_account,omitempty"`
	GogAuthed  bool            `json:"gog_authed,omitempty"`
}

// mcpStatusLine is the per-server MCP status: registered with the sbx gateway
// (from `sbx mcp ls`) and attach-on-run (it's in the resolved profile's mcp
// list, so `pi-stack run --mcp <name>` attaches it). Empty when sbx is
// unavailable, so status degrades to the bare MCP names.
type mcpStatusLine struct {
	Name       string `json:"name"`
	Registered bool   `json:"registered"`
	Attach     bool   `json:"attach_on_run"`
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
	//
	// DX-2: sbx being entirely absent from PATH is the SAME ambiguous signal
	// doctor sees (`sbxUnverifiableDetail`/the sbxAbsent note in doctor.go's
	// render) — it usually means "you're inside the sandbox", where sbx is
	// structurally absent and installing it here is not an available repair,
	// not "sbx is missing on the host and needs installing". status must share
	// that same perspective/action: it never presumes a host-install fix from
	// absence alone (that would only be knowable running ON the host, which
	// `pi-stack mcp register`'s own missing-sbx note already covers — see
	// mcp.go); it advises the same next step doctor gives, running `pi-stack
	// doctor` on the host, rather than a copy-paste install command that may not
	// even apply from here.
	switch {
	case !sbxOnPath:
		st.Todos = append(st.Todos, "can't verify provider keys here (sbx not on PATH, likely inside a sandbox); run `pi-stack doctor` on the host")
	case !sbxOK:
		st.Todos = append(st.Todos, "could not verify provider keys (sbx secret ls failed); check sbx")
	}

	// Knowledge bundles + git drift.
	for _, b := range cfg.KnowledgeBundles {
		st.Bundles = append(st.Bundles, bundleStatus{Path: b, Git: bundleGitStatus(env, b)})
	}

	// MCP registration state: `sbx mcp ls` once, best-effort. Each configured
	// server is attach-on-run (it's in cfg.MCP, so `run --mcp <name>` attaches it)
	// and shows whether it is currently registered with the gateway. When sbx is
	// unavailable MCPServers stays nil and render falls back to the bare names.
	if len(cfg.MCP) > 0 && env.lookPath != nil {
		if _, err := env.lookPath("sbx"); err == nil && env.run != nil {
			if o, err := env.run("sbx", "mcp", "ls"); err == nil {
				anyUnregistered := false
				for _, m := range cfg.MCP {
					reg := grepWord(o, m)
					st.MCPServers = append(st.MCPServers, mcpStatusLine{
						Name:       m,
						Registered: reg,
						Attach:     true,
					})
					if !reg {
						anyUnregistered = true
					}
				}
				// A configured server that isn't registered means `run` would attach a
				// server the gateway can't spawn — an outstanding item, so status can't
				// claim "all systems go". One deduped TODO covers all of them. Only
				// emitted when sbx is reachable (otherwise registration is unknowable).
				if anyUnregistered {
					st.Todos = append(st.Todos, "pi-stack mcp register")
				}
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
			st.Todos = append(st.Todos, "gog auth login")
		}
	}

	// Sandboxes: filter `sbx ls` to pi-stack-* boxes.
	if env.lookPath != nil {
		if _, err := env.lookPath("sbx"); err == nil && env.run != nil {
			if o, err := env.run("sbx", "ls"); err == nil {
				st.Sandboxes = parseSandboxes(o)
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
		fmt.Fprintln(out, "  knowledge   (no bundle): `pi-stack knowledge init`")
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
		for i, m := range st.MCPServers {
			label := "mcp"
			if i > 0 {
				label = "   "
			}
			reg := okGlyph(m.Registered) + " registered"
			if !m.Registered {
				reg = okGlyph(false) + " not registered"
			}
			fmt.Fprintf(out, "  %-9s   %-8s %s  %s attach-on-run\n", label, m.Name, reg, okGlyph(m.Attach))
		}
	}

	if st.GogAccount != "" {
		label := "account set, needs auth (run gog auth login)"
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
