package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"pi-stack/host/config"
)

// doctor_gog.go builds doctor's gog (Google Workspace) group on the S04
// readiness axes. gog is ALWAYS optional (never core, so nothing here can
// block doctor's exit code); a missing gog CLI or an unset account is
// optional-NOT-CONFIGURED (a note, not a failure). The honest path reads the
// command sbx ACTUALLY registered for gog, verifies its hardened read-only
// flags as EVIDENCE, trust-gates the executables against the PATH-resolved
// canonical binaries (never exec an attacker-controlled registered path or a
// symlink-swapped spelling), and probes THAT: non-empty tools = ready, a clean
// zero-tool list = a verified todo, a timeout/transport failure =
// unverifiable, and an explicit policy denial = denied.

// gogAccount resolves the Google Workspace account the best-effort fallback
// probe runs against. config.toml's `gog_account` is the SINGLE source of truth
// (it is what `make mcp-register` / `pi-stack mcp register` hand the gateway,
// both sourced via `pi-stack config get gog_account`):
//  1. config.toml's `gog_account` (cfg.GogAccount),
//  2. the $GOG_ACCOUNT env var.
//
// NEVER a hardcoded address. Empty means "not configured" and the caller emits
// a not-configured note rather than reporting green.
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

// probeStatus/probeResult (the `--list-tools` probe outcome), probeListTools,
// classifyProbeErr, trustedExecPath, and trustedGogSpawn are SHARED with
// doctor_mcp.go — see doctor_probe.go, which owns the single implementation
// (this file's version was the superset kept there: probeDeniedByPolicy, an
// EXPLICIT policy/permission refusal distinguished from a generic probe
// error).

// gogHardenedFlags are the read-only runtime flags a healthy gog registration
// MUST carry (mcp.go's gogHardenedArgv). Doctor verifies their presence in the
// registered argv and reports them as evidence — a registration missing any of
// them is a verified gap (writes would not be blocked at runtime).
var gogHardenedFlags = []string{"--gmail-no-send", "--wrap-untrusted", "--readonly"}

// gogMissingHardenedFlags returns the hardened read-only flags absent from the
// registered gog spawn's INNER argv (after unwrapping any op-run prefix).
func gogMissingHardenedFlags(argv []string) []string {
	inner, ok := gogSpawnArgv(argv)
	if !ok {
		return append([]string(nil), gogHardenedFlags...)
	}
	present := map[string]bool{}
	for _, a := range inner[1:] {
		present[a] = true
	}
	var missing []string
	for _, f := range gogHardenedFlags {
		if !present[f] {
			missing = append(missing, f)
		}
	}
	return missing
}

// gogHeadlessProbe probes the RECONSTRUCTED headless spawn (the exact argv
// registration would produce for these inputs — gogRegisteredArgv, so the
// probe can never drift lighter than what registration runs): op-wrapped when
// op + op-refs resolve, bare otherwise.
func gogHeadlessProbe(env shellEnv, acct, opRefs string) probeResult {
	if acct == "" {
		return probeResult{status: probeError, detail: "could not run (account unresolved)"}
	}
	if env.lookPath == nil {
		return probeResult{status: probeError, detail: "could not run (no lookPath)"}
	}
	gogPath, err := env.lookPath("gog")
	if err != nil {
		return probeResult{status: probeError, detail: "could not run (gog not found)"}
	}
	opPath, opErr := env.lookPath("op")
	if opErr != nil || opRefs == "" {
		opPath, opRefs = "", ""
	}
	return probeListTools(env, gogRegisteredArgv(gogPath, opPath, opRefs, acct))
}

// gogHeadlessOK is gogHeadlessProbe collapsed to a bool for callers that only
// need pass/fail (gog setup's follow-up gate).
func gogHeadlessOK(env shellEnv, acct, opRefs string) bool {
	return gogHeadlessProbe(env, acct, opRefs).status == probeToolsOK
}

// gogSetupHint is the ONE guided recovery command doctor ever points at for
// gog auth/registration gaps — never a raw legacy `gog auth login` recipe.
const gogSetupHint = "pi-stack gog setup"

// spawnCheck builds the "headless spawn" check from a structured probe result.
// Shared by the honest (registered-command) path and the best-effort
// reconstruction fallback, with per-path ready/zero-tools wording.
func gogSpawnCheck(env shellEnv, res probeResult, readyDetail, noToolsDetail string) check {
	switch res.status {
	case probeToolsOK:
		return check{label: "headless spawn", verdict: verdictReady,
			detail:   readyDetail,
			evidence: fmt.Sprintf("--list-tools returned %s", plural(res.tools, "tool"))}
	case probeNoTools:
		return check{label: "headless spawn", verdict: verdictTodo,
			detail:   noToolsDetail,
			evidence: "--list-tools exited cleanly with an empty tool list",
			todo:     "add GOG_KEYRING_BACKEND=file + GOG_KEYRING_PASSWORD + GOG_ACCOUNT + GOG_HOME to " + defaultOpRefsPath(env)}
	case probeDeniedByPolicy:
		return check{label: "headless spawn", verdict: verdictDenied,
			detail:   "the spawn was positively refused by policy/permission — an organizational denial, not a setup gap",
			evidence: "probe output carried an explicit policy denial"}
	default: // probeTimedOut / probeError — unverifiable, never a keyring claim
		return check{label: "headless spawn", verdict: verdictUnverifiable,
			detail:   "probe " + res.detail + " — could not verify (inspect: sbx mcp get gog)",
			evidence: "probe " + res.detail}
	}
}

