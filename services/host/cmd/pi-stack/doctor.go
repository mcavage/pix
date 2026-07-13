package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"pi-stack/host/config"
)

// doctor ports the Makefile `doctor:` target into Go. Unlike the shell version
// it LEADS WITH A ONE-LINE VERDICT, then details the checks grouped in
// dependency order (keys -> ollama/models -> memory -> gog -> mcp), keeping the
// copy-pasteable `TODO: <exact command>` lines for anything not set up.
//
// It must RUN cleanly inside the sandbox, where sbx and ollama are absent: every
// probe degrades to a sane TODO rather than crashing. All the OS-touching work
// goes through a shellEnv of function values so the tests drive it hermetically.

// shellEnv abstracts the ways doctor/setup touch the host: locating a binary,
// running a command for its output, reading an env var, and dialing a local TCP
// port. Tests substitute fakes; defaultShellEnv() wires the real thing.
type shellEnv struct {
	lookPath func(name string) (string, error)
	run      func(name string, args ...string) (string, error)
	getenv   func(name string) string
	dial     func(port int) bool
	statFile func(path string) bool            // does a regular file exist at path?
	readFile func(path string) (string, error) // read a file's contents
	homeDir  func() string                     // the user's home directory ($HOME)
}

// defaultShellEnv returns a shellEnv backed by the real OS.
func defaultShellEnv() shellEnv {
	return shellEnv{
		lookPath: exec.LookPath,
		run: func(name string, args ...string) (string, error) {
			out, err := exec.Command(name, args...).CombinedOutput()
			return string(out), err
		},
		getenv: os.Getenv,
		dial: func(port int) bool {
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 400*time.Millisecond)
			if err != nil {
				return false
			}
			_ = conn.Close()
			return true
		},
		statFile: func(path string) bool {
			info, err := os.Stat(path)
			return err == nil && !info.IsDir()
		},
		readFile: func(path string) (string, error) {
			b, err := os.ReadFile(path)
			return string(b), err
		},
		homeDir: func() string {
			h, _ := os.UserHomeDir()
			return h
		},
	}
}

// checkState is the rendered status of a single check.
type checkState int

const (
	stateOK   checkState = iota // set up / running
	stateTODO                   // needs action; carries an exact command
	stateInfo                   // informational, no action implied
)

// check is one line in a doctor group.
type check struct {
	label  string
	state  checkState
	detail string // short human note after the label
	todo   string // exact copy-pasteable command when state == stateTODO
}

// group is a titled cluster of checks in dependency order.
type group struct {
	title  string
	checks []check
}

// report is the full doctor result: an ordered set of groups. It knows how to
// count outstanding TODOs (for the verdict) and render itself.
type report struct {
	groups    []group
	sbxAbsent bool     // sbx not on PATH — provider/mcp checks can't be verified here
	services  []string // configured SERVICES, for the footer
	mcp       []string // configured MCP, for the footer
}

// todos returns every outstanding TODO command across all groups, in order.
func (r *report) todos() []string {
	var out []string
	for _, g := range r.groups {
		for _, c := range g.checks {
			if c.state == stateTODO && c.todo != "" {
				out = append(out, c.todo)
			}
		}
	}
	return out
}

