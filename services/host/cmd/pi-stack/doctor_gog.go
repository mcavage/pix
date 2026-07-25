package main

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"

	"pi-stack/host/config"
)

// gogAccount resolves the Google Workspace account the best-effort fallback
// probe runs against. config.toml's `gog_account` is the SINGLE source of truth
// (it is what `make mcp-register` / `pi-stack mcp register` hand the gateway,
// both sourced via `pi-stack config get gog_account`):
//  1. config.toml's `gog_account` (cfg.GogAccount, profile-resolved),
//  2. the $GOG_ACCOUNT env var.
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
	out, timedOut, err := probeRun(env, "op", "run", "--env-file="+opRefs, "--",
		"gog", "--account", acct, "mcp", "--list-tools")
	return !timedOut && err == nil && strings.TrimSpace(out) != ""
}

// gogGroup builds the gog check cluster. The HONEST path reads the ACTUAL
// command the sbx gateway registered for gog and probes THAT (so it verifies
// the registered account, op-refs path, and op/gog binaries as-registered). Only
// when sbx is absent (or exposes no command) does it fall back to a best-effort
// reconstruction from config — clearly labeled, and never a confirmed green.
// Every probe degrades to a TODO rather than crashing, so this runs cleanly
// in-sandbox (gog/sbx/op all absent).
func gogGroup(cfg *config.Config, env shellEnv, mcpOut string, mcpOK, sbxPresent bool) group {
	g := group{title: "gog (Google Workspace via host MCP — read-only)"}

	// HONEST PATH: probe the command sbx ACTUALLY registered for gog. This is the
	// only check that proves the real registration — account, op-refs path, and
	// op/gog binaries all exactly as the gateway will spawn them.
	if argv, ok := registeredGogCommand(env); ok {
		g.checks = append(g.checks, check{label: "registration", note: true,
			detail: "probing the sbx-registered command: " + redactRegisteredCommand(argv)})
		if probeRegisteredGog(env, argv) {
			// Distinguish the op-wrapped path (op-refs resolved) from a BARE spawn so a
			// bare green never implies 1Password creds were involved.
			detail := "registered command exposes tools (verified as-registered, via op run)"
			if !gogSpawnIsOpWrapped(argv) {
				detail = "registered command exposes tools (verified as-registered) — spawned BARE (no op-refs involved)"
			}
			g.checks = append(g.checks, check{label: "headless spawn", verdict: verdictReady,
				detail: detail})
		} else {
			g.checks = append(g.checks, check{label: "headless spawn", verdict: verdictTodo,
				detail: "the registered command returns 0 tools — keyring not headless",
				todo:   "add GOG_KEYRING_BACKEND=file + GOG_KEYRING_PASSWORD + GOG_ACCOUNT + GOG_HOME to " + defaultOpRefsPath(env)})
		}
		g.checks = append(g.checks, mcpCheck("gog", mcpOut, mcpOK, sbxPresent))
		g.checks = append(g.checks, gogAttachCheck(cfg))
		return g
	}

	// 1. gog CLI installed (the reconstruction probe uses it).
	if _, err := env.lookPath("gog"); err != nil {
		g.checks = append(g.checks, check{label: "gog CLI", verdict: verdictTodo,
			detail: "not found", todo: "brew install gog"})
		return g
	}
	g.checks = append(g.checks, check{label: "gog CLI", verdict: verdictReady, detail: "installed"})

	acct := gogAccount(cfg, env)
	opRefs := resolveOpRefs(env)

	// FALLBACK / TRANSPARENCY: sbx couldn't tell us the registered command, so we
	// reconstruct the probe from config and LABEL it best-effort — we can verify
	// THIS account/op-refs authenticates, but NOT that it matches what the gateway
	// registered. Name exactly what we're checking so a pass can never silently
	// mean "checked a different account/path than the sbx gateway got".
	acctShown, refsShown := acct, opRefs
	if acctShown == "" {
		acctShown = "<unknown>"
	}
	if refsShown == "" {
		refsShown = "<not found>"
	}
	// The fallback reason depends on sbx presence: if sbx is PRESENT but its
	// registration couldn't be read (host, gateway likely off), say so; only call
	// it "sbx unavailable" when sbx is actually absent (in the sandbox).
	fallbackWhy := "best-effort (sbx unavailable)"
	if sbxPresent {
		fallbackWhy = "best-effort (couldn't read sbx MCP registrations — check the sbx daemon: sbx mcp status)"
	}
	g.checks = append(g.checks,
		check{label: "verifying", note: true,
			detail: fallbackWhy + " — verifies " + acctShown + " via " + refsShown},
		check{label: "note", note: true,
			detail: "must match your `make mcp-register` (config.toml gog_account + config/op-refs.env)"})

	if acct == "" {
		// 2'. No account configured — can't probe auth or the headless path, so we
		// must NOT report green: say we cannot verify and name the two sources.
		g.checks = append(g.checks, check{label: "account", verdict: verdictTodo,
			detail: "cannot verify (gog_account unset in config.toml/env)",
			todo:   "pi-stack config set gog_account <you@example.com>"})
		g.checks = append(g.checks, mcpCheck("gog", mcpOut, mcpOK, sbxPresent))
		g.checks = append(g.checks, gogAttachCheck(cfg))
		return g
	}

	if opRefs == "" {
		// Can't run the gateway-equivalent headless probe without op-refs.env. But
		// op-refs is OPTIONAL for gog: it authenticates via OAuth (gog auth login),
		// and only needs op-refs to inject a headless keyring PASSWORD when the
		// gateway can't unlock its keyring otherwise. So this is an info line, not a
		// TODO — and it is self-contained (a gog-only config renders no Secrets
		// group, so we must not point at one).
		g.checks = append(g.checks,
			check{label: "account", note: true, detail: acct + " set (unconfirmed vs registration)"},
			check{label: "op-refs", note: true,
				detail: "op-refs.env not found — only needed if the gateway can't unlock gog's keyring headlessly"})
		g.checks = append(g.checks, mcpCheck("gog", mcpOut, mcpOK, sbxPresent))
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
		g.checks = append(g.checks, check{label: "account", verdict: verdictTodo,
			detail: acct + " not authorized",
			todo:   "gog auth add-client <client.json> && gog --account " + acct + " auth login"})
	case opErr != nil:
		// Interactive auth OK, but op is absent so we can't run the gateway-
		// equivalent probe. Say so rather than blaming the keyring.
		g.checks = append(g.checks,
			check{label: "account", verdict: verdictReady, detail: acct + " authorized (interactive)"},
			check{label: "headless spawn", verdict: verdictTodo,
				detail: "can't verify the gateway spawn — op (1Password CLI) not found",
				todo:   "install the 1Password CLI (op) so doctor can probe the real headless path"})
	case !headOK:
		// THE TRAP: interactive passes, the headless gateway spawn gets 0 tools.
		g.checks = append(g.checks,
			check{label: "account", verdict: verdictReady, detail: acct + " authorized (interactive)"},
			check{label: "headless spawn", verdict: verdictTodo,
				detail: "auth OK in your shell but the gateway spawn gets 0 tools — keyring not headless",
				todo:   "add GOG_KEYRING_BACKEND=file + GOG_KEYRING_PASSWORD + GOG_ACCOUNT + GOG_HOME to " + defaultOpRefsPath(env)})
	default:
		// Best-effort success: this account authenticates headlessly, but we could
		// NOT confirm it is the one the sbx gateway actually registered — so this
		// MUST count as an outstanding item (a TODO), never a silent green. A
		// reconstructed probe that happens to pass could still be a different
		// account/op-refs than the gateway got. Only the honest path above
		// (registered command read + probed) earns a confirmed ✓.
		g.checks = append(g.checks,
			check{label: "account", note: true, detail: acct + " authorized (best-effort, unconfirmed vs registration)"},
			check{label: "headless spawn", verdict: verdictTodo,
				detail: "best-effort OK, but could not confirm the sbx-registered command",
				todo:   "confirm the registered gog command: `sbx mcp get gog` (or `sbx mcp ls`)"})
	}

	// 4. registered with the gateway. 5. attached on run?
	g.checks = append(g.checks, mcpCheck("gog", mcpOut, mcpOK, sbxPresent))
	g.checks = append(g.checks, gogAttachCheck(cfg))
	return g
}

