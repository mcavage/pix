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
	// ProviderEvidence is the AUTHORITATIVE tri-state behind Providers (same
	// Evidence axis doctor uses): healthy (confirmed set), failed (confirmed
	// absent, sbx probe succeeded), or unverifiable (sbx off PATH or `sbx
	// secret ls` failed -- status could not check, so it must not claim ✗).
	// Providers stays a plain bool map for JSON back-compat (true only on a
	// confirmed-healthy key); a consumer that wants to tell "verified missing"
	// from "unverifiable" reads ProviderEvidence.
	ProviderEvidence map[string]Evidence `json:"provider_evidence"`
	Bundles          []bundleStatus      `json:"knowledge_bundles"`
	MCP              []string            `json:"mcp"`
	MCPServers       []mcpStatusLine     `json:"mcp_servers"`
	Sandboxes        []sandboxLine       `json:"sandboxes"`
	Tasks            int                 `json:"tasks"`
	ArtifactB        int64               `json:"artifact_bytes"`
	Todos            []string            `json:"todos"`
	GogAccount       string              `json:"gog_account,omitempty"`
	GogAuthed        bool                `json:"gog_authed,omitempty"`
}

// mcpStatusLine is the per-server MCP status: registered with the sbx gateway
// (from `sbx mcp ls`) and Attach -- whether it is EAGER (attach-on-run: --static-mcp
// at sandbox create) as resolved by the exact same resolveStaticMCP semantics
// (mcp_static/mcp_dynamic precedence) run.go uses for launch. The DEFAULT is
// dynamic for every registered server regardless of cfg.MCP membership --
// Attach is only true when mcp_static pins it (and mcp_dynamic doesn't win
// back). Empty when sbx is unavailable, so status degrades to the bare MCP
// names.
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

// statusResolveStaticMCP computes the eager/attach-on-run set for status's MCP
// section using the EXACT same fold applyPackToLaunch (pack.go) applies at
// launch time — the active pack's `integration.mcp` names declared with
// `static = true` count as eager too, not just cfg.MCPStatic — WITHOUT any of
// applyPackToLaunch's host side effects (no skills mount, no kit synth, no
// credential warning, no cfg mutation). It loads the configured active pack
// READ-ONLY via loadPack, folds packStaticMcpNames into a SHALLOW COPY of
// cfg.MCPStatic (the real cfg.MCPStatic slice is never appended to), and lets
// resolveStaticMCP apply the same mcp_dynamic override precedence launch uses.
// A pack that fails to load (broken, symlink-rejected, genuinely absent, or a
// stale cfg.Pack pointing nowhere) degrades to cfg's OWN mcp_static/
// mcp_dynamic only — status must never falsely claim attach-on-run for an
// integration whose pack it could not actually read.
func statusResolveStaticMCP(cfg *config.Config, servers []string) []string {
	cfgCopy := *cfg
	cfgCopy.MCPStatic = append([]string(nil), cfg.MCPStatic...)
	if root := activePackRoot(cfg.Pack, ""); root != "" {
		if p, err := loadPack(root); err == nil {
			for _, n := range packStaticMcpNames(p) {
				if !containsStr(cfgCopy.MCPStatic, n) {
					cfgCopy.MCPStatic = append(cfgCopy.MCPStatic, n)
				}
			}
		}
		// A load failure (of any kind) degrades silently here: statusResolveStaticMCP
		// is a read-only best-effort render, not a launch gate — it must not error
		// out or invent an eager attach it can't back up.
	}
	return resolveStaticMCP(servers, &cfgCopy)
}