// runDoctor builds the report. Pure apart from env: no direct OS access, so the
// tests feed a faked shellEnv and assert on the rendered output.
func runDoctor(cfg *config.Config, env shellEnv) *report {
	r := &report{}

	// sbx presence gates the provider + mcp checks (they read `sbx secret ls` /
	// `sbx mcp ls`). Inside the sandbox sbx is absent — say so, don't crash.
	sbxOut, sbxOK := "", false
	if _, err := env.lookPath("sbx"); err == nil {
		if out, err := env.run("sbx", "secret", "ls"); err == nil {
			sbxOut, sbxOK = out, true
		}
	}
	r.sbxAbsent = !sbxOK

	// MCP registrations (`sbx mcp ls`), listed once and reused by the gog group
	// (its gateway registration) and the MCP group below.
	mcpOut, mcpOK := "", false
	if sbxOK {
		if out, err := env.run("sbx", "mcp", "ls"); err == nil {
			mcpOut, mcpOK = out, true
		}
	}

	// (a) provider secrets — proxy-injected, never in the VM.
	providers := group{title: "Providers / keys (proxy-injected, never in the VM)"}
	for _, p := range []struct{ label, key string }{
		{"anthropic", "anthropic"},
		{"openai", "openai"},
		{"google", "google"},
		{"github", "github"},
	} {
		providers.checks = append(providers.checks, secretCheck(p.label, p.key, sbxOut, sbxOK))
	}
	r.groups = append(r.groups, providers)

	// (b) ollama + the configured watcher/embed models.
	ollama := group{title: "Ollama / local models (optional: fact capture + semantic recall)"}
	ollamaInstalled := false
	if _, err := env.lookPath("ollama"); err == nil {
		ollamaInstalled = true
		up := env.dial(11434)
		ollama.checks = append(ollama.checks, check{
			label:  "ollama",
			state:  stateOK,
			detail: "installed, :11434 " + upDown(up),
		})
	} else {
		ollama.checks = append(ollama.checks, check{
			label:  "ollama",
			state:  stateTODO,
			detail: "not installed",
			todo:   "install ollama — https://ollama.com",
		})
	}
	// List models once, reuse for both watcher + embed.
	modelOut, modelOK := "", false
	if ollamaInstalled {
		if out, err := env.run("ollama", "list"); err == nil {
			modelOut, modelOK = out, true
		}
	}
	ollama.checks = append(ollama.checks,
		modelCheck("watcher", cfg.MemoryWatcherModel, "fact capture", ollamaInstalled, modelOut, modelOK),
		modelCheck("embed", cfg.MemoryEmbedModel, "semantic recall", ollamaInstalled, modelOut, modelOK),
	)
	r.groups = append(r.groups, ollama)

	// (c) memory service on :11435.
	memory := group{title: "Memory service (recall + capture)"}
	memUp := env.dial(11435)
	memory.checks = append(memory.checks, serviceCheck("memory", 11435, memUp, "pi-stack serve", enabled(cfg, "memory")))
	r.groups = append(r.groups, memory)

	// (d) gog: Google Workspace via a host-side stdio MCP server the sbx gateway
	// spawns (the slack pattern). No CLI in the VM, no token service, no bearer.
	// Checks run in strict dependency order and DELIBERATELY probe the REAL path
	// the gateway uses (headless, through `op run --env-file=config/op-refs.env`),
	// because `gog auth doctor` in a logged-in shell passes and lies.
	r.groups = append(r.groups, gogGroup(cfg, env, mcpOut, mcpOK))

	// (e) MCP servers registered with sbx.
	mcp := group{title: "MCP servers (local stdio, run by the sbx gateway)"}
	if len(cfg.MCP) == 0 {
		mcp.checks = append(mcp.checks, check{
			label:  "(none configured)",
			state:  stateInfo,
			detail: "add servers to `mcp` in " + config.Path(),
		})
	} else {
		for _, m := range cfg.MCP {
			mcp.checks = append(mcp.checks, mcpCheck(m, mcpOut, mcpOK))
		}
	}
	r.groups = append(r.groups, mcp)

	return r
}

// secretCheck reports whether a provider secret is set. When sbx is
// unreachable (e.g. inside the sandbox) it emits a TODO rather than a false OK.
func secretCheck(label, key, sbxOut string, sbxOK bool) check {
	cmd := "sbx secret set -g " + key
	if !sbxOK {
		return check{label: label, state: stateTODO, detail: "sbx unavailable here (set on the host)", todo: cmd}
	}
	if grepWord(sbxOut, key) {
		return check{label: label, state: stateOK, detail: "set"}
	}
	return check{label: label, state: stateTODO, detail: "not set", todo: cmd}
}

