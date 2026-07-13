package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"pi-stack/host/config"
)

// doctor ports the Makefile `doctor:` target into Go. Unlike the shell version
// it LEADS WITH A ONE-LINE VERDICT, then details the checks grouped in
// dependency order (keys -> ollama/models -> memory -> gws -> mcp), keeping the
// copy-pasteable `TODO: <exact command>` lines for anything not set up.
//
// It must RUN cleanly inside the sandbox, where sbx and ollama are absent: every
// probe degrades to a sane TODO rather than crashing. All the OS-touching work
// goes through a shellEnv of function values so the tests drive it hermetically.

// shellEnv abstracts the three ways doctor/setup touch the host: locating a
// binary, running a command for its output, and dialing a local TCP port.
// Tests substitute fakes; defaultShellEnv() wires the real thing.
type shellEnv struct {
	lookPath func(name string) (string, error)
	run      func(name string, args ...string) (string, error)
	dial     func(port int) bool
}

// defaultShellEnv returns a shellEnv backed by the real OS.
func defaultShellEnv() shellEnv {
	return shellEnv{
		lookPath: exec.LookPath,
		run: func(name string, args ...string) (string, error) {
			out, err := exec.Command(name, args...).CombinedOutput()
			return string(out), err
		},
		dial: func(port int) bool {
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 400*time.Millisecond)
			if err != nil {
				return false
			}
			_ = conn.Close()
			return true
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

	// (d) gws-token service on :11441 + (e) the gws CLI.
	gws := group{title: "gws (Google Workspace data)"}
	gwsUp := env.dial(11441)
	gws.checks = append(gws.checks, serviceCheck("gws-token", 11441, gwsUp, "pi-stack serve", enabled(cfg, "gws")))
	if _, err := env.lookPath("gws"); err == nil {
		gws.checks = append(gws.checks, check{label: "gws CLI", state: stateOK, detail: "installed"})
	} else {
		gws.checks = append(gws.checks, check{
			label:  "gws CLI",
			state:  stateTODO,
			detail: "not found",
			todo:   "install gws + run `gws auth login`",
		})
	}
	r.groups = append(r.groups, gws)

	// (f) MCP servers registered with sbx.
	mcp := group{title: "MCP servers (local stdio, run by the sbx gateway)"}
	mcpOut, mcpOK := "", false
	if sbxOK {
		if out, err := env.run("sbx", "mcp", "ls"); err == nil {
			mcpOut, mcpOK = out, true
		}
	}
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
