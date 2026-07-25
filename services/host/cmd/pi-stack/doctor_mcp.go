package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"pi-stack/host/config"
)

// gatewayDownDetail / gatewayTODO describe the HOST condition where sbx IS
// present (secret ls succeeded) but `sbx mcp ls` failed. The MCP gateway is now
// the local data-plane one (always available, no SBX_MCP_URL), so a failed
// listing means the sbx daemon/gateway is unhealthy, not "gateway off". This is
// NOT "sbx unavailable": the CLI is here, only the MCP-registration listing failed.
const (
	gatewayDownDetail = "sbx present but couldn't list MCP registrations — check the sbx daemon (sbx mcp status; sbx daemon status)"
	gatewayTODO       = "check the sbx MCP gateway: run `sbx mcp status` and `sbx daemon status`, then re-run doctor"
)

// mcpCheck reports whether an MCP server is registered with sbx. When the
// sandbox — a register-on-the-host TODO) from sbx being PRESENT but the listing
// having failed (host, sbx daemon/gateway likely unhealthy — a check-the-daemon TODO).
func mcpCheck(name, mcpOut string, mcpOK, sbxPresent bool) check {
	cmd := "pi-stack mcp register"
	if !mcpOK {
		if sbxPresent {
			return check{label: name, verdict: verdictTodo, detail: gatewayDownDetail, todo: gatewayTODO}
		}
		return check{label: name, verdict: verdictTodo, detail: "sbx unavailable here (register on the host)", todo: cmd}
	}
	if grepWord(mcpOut, name) {
		return check{label: name, verdict: verdictReady, detail: "registered"}
	}
	return check{label: name, verdict: verdictTodo, detail: "not registered", todo: cmd}
}

// mcpProbeCheck is the HONEST, generalized MCP check: for every configured local
// stdio server (slack, an overlay `pio`/`fastmail`, …), not just gog, it reports
// registered -> spawns -> returns N tools. It reads the command sbx ACTUALLY
// registered for <name> and probes THAT (the same honest path the gog group
// uses), so a pass proves the real gateway spawn, not a config reconstruction.
// It degrades cleanly: sbx absent -> a register TODO; registered but no readable
// command / no --list-tools support -> a confirmed "registered" without the tool
// count (never a false TODO); registered but 0 tools -> a TODO naming the
// headless-creds fix (the same trap the gog headless-spawn check catches).
func mcpProbeCheck(env shellEnv, name, mcpOut string, mcpOK, sbxPresent bool) check {
	cmd := "pi-stack mcp register"
	if !mcpOK {
		if sbxPresent {
			return check{label: name, verdict: verdictTodo, detail: gatewayDownDetail, todo: gatewayTODO}
		}
		return check{label: name, verdict: verdictTodo, detail: "sbx unavailable here (register on the host)", todo: cmd}
	}
	if !grepWord(mcpOut, name) {
		return check{label: name, verdict: verdictTodo, detail: "not registered", todo: cmd}
	}
	// Registered — try the honest headless probe of the registered command.
	argv, ok := registeredMCPCommand(env, name)
	if !ok {
		return check{label: name, verdict: verdictReady, detail: "registered (tool probe unavailable)"}
	}
	// SAFETY: only exec a command whose SHAPE we trust. sbx will hand us whatever
	// argv someone registered for <name>; doctor must not blindly run it. If it is
	// not the known gog form or a `pi-stack-host mcp <name>` spawn, skip the probe
	// (still a confirmed registration, just no tool count) rather than exec it.
	if !recognizedMCPArgv(argv, name) {
		return check{label: name, verdict: verdictReady, detail: "registered (probe skipped: unrecognized command)"}
	}
	n, probed := probeRegisteredMCP(env, argv)
	if !probed {
		return check{label: name, verdict: verdictReady, detail: "registered (tool probe unavailable)"}
	}
	if n == 0 {
		return check{label: name, verdict: verdictTodo,
			detail: "registered but the spawned command returns 0 tools — headless creds/keyring",
			todo:   "review the registered command: sbx mcp get " + name}
	}
	return check{label: name, verdict: verdictReady, detail: fmt.Sprintf("registered, spawns %s", plural(n, "tool"))}
}

// registeredMCPCommand is the generalized sibling of registeredGogCommand: it
// asks sbx for the command ACTUALLY registered for <name>, so doctor can probe
// the real registration for any local stdio server. It tries `sbx mcp get
// <name>` then `sbx mcp ls -o json`, returning the parsed argv. Unlike the gog
// parsers it applies no gog-specific completeness bar — any non-empty,
// unambiguous (unquoted) command counts. Returns (nil,false) when sbx is absent
// or exposes no command; the caller then reports "registered" without a tool
// count rather than a false TODO.
func registeredMCPCommand(env shellEnv, name string) ([]string, bool) {
	if env.lookPath == nil || env.run == nil {
		return nil, false
	}
	if _, err := env.lookPath("sbx"); err != nil {
		return nil, false
	}
	if out, err := env.run("sbx", "mcp", "get", name); err == nil {
		if argv, ok := parseMCPCommandLine(out); ok {
			return argv, true
		}
	}
	if out, err := env.run("sbx", "mcp", "ls", "-o", "json"); err == nil {
		if argv, ok := parseMCPCommandJSON(out, name); ok {
			return argv, true
		}
	}
	return nil, false
}