// modelCheck reports whether an ollama model is pulled.
func modelCheck(role, model, purpose string, ollamaInstalled bool, listOut string, listOK bool) check {
	label := "  " + role
	detail := purpose + " [" + model + "]"
	cmd := "ollama pull " + model
	if !ollamaInstalled {
		return check{label: label, state: stateTODO, detail: detail + " — needs ollama", todo: cmd}
	}
	if listOK && modelPulled(listOut, model) {
		return check{label: label, state: stateOK, detail: "pulled — " + detail}
	}
	return check{label: label, state: stateTODO, detail: detail + " — not pulled", todo: cmd}
}

// serviceCheck reports a host service's port state. A down service that is in
// the configured SERVICES set gets a `pi-stack serve` TODO; one that isn't
// enabled is merely informational.
func serviceCheck(label string, port int, up bool, startCmd string, isEnabled bool) check {
	if up {
		return check{label: label, state: stateOK, detail: fmt.Sprintf(":%d up", port)}
	}
	if isEnabled {
		return check{label: label, state: stateTODO, detail: fmt.Sprintf(":%d down", port), todo: startCmd}
	}
	return check{label: label, state: stateInfo, detail: fmt.Sprintf(":%d down (not in configured services)", port)}
}

// mcpCheck reports whether an MCP server is registered with sbx.
func mcpCheck(name, mcpOut string, mcpOK bool) check {
	cmd := "pi-stack mcp register (or `make mcp-register`)"
	if !mcpOK {
		return check{label: name, state: stateTODO, detail: "sbx unavailable here (register on the host)", todo: cmd}
	}
	if grepWord(mcpOut, name) {
		return check{label: name, state: stateOK, detail: "registered"}
	}
	return check{label: name, state: stateTODO, detail: "not registered", todo: cmd}
}

// enabled reports whether a service name is in the configured SERVICES set.
func enabled(cfg *config.Config, name string) bool {
	for _, s := range cfg.Services {
		if s == name {
			return true
		}
	}
	return false
}

// mcpConfigured reports whether name is in the configured MCP set (so `run`
// auto-attaches it via --mcp).
func mcpConfigured(cfg *config.Config, name string) bool {
	for _, m := range cfg.MCP {
		if m == name {
			return true
		}
	}
	return false
}

// gogAccount resolves the Google Workspace account doctor/setup probe against,
// in the SAME precedence order that ends at what `make mcp-register` uses, so a
// green doctor can't mean "probed a different account than the gateway got":
//  1. config.toml's `gog_account` (the Go-side source of truth, cfg.GogAccount),
//  2. the $GOG_ACCOUNT env var,
//  3. GOG_ACCOUNT parsed out of a located config/local.mk — exactly the value
//     `make mcp-register` registers with the gateway.
//
// NEVER a hardcoded address. Empty means "not configured" and the caller emits a
// "cannot verify" TODO rather than reporting green.
func gogAccount(cfg *config.Config, env shellEnv) string {
	if cfg != nil {
		if a := strings.TrimSpace(cfg.GogAccount); a != "" {
			return a
		}
	}
	if env.getenv != nil {
		if a := strings.TrimSpace(env.getenv("GOG_ACCOUNT")); a != "" {
			return a
		}
	}
	return gogAccountFromLocalMk(env)
}

// gogAccountRe matches a `GOG_ACCOUNT = <val>` / `:= ` / `?= ` assignment in a
// Makefile-style config/local.mk (the same var `make mcp-register` reads).
var gogAccountRe = regexp.MustCompile(`(?m)^\s*GOG_ACCOUNT\s*[:?]?=\s*(\S+)`)

