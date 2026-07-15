package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"pi-stack/host/config"
)

// runStatusCmd is the `status` verb AND the bare-`pi-stack` landing screen: a
// fast, read-only control panel answering "what state am I in, what's my next
// move" — WITHOUT launching anything. It replaces the old footgun where bare
// `pi-stack` spun up a sandbox.
func runStatusCmd(argv []string) {
	jsonOut := false
	for _, a := range argv {
		if a == "--json" {
			jsonOut = true
		}
	}
	cfg, name, err := loadResolvedConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack status: %v\n", err)
		os.Exit(1)
	}
	renderStatus(cfg, name, defaultShellEnv(), os.Stdout, jsonOut)
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
	Providers  map[string]bool `json:"providers"`
	Bundles    []bundleStatus  `json:"knowledge_bundles"`
	MCP        []string        `json:"mcp"`
	MCPServers []mcpStatusLine `json:"mcp_servers"`
	Sandboxes  []sandboxLine   `json:"sandboxes"`
	Todos      []string        `json:"todos"`
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
	if profile == "" {
		profile = config.DefaultProfile
	}
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
	}

	// Providers: probe `sbx secret ls` once (proxy-injected keys; never in VM).
	sbxOut, sbxOK := "", false
	if env.lookPath != nil {
		if _, err := env.lookPath("sbx"); err == nil && env.run != nil {
			if o, err := env.run("sbx", "secret", "ls"); err == nil {
				sbxOut, sbxOK = o, true
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

	// Sandboxes: filter `sbx ls` to pi-stack-* boxes.
	if env.lookPath != nil {
		if _, err := env.lookPath("sbx"); err == nil && env.run != nil {
			if o, err := env.run("sbx", "ls"); err == nil {
				st.Sandboxes = parseSandboxes(o)
			}
		}
	}
	return st
}

func (st statusReport) render(out io.Writer) {
	fmt.Fprintf(out, "pi-stack %s    config: %s    profile: %s\n\n", st.Version, st.ConfigPath, st.Profile)

	serve := "down"
	if st.Memory || st.Knowledge {
		serve = "up"
	}
	fmt.Fprintf(out, "  services    memory %s :%d    knowledge %s :%d    (serve: %s)\n",
		okGlyph(st.Memory), memoryClient().Port, okGlyph(st.Knowledge), knowledgeClient().Port, serve)

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

	if len(st.Sandboxes) > 0 {
		for i, s := range st.Sandboxes {
			label := "sandboxes"
			if i > 0 {
				label = "         "
			}
			fmt.Fprintf(out, "  %s   %-24s %s\n", label, s.Name, s.State)
		}
	}

	fmt.Fprintln(out)
	if len(st.Todos) > 0 {
		fmt.Fprintf(out, "  ⚠ %s outstanding.   `pi-stack doctor` for fix commands.\n", plural(len(st.Todos), "item"))
	} else {
		fmt.Fprintln(out, "  ✓ all systems go.")
	}
	fmt.Fprintln(out, "\n  Common:  run · serve · doctor · setup · memory · knowledge · config · help")
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
