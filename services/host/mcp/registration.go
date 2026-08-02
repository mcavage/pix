package mcp

import (
	"encoding/json"
	"pix/host/config"
	"pix/host/hostenv"
	"regexp"
	"strings"
)

// registration.go — questions about what IS registered with the sbx gateway,
// as opposed to what registration WOULD do. These were declared in doctor,
// which was their first caller; they are MCP knowledge, and slack, status and
// pack all ask them too.

// RegisteredCommand asks sbx for the DEFINITION actually registered for
// <name> — the argv the gateway would spawn — so doctor can probe the real
// registration for a local stdio server. Definition inspection ONLY: nothing
// here says anything about sandbox attachment (that is the receipt's job).
// It tries the current `sbx mcp inspect <name>`, then the legacy `get` form,
// then `sbx mcp ls -o json`, all BOUNDED via
// probeRun so a hung sbx degrades to "couldn't read the registered command",
// never a wedged doctor. Returns (nil,false) when sbx is absent or exposes no
// command.
func RegisteredCommand(env hostenv.Env, name string) ([]string, bool) {

	if _, err := env.LookPath("sbx"); err != nil {
		return nil, false
	}
	if out, timedOut, err := env.RunTimed("sbx", "mcp", "inspect", name); err == nil && !timedOut {
		if argv, ok := parseMCPCommandLine(out); ok {
			return argv, true
		}
	}
	// Compatibility with older sbx releases that called this command `get`.
	if out, timedOut, err := env.RunTimed("sbx", "mcp", "get", name); err == nil && !timedOut {
		if argv, ok := parseMCPCommandLine(out); ok {
			return argv, true
		}
	}
	if out, timedOut, err := env.RunTimed("sbx", "mcp", "ls", "-o", "json"); err == nil && !timedOut {
		if argv, ok := parseMCPCommandJSON(out, name); ok {
			return argv, true
		}
	}
	return nil, false
}

// RecognizedArgv reports whether argv is a shape doctor TRUSTS to exec as a
// probe: either a TRUSTED gog spawn (canonical gog/op executables), or
// (optionally wrapped in `op run … -- …`, with the op binary itself canonical)
// an ABSOLUTE path equal to the canonical `pix-host` followed by
// `mcp <name>` — exactly how mcp.go registers a local stdio server. Anything
// else is an arbitrary command someone put in the registration, which doctor
// must NOT run. On success it returns the NORMALIZED argv: every executable
// token replaced with the resolver's canonical path, so the caller execs the
// TRUSTED tokens, never the registered spelling — there is no check-then-exec
// window on a path an attacker controls (a symlink blessed at check time and
// swapped before exec never enters the picture, because symlink resolution is
// never consulted and the exec'd token is the resolver's own answer).
func RecognizedArgv(env hostenv.Env, argv []string, name, opRefs string) ([]string, bool) {
	if norm, ok := TrustedGogSpawn(env, argv, opRefs); ok {
		return norm, true
	}
	// Unwrap ONLY the exact launcher-generated `op run --no-masking
	// --env-file=<refs> --` wrapper grammar (UnwrapOpRun). A `--` behind any
	// other prefix — a foreign argv[0], another op subcommand, an alternate
	// env file, extra options — is rejected: the probe execs these tokens.
	cmd, ok := UnwrapOpRun(env, argv, opRefs)
	if !ok {
		return nil, false
	}
	norm := append([]string(nil), argv...)
	innerStart := len(argv) - len(cmd)
	if innerStart > 0 {
		// An op-wrapped command must run the SAME op binary env.LookPath finds —
		// a look-alike `/tmp/op` is never executed.
		opTok, opOK := trustedExecPath(env, argv[0], "op")
		if !opOK {
			return nil, false
		}
		norm[0] = opTok
	}
	if len(cmd) < 3 {
		return nil, false
	}
	if cmd[1] != "mcp" || cmd[2] != name {
		return nil, false
	}
	// Basename alone ("pix-host") is NOT enough — an absolute path
	// anywhere on disk with that basename (e.g. /tmp/malicious/pix-host)
	// would satisfy a basename check. Require the CANONICAL binary registration
	// actually uses, and exec THAT token.
	hostTok, hostOK := TrustedHostBinaryExecPath(env, cmd[0])
	if !hostOK {
		return nil, false
	}
	norm[innerStart] = hostTok
	return norm, true
}

// Configured reports whether name is in the configured MCP set (so `run`
// auto-attaches it via --mcp).
func Configured(cfg *config.Config, name string) bool {
	for _, m := range cfg.MCP {
		if m == name {
			return true
		}
	}
	return false
}

// commandLineRe matches a `command: <full command>` (or `command = ...`) line
// in an `sbx mcp inspect` text dump.
var commandLineRe = regexp.MustCompile(`(?im)^\s*command\s*[:=]\s*(.+?)\s*$`)

// parseMCPCommandLine extracts a registered argv from `sbx mcp inspect <name>`
// (or the legacy `get` equivalent)
// text dump: the `command:` line split into fields. A shell-quoted line (which
// strings.Fields cannot split reliably) or an empty command returns (nil,false)
// so mcp.RegisteredCommand falls through to the structured JSON parser.
func parseMCPCommandLine(out string) ([]string, bool) {
	m := commandLineRe.FindStringSubmatch(out)
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