// gogAccountFromLocalMk locates a repo checkout's config/local.mk and greps
// GOG_ACCOUNT out of it, so doctor matches the make-side value even when nothing
// is set in config.toml or the env. Returns "" when no local.mk is found or the
// var is unset — never crashes.
func gogAccountFromLocalMk(env shellEnv) string {
	if env.readFile == nil {
		return ""
	}
	path := findUpward(env, filepath.Join("config", "local.mk"))
	if path == "" {
		return ""
	}
	data, err := env.readFile(path)
	if err != nil {
		return ""
	}
	if m := gogAccountRe.FindStringSubmatch(data); len(m) >= 2 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// findUpward walks up from the current working directory looking for a directory
// that contains BOTH a Makefile and the given repo-relative file, returning the
// absolute path to that file (or "" if none is found before the filesystem root).
// This is how doctor locates a repo checkout's config files (op-refs.env,
// local.mk) regardless of where it was invoked from within the tree.
func findUpward(env shellEnv, rel string) string {
	if env.statFile == nil {
		return ""
	}
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		if env.statFile(filepath.Join(dir, "Makefile")) && env.statFile(filepath.Join(dir, rel)) {
			return filepath.Join(dir, rel)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// resolveOpRefs resolves config/op-refs.env to an ABSOLUTE, canonical location
// so doctor's headless probe matches the gateway registration exactly (`make
// mcp-register` registers the gog spawn with an absolute --env-file; a relative
// one here would resolve against doctor's cwd and could probe a different file
// than the gateway actually uses). It searches, in order, and returns the FIRST
// that exists:
//  1. $PI_STACK_CONFIG's directory + op-refs.env,
//  2. a repo checkout's config/op-refs.env (walk up for Makefile + that file),
//  3. ~/.config/pi-stack/op-refs.env.
//
// Returns "" when none exists, so the caller reports "cannot verify" rather than
// probing (and blessing) a file the gateway never uses.
func resolveOpRefs(env shellEnv) string {
	if env.getenv != nil {
		if p := env.getenv("PI_STACK_CONFIG"); p != "" {
			cand := filepath.Join(filepath.Dir(p), "op-refs.env")
			if env.statFile != nil && env.statFile(cand) {
				return cand
			}
		}
	}
	if p := findUpward(env, filepath.Join("config", "op-refs.env")); p != "" {
		return p
	}
	if env.homeDir != nil && env.statFile != nil {
		if home := env.homeDir(); home != "" {
			cand := filepath.Join(home, ".config", "pi-stack", "op-refs.env")
			if env.statFile(cand) {
				return cand
			}
		}
	}
	return ""
}

// gogHeadlessOK runs the gateway-EQUIVALENT probe — list gog's tools the exact
// way the sbx gateway spawns it: headless, in a bare env, through the same
// `op run --env-file=config/op-refs.env` wrapper mcp-register uses — and reports
// whether it yields a NON-EMPTY tool list. This is the ONLY check that proves
// the real path; `gog auth doctor` in a logged-in shell passes and lies. It
// degrades cleanly (returns false, never crashes) when gog/op/account are
// absent. shellEnv keeps it unit-testable.
func gogHeadlessOK(env shellEnv, acct, opRefs string) bool {
	if acct == "" || opRefs == "" {
		return false
	}
	if _, err := env.lookPath("op"); err != nil {
		return false
	}
	out, err := env.run("op", "run", "--env-file="+opRefs, "--",
		"gog", "--account", acct, "mcp", "--list-tools")
	return err == nil && strings.TrimSpace(out) != ""
}

// gogGroup builds the gog check cluster in strict dependency order, naming the
// one footgun by name: auth that works in your shell but returns 0 tools on the
// gateway's headless spawn because GOG_KEYRING_BACKEND/PASSWORD (+ GOG_ACCOUNT/
// GOG_HOME) never reached config/op-refs.env. Every probe degrades to a TODO
// rather than crashing, so this runs cleanly in-sandbox (gog/sbx/op all absent).
func gogGroup(cfg *config.Config, env shellEnv, mcpOut string, mcpOK bool) group {
	g := group{title: "gog (Google Workspace via host MCP — read-only)"}

	// 1. gog CLI installed.
	if _, err := env.lookPath("gog"); err != nil {
		g.checks = append(g.checks, check{label: "gog CLI", state: stateTODO,
			detail: "not found", todo: "brew install gog"})
		return g
	}
	g.checks = append(g.checks, check{label: "gog CLI", state: stateOK, detail: "installed"})

	acct := gogAccount(cfg, env)
	opRefs := resolveOpRefs(env)

	// TRANSPARENCY: name EXACTLY what this probe is verifying — which account and
	// which op-refs file — so a green result can never silently mean "checked a
	// different account/path than the sbx gateway actually registered".
	acctShown, refsShown := acct, opRefs
	if acctShown == "" {
		acctShown = "<unknown>"
	}
	if refsShown == "" {
		refsShown = "<not found>"
	}
	g.checks = append(g.checks,
		check{label: "verifying", state: stateInfo, detail: "gog for " + acctShown + " via " + refsShown},
		check{label: "note", state: stateInfo,
			detail: "must match your `make mcp-register` (config/local.mk GOG_ACCOUNT + config/op-refs.env)"})

	if acct == "" {
		// 2'. No account configured — can't probe auth or the headless path, so we
		// must NOT report green: say we cannot verify and name the two sources.
		g.checks = append(g.checks, check{label: "account", state: stateTODO,
			detail: "cannot verify (GOG_ACCOUNT unset in env/config/local.mk)",
			todo:   "set gog_account in " + config.Path() + " (or GOG_ACCOUNT in config/local.mk) so doctor probes the right account"})
		g.checks = append(g.checks, mcpCheck("gog", mcpOut, mcpOK))
		g.checks = append(g.checks, gogAttachCheck(cfg))
		return g
	}

	if opRefs == "" {
		// Can't run the gateway-equivalent headless probe without op-refs.env — so
		// we must NOT report green: say we cannot verify and name the fix.
		g.checks = append(g.checks,
			check{label: "account", state: stateOK, detail: acct + " set"},
			check{label: "op-refs", state: stateTODO,
				detail: "cannot verify (config/op-refs.env not found)",
				todo:   "create config/op-refs.env (cp config/op-refs.env.example config/op-refs.env) so doctor can probe the gateway path"})
		g.checks = append(g.checks, mcpCheck("gog", mcpOut, mcpOK))
		g.checks = append(g.checks, gogAttachCheck(cfg))
		return g
	}

	// 2. account authorized (interactive). 3. THE GOTCHA — headless spawn.
	_, interErr := env.run("gog", "--account", acct, "auth", "doctor", "--check")
	_, opErr := env.lookPath("op")
	headOK := gogHeadlessOK(env, acct, opRefs)
	switch {
	case interErr != nil:
		// Auth itself isn't set up — don't double-report the keyring below.
		g.checks = append(g.checks, check{label: "account", state: stateTODO,
			detail: acct + " not authorized",
			todo:   "gog auth add-client <client.json> && gog --account " + acct + " auth login"})
	case opErr != nil:
		// Interactive auth OK, but op is absent so we can't run the gateway-
		// equivalent probe. Say so rather than blaming the keyring.
		g.checks = append(g.checks,
			check{label: "account", state: stateOK, detail: acct + " authorized (interactive)"},
			check{label: "headless spawn", state: stateTODO,
				detail: "can't verify the gateway spawn — op (1Password CLI) not found",
				todo:   "install the 1Password CLI (op) so doctor can probe the real headless path"})
	case !headOK:
		// THE TRAP: interactive passes, the headless gateway spawn gets 0 tools.
		g.checks = append(g.checks,
			check{label: "account", state: stateOK, detail: acct + " authorized (interactive)"},
			check{label: "headless spawn", state: stateTODO,
				detail: "auth OK in your shell but the gateway spawn gets 0 tools — keyring not headless",
				todo:   "add GOG_KEYRING_BACKEND=file + GOG_KEYRING_PASSWORD + GOG_ACCOUNT + GOG_HOME to config/op-refs.env"})
	default:
		g.checks = append(g.checks,
			check{label: "account", state: stateOK, detail: acct + " authorized"},
			check{label: "headless spawn", state: stateOK, detail: "tools exposed via headless keyring"})
	}

	// 4. registered with the gateway. 5. attached on run?
	g.checks = append(g.checks, mcpCheck("gog", mcpOut, mcpOK))
	g.checks = append(g.checks, gogAttachCheck(cfg))
	return g
}

// gogAttachCheck is the informational check 5: is gog in the configured MCP set,
// so `pi-stack run` auto-attaches it (--mcp gog)?
func gogAttachCheck(cfg *config.Config) check {
	if mcpConfigured(cfg, "gog") {
		return check{label: "attached", state: stateInfo, detail: "auto-attached on run (--mcp gog)"}
	}
	return check{label: "attached", state: stateInfo,
		detail: `add "gog" to mcp in ` + config.Path() + " to attach it"}
}

// render writes the verdict-first report to w.
func (r *report) render(w io.Writer) {
	todos := r.todos()

	// One-line verdict up front — the whole point of the Go rewrite.
	if len(todos) == 0 {
		fmt.Fprintln(w, "✓ pi-stack: all checks pass — you're ready to `pi-stack serve` + `pi-stack`.")
	} else {
		fmt.Fprintf(w, "⚠ pi-stack: %s outstanding — see the TODOs below.\n", plural(len(todos), "item"))
	}
	if r.sbxAbsent {
		fmt.Fprintln(w, "  note: sbx not on PATH (you're likely inside the sandbox) — provider/MCP")
		fmt.Fprintln(w, "        checks can't be verified here; run `pi-stack doctor` on the host.")
	}
	fmt.Fprintln(w)

	for _, g := range r.groups {
		fmt.Fprintf(w, "%s:\n", g.title)
		for _, c := range g.checks {
			fmt.Fprintf(w, "  %s %-12s %s\n", glyph(c.state), c.label, c.detail)
		}
		fmt.Fprintln(w)
	}

	if len(todos) > 0 {
		fmt.Fprintln(w, "TODO (copy-paste, in dependency order):")
		for _, t := range todos {
			fmt.Fprintf(w, "  TODO: %s\n", t)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "Config: %s   (services=%s, mcp=%s)\n",
		config.Path(), strings.Join(r.cfgServices(), " "), r.cfgMCP())
}

// cfgServices / cfgMCP are filled by runDoctor via closure-free re-derivation;
// keep them on the report so render stays config-free. Stored at build time.
func (r *report) cfgServices() []string { return r.services }
func (r *report) cfgMCP() string {
	if len(r.mcp) == 0 {
		return "<none>"
	}
	return strings.Join(r.mcp, " ")
}

func glyph(s checkState) string {
	switch s {
	case stateOK:
		return "✓"
	case stateTODO:
		return "✗"
	default:
		return "·"
	}
}

func upDown(up bool) string {
	if up {
		return "up"
	}
	return "down"
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// grepWord reports whether out contains name as a whole word (matches the
// Makefile's `grep -qw`).
func grepWord(out, name string) bool {
	for _, line := range strings.Split(out, "\n") {
		for _, f := range strings.FieldsFunc(line, func(r rune) bool {
			return r == ' ' || r == '\t' || r == ',' || r == ':' || r == '/' || r == '"' || r == '='
		}) {
			if f == name {
				return true
			}
		}
	}
	return false
}

// modelPulled reports whether `ollama list` output lists the given model. The
// first column may carry a :tag suffix (e.g. "gemma4:latest").
func modelPulled(listOut, model string) bool {
	for _, line := range strings.Split(listOut, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		name := fields[0]
		if name == model || strings.HasPrefix(name, model+":") {
			return true
		}
	}
	return false
}

// runDoctorCmd is the CLI entry point wired into main's dispatch.
func runDoctorCmd(argv []string) {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack doctor: loading config: %v\n", err)
		os.Exit(1)
	}
	r := runDoctor(cfg, defaultShellEnv())
	r.services = cfg.Services
	r.mcp = cfg.MCP
	r.render(os.Stdout)
}