// parseMCPCommandLine extracts a registered argv from a `sbx mcp get <name>`
// text dump: the `command:` line split into fields. A shell-quoted line (which
// strings.Fields cannot split reliably) or an empty command returns (nil,false)
// so registeredMCPCommand falls through to the structured JSON parser.
func parseMCPCommandLine(out string) ([]string, bool) {
	m := gogCommandLineRe.FindStringSubmatch(out)
	if len(m) < 2 {
		return nil, false
	}
	cmd := strings.TrimSpace(m[1])
	if cmd == "" || strings.ContainsAny(cmd, "\"'") {
		return nil, false
	}
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return nil, false
	}
	return fields, true
}

// parseMCPCommandJSON extracts the registered argv for <name> from `sbx mcp ls
// -o json` (an array of {name, command, args}). Returns (nil,false) when there
// is no matching entry or the JSON doesn't parse.
func parseMCPCommandJSON(out, name string) ([]string, bool) {
	var servers []struct {
		Name    string   `json:"name"`
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	if err := json.Unmarshal([]byte(out), &servers); err != nil {
		return nil, false
	}
	for _, s := range servers {
		if s.Name != name || strings.TrimSpace(s.Command) == "" {
			continue
		}
		return append([]string{s.Command}, s.Args...), true
	}
	return nil, false
}

// recognizedMCPArgv reports whether argv is a shape doctor TRUSTS to exec as a
// probe: either the known gog spawn form, or (optionally wrapped in
// `op run … -- …`) an ABSOLUTE path whose basename is `pi-stack-host` followed
// by `mcp <name>` — exactly how mcp.go registers a local stdio server. Anything
// else is an arbitrary command someone put in the registration, which doctor
// must NOT run.
func recognizedMCPArgv(argv []string, name string) bool {
	if _, ok := gogSpawnArgv(argv); ok {
		return true
	}
	// Unwrap ONLY a trusted `op run … -- <cmd…>` prefix. A `--` behind any other
	// argv[0] is rejected: the probe execs argv[0] verbatim, so unwrapping a
	// prefix like `/tmp/evil -- pi-stack-host mcp slack` would exec /tmp/evil.
	cmd, ok := unwrapOpRun(argv)
	if !ok {
		return false
	}
	if len(cmd) < 3 {
		return false
	}
	if !filepath.IsAbs(cmd[0]) || filepath.Base(cmd[0]) != "pi-stack-host" {
		return false
	}
	return cmd[1] == "mcp" && cmd[2] == name
}

// probeRegisteredMCP runs the registered command with `--list-tools` appended
// (the same handshake probeRegisteredGog uses), BOUNDED by probeRun's timeout +
// output cap, and returns the count of tool lines it prints plus whether the
// command ran cleanly. A timeout, a non-zero exit, or a server that doesn't
// support `--list-tools` -> (0,false), so the caller reports "registered"
// without a bogus tool count instead of hanging or emitting a false failure.
func probeRegisteredMCP(env shellEnv, argv []string) (int, bool) {
	if len(argv) == 0 {
		return 0, false
	}
	full := append(append([]string{}, argv...), "--list-tools")
	out, timedOut, err := probeRun(env, full[0], full[1:]...)
	if timedOut || err != nil {
		return 0, false
	}
	n := 0
	for _, ln := range strings.Split(out, "\n") {
		if strings.TrimSpace(ln) != "" {
			n++
		}
	}
	return n, true
}

// mcpGroup builds the generic MCP-servers cluster for every configured local
// stdio server. gog is DELIBERATELY skipped here — the dedicated gog group
// already owns its registration check + TODO, so probing it again would emit a
// duplicate `pi-stack mcp register`.
func mcpGroup(cfg *config.Config, env shellEnv, mcpOut string, mcpOK, sbxPresent bool) group {
	mcp := group{title: "MCP servers (local stdio, run by the sbx gateway)"}
	var others []string
	for _, m := range cfg.MCP {
		if m == "gog" {
			continue
		}
		others = append(others, m)
	}
	if len(others) == 0 {
		mcp.checks = append(mcp.checks, check{
			label:  "(none configured)",
			note:   true,
			detail: "add servers with `pi-stack config set mcp <server>`",
		})
	} else {
		for _, m := range others {
			mcp.checks = append(mcp.checks, mcpProbeCheck(env, m, mcpOut, mcpOK, sbxPresent))
		}
	}
	return mcp
}