// gogGroup builds the gog check cluster. The HONEST path reads the ACTUAL
// command the sbx gateway registered for gog, verifies its hardened read-only
// flags, trust-gates its executables, and probes THAT. Only when sbx is absent
// (or exposes no command) does it fall back to a best-effort reconstruction
// from config — clearly labeled, and never a confirmed green. Every probe
// degrades cleanly, so this runs in-sandbox (gog/sbx/op all absent) too.
func gogGroup(cfg *config.Config, env shellEnv, mcpOut string, mcpOK, sbxPresent bool) group {
	g := group{title: "gog (Google Workspace via host MCP — read-only)"}

	// HONEST PATH: probe the command sbx ACTUALLY registered for gog. This is the
	// only check that proves the real registration — account, op-refs path, and
	// op/gog binaries all exactly as the gateway will spawn them.
	if argv, ok := registeredGogCommand(env); ok {
		g.checks = append(g.checks, check{label: "registration", note: true,
			detail: "probing the sbx-registered command: " + redactRegisteredCommand(argv)})

		// Read-only hardening as EVIDENCE: the registered argv must carry the
		// exact runtime flags that block writes. Their absence is a VERIFIED gap
		// (the runtime backstop is off), fixed by re-registering the hardened
		// command — via the guided setup, never a raw recipe.
		if missing := gogMissingHardenedFlags(argv); len(missing) > 0 {
			g.checks = append(g.checks, check{label: "read-only", verdict: verdictTodo,
				detail:   "registered command is missing hardened read-only flags: " + strings.Join(missing, " "),
				evidence: "registered argv lacks " + strings.Join(missing, " "),
				todo:     gogSetupHint + "  (re-registers gog with the hardened read-only flags)"})
		} else {
			g.checks = append(g.checks, check{label: "read-only", verdict: verdictReady,
				detail:   "registered command carries the hardened read-only flags",
				evidence: strings.Join(gogHardenedFlags, " ") + " present in the registered argv"})
		}

		// TRUST GATE: NEVER exec a registered command whose gog/op executable is
		// not the canonical PATH-resolved binary — a look-alike /tmp/gog, a fake
		// op, or a symlink-swapped spelling is skipped (unverifiable), not probed.
		// On trust, exec the NORMALIZED argv (canonical executable tokens), never
		// the registered spelling.
		trustedArgv, trusted := trustedGogSpawn(env, argv)
		if !trusted {
			g.checks = append(g.checks, check{label: "headless spawn", verdict: verdictUnverifiable,
				detail:   "probe skipped: the registered command's gog/op executable does not match the PATH-resolved binary (inspect: sbx mcp get gog) — never executed",
				evidence: "registered executable token not canonical; probe not executed"})
			g.checks = append(g.checks, mcpCheck("gog", mcpOut, mcpOK, sbxPresent))
			g.checks = append(g.checks, gogAttachCheck(cfg))
			return g
		}
		readyDetail := "registered command exposes tools (verified as-registered, via op run)"
		if !gogSpawnIsOpWrapped(argv) {
			readyDetail = "registered command exposes tools (verified as-registered) — spawned BARE (no op-refs involved)"
		}
		g.checks = append(g.checks, gogSpawnCheck(env, probeListTools(env, trustedArgv),
			readyDetail,
			"the registered command returns 0 tools — keyring not headless"))
		g.checks = append(g.checks, mcpCheck("gog", mcpOut, mcpOK, sbxPresent))
		g.checks = append(g.checks, gogAttachCheck(cfg))
		return g
	}

	// 1. gog CLI installed (the reconstruction probe uses it). Not installed is
	// optional-NOT-CONFIGURED: an expected absence (a note), never a failure.
	if _, err := env.lookPath("gog"); err != nil {
		g.checks = append(g.checks, check{label: "gog CLI", note: true,
			detail: "not installed — optional; set up Google Workspace with: " + gogSetupHint})
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
			detail: "must match the sbx-registered gog command (config.toml gog_account + op-refs.env)"})

	if acct == "" {
		// 2'. No account configured — optional-NOT-CONFIGURED: an expected
		// absence, a note (no ✗, no repair TODO — the setup command lives in the
		// detail for whoever wants to opt in).
		g.checks = append(g.checks, check{label: "account", note: true,
			detail: "not configured (gog_account unset) — set up: " + gogSetupHint})
		g.checks = append(g.checks, mcpCheck("gog", mcpOut, mcpOK, sbxPresent))
		g.checks = append(g.checks, gogAttachCheck(cfg))
		return g
	}

	if opRefs == "" {
		// Can't run the op-wrapped headless probe without op-refs.env. op-refs is
		// OPTIONAL for gog (it authenticates via OAuth; op-refs only injects a
		// headless keyring password when needed), so this is informational.
		g.checks = append(g.checks,
			check{label: "account", verdict: verdictUnverifiable,
				detail: acct + " set (unconfirmed vs registration)"},
			check{label: "op-refs", note: true,
				detail: "op-refs.env not found — only needed if the gateway can't unlock gog's keyring headlessly"})
		g.checks = append(g.checks, mcpCheck("gog", mcpOut, mcpOK, sbxPresent))
		g.checks = append(g.checks, gogAttachCheck(cfg))
		return g
	}

	// 2. account authorized (interactive). 3. THE GOTCHA — headless spawn. The
	// auth check runs through the BOUNDED probe machinery so a hung `gog auth
	// doctor --check` can never wedge doctor.
	_, interTimedOut, interErr := probeRun(env, "gog", "--account", acct, "auth", "doctor", "--check")
	_, opErr := env.lookPath("op")
	head := gogHeadlessProbe(env, acct, opRefs)
	switch {
	case interTimedOut:
		// A timed-out auth check is UNVERIFIABLE, not "not authorized".
		g.checks = append(g.checks, check{label: "account", verdict: verdictUnverifiable,
			detail: acct + " — `gog auth doctor --check` timed out; could not verify"})
	case interErr != nil:
		// Auth itself isn't set up — don't double-report the keyring below. Point
		// at the guided command, never the raw legacy auth recipe.
		g.checks = append(g.checks, check{label: "account", verdict: verdictTodo,
			detail: acct + " not authorized",
			todo:   gogSetupHint})
	case opErr != nil:
		// Interactive auth OK, but op is absent so we can't run the op-wrapped
		// probe. Say so rather than blaming the keyring.
		g.checks = append(g.checks,
			check{label: "account", verdict: verdictReady, detail: acct + " authorized (interactive)"},
			check{label: "headless spawn", verdict: verdictUnverifiable,
				detail: "can't verify the gateway spawn — op (1Password CLI) not found; install it so doctor can probe the real headless path"})
	case head.status == probeToolsOK:
		// Best-effort success: this account authenticates headlessly, but we could
		// NOT confirm it is the command the sbx gateway actually registered. That
		// is UNVERIFIABLE, not a failure: doctor genuinely does not know whether
		// it matches the real registration, so it renders as ⚠, never ✗, and
		// carries no fix-it TODO. Only the honest path above earns a confirmed ✓.
		g.checks = append(g.checks,
			check{label: "account", verdict: verdictUnverifiable,
				detail: acct + " authorized (best-effort, unconfirmed vs registration)"},
			check{label: "headless spawn", verdict: verdictUnverifiable,
				detail: "best-effort headless spawn succeeded, but the sbx-registered command could not be confirmed"})
	default:
		g.checks = append(g.checks,
			check{label: "account", verdict: verdictReady, detail: acct + " authorized (interactive)"},
			gogSpawnCheck(env, head,
				"", // unreachable: probeToolsOK handled above
				"auth OK in your shell but the gateway spawn gets 0 tools — keyring not headless"))
	}

	// 4. registered with the gateway. 5. in the configured MCP set?
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
		"--allow-tool": true, "--no-masking": true,
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
// — the caller then falls back to the best-effort reconstruction. Both
// discovery subprocesses are BOUNDED (probeRun) so a hung sbx can never wedge
// the caller.
func registeredGogCommand(env shellEnv) ([]string, bool) {
	if env.lookPath == nil || (env.run == nil && env.probe == nil) {
		return nil, false
	}
	if _, err := env.lookPath("sbx"); err != nil {
		return nil, false
	}
	if out, timedOut, err := probeRun(env, "sbx", "mcp", "get", "gog"); err == nil && !timedOut {
		if argv, ok := parseGogCommandLine(out); ok {
			return argv, true
		}
	}
	if out, timedOut, err := probeRun(env, "sbx", "mcp", "ls", "-o", "json"); err == nil && !timedOut {
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

// gogAttachCheck is the informational check 5: is gog in the configured MCP set,
// so `pi-stack run` preloads it at sandbox create?
func gogAttachCheck(cfg *config.Config) check {
	if mcpConfigured(cfg, "gog") {
		return check{label: "attached", note: true, detail: "in the configured MCP set — preloaded at sandbox create"}
	}
	return check{label: "attached", note: true,
		detail: "run `pi-stack config set mcp gog` to attach it"}
}