// redactRegisteredCommand renders a registered MCP argv SAFELY for display: it
// keeps argv[0]'s basename plus recognizable subcommands/flag NAMES (run, mcp,
// gog, op, pi-stack-host, --account, --env-file=…, etc.) and replaces every
// other token — any of which could be a pasted value/secret — with ‹redacted›.
// It NEVER echoes an unrecognized token verbatim.
func redactRegisteredCommand(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	// Bare words + flag NAMES doctor recognizes as non-secret structure. Anything
	// NOT here is treated as a potential value and redacted, so an unrecognized
	// token is never echoed verbatim.
	recognized := map[string]bool{
		// binaries / subcommands
		"run": true, "mcp": true, "gog": true, "op": true, "pi-stack-host": true,
		"slack": true, "auth": true, "doctor": true, "--": true,
		// flag NAMES (their VALUES are still redacted)
		"--list-tools": true, "--account": true, "--env-file": true, "--check": true,
		"--gmail-no-send": true, "--wrap-untrusted": true, "--readonly": true,
		"--allow-tool": true,
	}
	out := make([]string, 0, len(argv))
	for i, tok := range argv {
		if i == 0 {
			out = append(out, filepath.Base(tok))
			continue
		}
		// A --flag=value token: keep the recognized flag NAME, elide the value.
		if strings.HasPrefix(tok, "--") {
			if eq := strings.IndexByte(tok, '='); eq > 0 {
				name := tok[:eq]
				if recognized[name] {
					out = append(out, name+"=…")
					continue
				}
				out = append(out, "‹redacted›")
				continue
			}
		}
		if recognized[tok] {
			out = append(out, tok)
			continue
		}
		out = append(out, "‹redacted›")
	}
	return strings.Join(out, " ")
}