func gatherStatus(cfg *config.Config, profile string, env shellEnv) statusReport {
	memPort, knPort := memoryClient().Port, knowledgeClient().Port
	st := statusReport{
		Version:          version,
		ConfigPath:       config.Path(),
		Profile:          profile,
		Providers:        map[string]bool{},
		ProviderEvidence: map[string]Evidence{},
		MCP:              cfg.MCP,
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
	// finding #4: the runtime needs ANY ONE of the three model-provider keys
	// (anthropic/openai/google) -- mirrors doctor's modelProviderAggregateCheck.
	// An individually-missing key is never itself outstanding once at least one
	// alternative is present; only ALL THREE missing is a genuine gap, worth
	// exactly one aggregate todo (never one per missing key). GitHub is always
	// optional and must never add a todo, mirroring doctor's secretCheck.
	// Evidence is the AUTHORITATIVE tri-state (R1-03, mirrors doctor's
	// secretCheck/modelProviderKeyCheck): !sbxOK means the probe never ran, so
	// every key is unverifiable -- NOT a confirmed-absent failure. Only when
	// sbxOK is true does an absent key become a verified EvidenceFailed.
	modelKeysPresent := 0
	for _, key := range []string{"anthropic", "openai", "google"} {
		set := sbxOK && grepWord(sbxOut, key)
		st.Providers[key] = set
		switch {
		case !sbxOK:
			st.ProviderEvidence[key] = EvidenceUnverifiable
		case set:
			st.ProviderEvidence[key] = EvidenceHealthy
			modelKeysPresent++
		default:
			st.ProviderEvidence[key] = EvidenceFailed
		}
	}
	githubSet := sbxOK && grepWord(sbxOut, "github")
	st.Providers["github"] = githubSet
	switch {
	case !sbxOK:
		st.ProviderEvidence["github"] = EvidenceUnverifiable
	case githubSet:
		st.ProviderEvidence["github"] = EvidenceHealthy
	default:
		st.ProviderEvidence["github"] = EvidenceFailed
	}
	if sbxOK && modelKeysPresent == 0 {
		st.Todos = append(st.Todos, "sbx secret set -g anthropic")
	}
	// When sbx could NOT verify keys every provider renders ⚠ unverifiable (never
	// a confirmed ✗) but no per-key TODO is added -- so without an outstanding
	// item the verdict would be falsely "all systems go". Distinguish the two
	// failure modes: sbx not installed at all vs installed-but-the-probe-failed.
	// Not emitted when sbxOK is true (the per-key TODOs cover that case).
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

	// MCP registration state: `sbx mcp ls` once, best-effort. Attach is resolved
	// via resolveStaticMCP -- the SAME eager-vs-lazy semantics (mcp_static/
	// mcp_dynamic precedence) run.go uses at launch, never re-derived from bare
	// cfg.MCP membership (the DEFAULT is dynamic for every registered server).
	// Each entry also shows whether it is currently registered with the gateway.
	// When sbx is unavailable MCPServers stays nil and render falls back to the
	// bare names.
	if len(cfg.MCP) > 0 && env.lookPath != nil {
		if _, err := env.lookPath("sbx"); err == nil && env.run != nil {
			if o, err := env.run("sbx", "mcp", "ls"); err == nil {
				eager := map[string]bool{}
				for _, n := range statusResolveStaticMCP(cfg, cfg.MCP) {
					eager[n] = true
				}
				// finding #1: an unregistered server's fix-it command depends on
				// what KIND of server it is (mirrors doctor's classifyMCP) — a
				// confirmed LOCAL stdio server needs `pi-stack mcp register`; a
				// confirmed REMOTE gateway-catalog server needs `pi-stack mcp
				// bundle` instead (register only knows local servers); gog is
				// always the local special case (see mcp.go); and when the
				// classification itself is unknown, status must not guess — no
				// todo for that name at all, same posture as doctor's
				// mcpUnknownClassificationCheck. A confirmed CUSTOM name (non-local,
				// also outside the shipped catalog) gets its OWN honest guidance:
				// neither `pi-stack mcp register` nor `pi-stack mcp bundle` can
				// register it (the final false-green regression -- a
				// confirmed-missing custom server must still be an outstanding item,
				// pointed at native `sbx mcp add` with the server's OWN url/transport,
				// never a guessed URL or unsafe placeholder command).
				localSet, localKnown := localMCPNames(env, env.hostBinary)
				anyUnregisteredLocal := false
				var remoteTodos []string
				var customTodos []string
				seenRemoteTodo := map[string]bool{}
				for _, m := range cfg.MCP {
					reg := grepWord(o, m)
					st.MCPServers = append(st.MCPServers, mcpStatusLine{
						Name:       m,
						Registered: reg,
						Attach:     eager[m],
					})
					if reg {
						continue
					}
					switch {
					case m == "gog":
						anyUnregisteredLocal = true
					case classifyMCP(m, localSet, localKnown) == mcpClassLocal:
						anyUnregisteredLocal = true
					case classifyMCP(m, localSet, localKnown) == mcpClassRemote:
						const cmd = "pi-stack mcp bundle"
						if !seenRemoteTodo[cmd] {
							seenRemoteTodo[cmd] = true
							remoteTodos = append(remoteTodos, cmd)
						}
					case classifyMCP(m, localSet, localKnown) == mcpClassCustom:
						// Confirmed non-local AND outside the shipped catalog: `pi-stack
						// mcp bundle` only registers mcpCatalogNames (a silent no-op here)
						// and `pi-stack mcp register` only knows local stdio servers, so
						// neither applies -- but this is still a CONFIRMED absence, so it
						// must still be an outstanding item (never a false "all systems
						// go"). The guidance names native `sbx mcp add` with the
						// server's OWN url/transport rather than inventing one.
						customTodos = append(customTodos,
							"sbx mcp add "+m+" --url <the server's own URL> --transport <its transport>")
						// default (mcpClassUnknown): classification itself failed --
						// genuinely can't tell how to register, so no todo at all, same
						// posture as doctor's mcpUnknownClassificationCheck.
					}
				}
				// A configured server that isn't registered means `run` would attach a
				// server the gateway can't spawn — an outstanding item, so status can't
				// claim "all systems go". One deduped TODO per kind covers all of them.
				// Only emitted when sbx is reachable (otherwise registration is
				// unknowable).
				if anyUnregisteredLocal {
					st.Todos = append(st.Todos, "pi-stack mcp register")
				}
				st.Todos = append(st.Todos, remoteTodos...)
				st.Todos = append(st.Todos, customTodos...)
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
			st.Todos = append(st.Todos, "pi-stack gog setup")
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
		prov = append(prov, fmt.Sprintf("%s %s", k, evidenceGlyph(st.ProviderEvidence[k])))
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
			// Attach mirrors resolveStaticMCP: eager (mcp_static-pinned) renders
			// attach-on-run; the DEFAULT dynamic renders as dynamically
			// discoverable -- never a ✗, since lazy is the working-as-intended
			// default, not a failure.
			attach := "dynamically discoverable (mcp-find/mcp-exec on demand)"
			if m.Attach {
				attach = okGlyph(true) + " attach-on-run (eager, pinned via mcp_static)"
			}
			fmt.Fprintf(out, "  %-9s   %-8s %s  %s\n", label, m.Name, reg, attach)
		}
	}

	if st.GogAccount != "" {
		label := "account set, needs auth (run pi-stack gog setup)"
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

// evidenceGlyph renders a provider's Evidence tri-state (R1-03, same axis as
// doctor's check.state): healthy -> ✓, a verified failure -> ✗, unverifiable
// -> ⚠ (never ✗ -- that would misreport "could not check" as "confirmed
// missing"). Any other/unset value degrades to the unverifiable glyph rather
// than a false ✗.
func evidenceGlyph(ev Evidence) string {
	switch ev {
	case EvidenceHealthy:
		return "✓"
	case EvidenceFailed:
		return "✗"
	default: // EvidenceUnverifiable, EvidenceNotConfigured, or unset
		return "⚠"
	}
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