// gogSpawnIsOpWrapped reports whether the registered gog command runs via the
// `op run --env-file=… -- gog … mcp …` wrapper (argv[0] is the op binary) rather
// than a BARE `gog … mcp …` spawn. Used so a bare-spawn green never implies
// op-refs were resolved.
func gogSpawnIsOpWrapped(argv []string) bool {
	return len(argv) > 0 && filepath.Base(argv[0]) == "op"
}

// registeredGogCommand asks sbx what command it ACTUALLY registered for the gog
// MCP server, so doctor can probe the real registration instead of a config
// reconstruction that may have drifted from what `make mcp-register` wired up.
// It tries, in order, `sbx mcp get gog`, then `sbx mcp ls -o json`, returning
// the parsed argv. Returns (nil,false) when sbx is absent or exposes no command
// — the caller then falls back to the best-effort reconstruction.
func registeredGogCommand(env shellEnv) ([]string, bool) {
	if env.lookPath == nil || env.run == nil {
		return nil, false
	}
	if _, err := env.lookPath("sbx"); err != nil {
		return nil, false
	}
	if out, err := env.run("sbx", "mcp", "get", "gog"); err == nil {
		if argv, ok := parseGogCommandLine(out); ok {
			return argv, true
		}
	}
	if out, err := env.run("sbx", "mcp", "ls", "-o", "json"); err == nil {
		if argv, ok := parseGogCommandJSON(out); ok {
			return argv, true
		}
	}
	return nil, false
}

// gogCommandLineRe matches a `command: <full command>` (or `command = ...`) line
// in `sbx mcp get gog` output.
var gogCommandLineRe = regexp.MustCompile(`(?im)^\s*command\s*[:=]\s*(.+?)\s*$`)

// parseGogCommandLine extracts the registered argv from a `sbx mcp get gog`
// text dump: the `command:` line, split into fields. It only accepts an
// UNAMBIGUOUS, COMPLETE command (see gogCommandComplete). A shell-quoted line
// (which strings.Fields cannot split reliably), or a partial capture — just
// `op`, `op run`, or the command line when the args landed on a separate line —
// returns (nil,false) so registeredGogCommand falls through to the structured
// JSON parser rather than probing a truncated/wrong argv.
func parseGogCommandLine(out string) ([]string, bool) {
	m := gogCommandLineRe.FindStringSubmatch(out)
	if len(m) < 2 {
		return nil, false
	}
	cmd := strings.TrimSpace(m[1])
	if cmd == "" {
		return nil, false
	}
	// Shell-quoted args are ambiguous under strings.Fields — fall through to JSON.
	if strings.ContainsAny(cmd, "\"'") {
		return nil, false
	}
	fields := strings.Fields(cmd)
	if !gogCommandComplete(fields) {
		return nil, false
	}
	return fields, true
}

// gogCommandComplete reports whether argv is a full, unambiguous gog spawn. gog
// can be registered TWO ways (see mcp.go serverCmd/addArgs): op-wrapped
// (`op run --env-file=… -- gog … mcp …`, when op-refs is present) or BARE
// (`gog … mcp …`, when op-refs is absent — 1Password is optional for gog). A
// command is complete in EITHER form: it resolves (unwrapping any `op run … --`
// prefix) to a binary whose basename is `gog` and whose args carry the `mcp`
// subcommand. A partial capture (`op`, `op run`, args on a separate line) does
// not, so the caller keeps looking rather than probe a truncated command.
func gogCommandComplete(argv []string) bool {
	_, ok := gogSpawnArgv(argv)
	return ok
}

// gogSpawnArgv extracts the effective gog spawn argv from a registered command,
// handling both the op-wrapped form (`op run … -- gog … mcp …`) and the bare
// form (`gog … mcp …`). It returns (cmd,true) when the resolved binary's
// basename is `gog` and its args contain the `mcp` subcommand; (nil,false)
// otherwise. Guards against index-out-of-range on short/empty argv.
func gogSpawnArgv(argv []string) ([]string, bool) {
	if len(argv) == 0 {
		return nil, false
	}
	// Unwrap ONLY a trusted `op run … -- <cmd…>` prefix; a `--` behind a non-op
	// argv[0] is rejected (the probe execs argv[0] verbatim).
	cmd, ok := unwrapOpRun(argv)
	if !ok {
		return nil, false
	}
	if len(cmd) == 0 || strings.TrimSpace(cmd[0]) == "" {
		return nil, false
	}
	if filepath.Base(cmd[0]) != "gog" {
		return nil, false
	}
	for _, a := range cmd[1:] {
		if a == "mcp" {
			return cmd, true
		}
	}
	return nil, false
}

// parseGogCommandJSON extracts the registered argv from `sbx mcp ls -o json`
// (an array of {name, command, args}). Returns (nil,false) when there is no gog
// entry or the JSON doesn't parse.
func parseGogCommandJSON(out string) ([]string, bool) {
	var servers []struct {
		Name    string   `json:"name"`
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	if err := json.Unmarshal([]byte(out), &servers); err != nil {
		return nil, false
	}
	for _, s := range servers {
		if s.Name != "gog" || strings.TrimSpace(s.Command) == "" {
			continue
		}
		argv := append([]string{s.Command}, s.Args...)
		// Same completeness bar as the line form: a JSON entry that does not resolve
		// to a `gog … mcp …` spawn (op-wrapped or bare) is not a confident command,
		// so return not-found and let doctor take the honest best-effort fallback.
		if !gogCommandComplete(argv) {
			return nil, false
		}
		return argv, true
	}
	return nil, false
}

// probeRegisteredGog runs the EXACT registered command with `--list-tools`
// appended and reports whether it yields a non-empty tool list — verifying the
// real gateway spawn (account, op-refs, op/gog binaries) all as-registered.
// This works for BOTH registration forms unchanged: the op-wrapped form runs
// `op run … -- gog … mcp … --list-tools`, and the bare form runs
// `gog … mcp … --list-tools` (argv[0] is gog itself). It degrades cleanly
// (returns false, never crashes) on any error.
func probeRegisteredGog(env shellEnv, argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	full := append(append([]string{}, argv...), "--list-tools")
	out, timedOut, err := probeRun(env, full[0], full[1:]...)
	return !timedOut && err == nil && strings.TrimSpace(out) != ""
}

// gogAttachCheck is the informational check 5: is gog in the configured MCP set,
// so `pi-stack run` auto-attaches it (--mcp gog)?
func gogAttachCheck(cfg *config.Config) check {
	if mcpConfigured(cfg, "gog") {
		return check{label: "attached", note: true, detail: "auto-attached on run (--mcp gog)"}
	}
	return check{label: "attached", note: true,
		detail: "run `pi-stack config set mcp gog` to attach it"}
}
