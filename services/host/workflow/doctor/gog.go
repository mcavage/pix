package doctor

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"pix/host/cli"
	"pix/host/hostenv"
	"pix/host/mcp"
	"pix/host/readiness"
	"pix/host/readiness/axis"
	"pix/host/secret"
	"regexp"
	"strings"

	"pix/host/config"
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

// GogAccount resolves the Google Workspace account the best-effort fallback
// probe runs against. config.toml's `gog_account` is the SINGLE source of truth
// (it is what `make mcp-register` / `pix mcp register` hand the gateway,
// both sourced via `pix config get gog_account`):
//  1. config.toml's `gog_account` (cfg.GogAccount),
//  2. the $GOG_ACCOUNT env var.
//
// NEVER a hardcoded address. Empty means "not configured" and the caller emits
// a not-configured note rather than reporting green.
func GogAccount(cfg *config.Config, env hostenv.Env) string {
	if cfg != nil {
		if a := strings.TrimSpace(cfg.GogAccount); a != "" {
			return a
		}
	}
	if a := strings.TrimSpace(env.Getenv("GOG_ACCOUNT")); a != "" {
		return a
	}
	return ""
}

// probeStatus/axis.ProbeResult (the `--list-tools` probe outcome), axis.ProbeListTools,
// classifyProbeErr, trustedExecPath, and mcp.TrustedGogSpawn are SHARED with
// doctor_mcp.go — see doctor_probe.go, which owns the single implementation
// (this file's version was the superset kept there: axis.ProbeDeniedByPolicy, an
// EXPLICIT policy/permission refusal distinguished from a generic probe
// error).

// GogHardenedFlags are the read-only runtime flags a healthy gog registration
// MUST carry (mcp.go's mcp.GogHardenedArgv). Doctor verifies their presence in the
// registered argv and reports them as evidence — a registration missing any of
// them is a verified gap (writes would not be blocked at runtime).
var GogHardenedFlags = []string{"--gmail-no-send", "--wrap-untrusted", "--readonly"}

// GogMissingHardenedFlags returns the hardened read-only flags absent from the
// registered gog spawn's INNER argv (after unwrapping any op-run prefix).
func GogMissingHardenedFlags(env hostenv.Env, argv []string) []string {
	inner, ok := mcp.GogSpawnArgv(env, argv, secret.FindOpRefs(env))
	if !ok {
		return append([]string(nil), GogHardenedFlags...)
	}
	present := map[string]bool{}
	for _, a := range inner[1:] {
		present[a] = true
	}
	var missing []string
	for _, f := range GogHardenedFlags {
		if !present[f] {
			missing = append(missing, f)
		}
	}
	return missing
}

// gogHeadlessProbe probes the RECONSTRUCTED headless spawn (the exact argv
// registration would produce for these inputs — mcp.GogRegisteredArgv, so the
// probe can never drift lighter than what registration runs): op-wrapped when
// op + op-refs resolve, bare otherwise.
func gogHeadlessProbe(env hostenv.Env, acct, opRefs string) axis.ProbeResult {
	if acct == "" {
		return axis.ProbeResult{Status: axis.ProbeError, Detail: "could not run (account unresolved)"}
	}

	gogPath, err := env.LookPath("gog")
	if err != nil {
		return axis.ProbeResult{Status: axis.ProbeError, Detail: "could not run (gog not found)"}
	}
	opPath, opErr := env.LookPath("op")
	if opErr != nil || opRefs == "" || !secret.OpRefFilled(env, "GOG_KEYRING_PASSWORD") {
		opPath, opRefs = "", ""
	}
	return axis.ProbeListTools(env, mcp.GogRegisteredArgv(gogPath, opPath, opRefs, acct))
}

// GogHeadlessOK is gogHeadlessProbe collapsed to a bool for callers that only
// need pass/fail (gog setup's follow-up gate).
func GogHeadlessOK(env hostenv.Env, acct, opRefs string) bool {
	return gogHeadlessProbe(env, acct, opRefs).Status == axis.ProbeToolsOK
}

// gogSetupHint is the ONE guided recovery command doctor ever points at for
// gog auth/registration gaps — never a raw legacy direct-login recipe.
const gogSetupHint = "pix gworkspace setup"

// spawnCheck builds the "headless spawn" check from a structured probe result.
// Shared by the honest (registered-command) path and the best-effort
// reconstruction fallback, with per-path ready/zero-tools wording.
func GogSpawnCheck(env hostenv.Env, res axis.ProbeResult, readyDetail, noToolsDetail string) readiness.Check {
	switch res.Status {
	case axis.ProbeToolsOK:
		return readiness.Check{Label: "headless spawn", Verdict: readiness.VerdictReady,
			Detail:   readyDetail,
			Evidence: fmt.Sprintf("--list-tools returned %s", cli.Plural(res.Tools, "tool"))}
	case axis.ProbeNoTools:
		return readiness.Check{Label: "headless spawn", Verdict: readiness.VerdictTodo,
			Detail:   noToolsDetail,
			Evidence: "--list-tools exited cleanly with an empty tool list",
			Todo:     "add GOG_KEYRING_BACKEND=file + GOG_KEYRING_PASSWORD + GOG_ACCOUNT + GOG_HOME to " + secret.DefaultOpRefsPath(env)}
	case axis.ProbeDeniedByPolicy:
		return readiness.Check{Label: "headless spawn", Verdict: readiness.VerdictDenied,
			Detail:   "the spawn was positively refused by policy/permission — an organizational denial, not a setup gap",
			Evidence: "probe output carried an explicit policy denial"}
	default: // probeTimedOut / axis.ProbeError — unverifiable, never a keyring claim
		return readiness.Check{Label: "headless spawn", Verdict: readiness.VerdictUnverifiable,
			Detail:   "probe " + res.Detail + " — could not verify (inspect: sbx mcp inspect " + config.GWServerName + ")",
			Evidence: "probe " + res.Detail}
	}
}

// gogGroup builds the gog check cluster. The HONEST path reads the ACTUAL
// command the sbx gateway registered for gog, verifies its hardened read-only
// flags, trust-gates its executables, and probes THAT. Only when sbx is absent
// (or exposes no command) does it fall back to a best-effort reconstruction
// from config — clearly labeled, and never a confirmed green. Every probe
// degrades cleanly, so this runs in-sandbox (gog/sbx/op all absent) too.
func gogGroup(cfg *config.Config, env hostenv.Env, mcpOut string, mcpOK, sbxPresent bool, ctx mcpSandboxContext) readiness.Group {
	g := readiness.Group{Title: "Google Workspace (optional, via host MCP — read-only)"}
	// gog's attachment truth comes from the SAME receipt-backed join row every
	// other MCP server uses (mcpjoin.go, via the shared workspace-sandbox
	// context) — config membership alone is an intent, never an attachment.
	gogReg := mcp.McpRegEvidenceFrom(mcpOut, mcpOK, config.GWServerName)

	// HONEST PATH: probe the command sbx ACTUALLY registered for gog. This is the
	// only check that proves the real registration — account, op-refs path, and
	// op/gog binaries all exactly as the gateway will spawn them.
	if argv, ok := RegisteredGogCommand(env); ok {
		g.Checks = append(g.Checks, readiness.Check{Label: "registration", Note: true, Verdict: readiness.VerdictUnverifiable,
			Detail: "probing the sbx-registered command: " + redactRegisteredCommand(argv)})

		// Read-only hardening as EVIDENCE: the registered argv must carry the
		// exact runtime flags that block writes. Their absence is a VERIFIED gap
		// (the runtime backstop is off), fixed by re-registering the hardened
		// command — via the guided setup, never a raw recipe.
		if missing := GogMissingHardenedFlags(env, argv); len(missing) > 0 {
			g.Checks = append(g.Checks, readiness.Check{Label: "read-only", Verdict: readiness.VerdictTodo,
				Detail:   "registered command is missing hardened read-only flags: " + strings.Join(missing, " "),
				Evidence: "registered argv lacks " + strings.Join(missing, " "),
				Todo:     gogSetupHint + "  (re-registers gog with the hardened read-only flags)"})
		} else {
			g.Checks = append(g.Checks, readiness.Check{Label: "read-only", Verdict: readiness.VerdictReady,
				Detail:   "registered command carries the hardened read-only flags",
				Evidence: strings.Join(GogHardenedFlags, " ") + " present in the registered argv"})
		}

		// TRUST GATE: NEVER exec a registered command whose gog/op executable is
		// not the canonical PATH-resolved binary — a look-alike /tmp/gog, a fake
		// op, or a symlink-swapped spelling is skipped (unverifiable), not probed.
		// On trust, exec the NORMALIZED argv (canonical executable tokens), never
		// the registered spelling.
		trustedArgv, trusted := mcp.TrustedGogSpawn(env, argv, secret.FindOpRefs(env))
		if !trusted {
			g.Checks = append(g.Checks, readiness.Check{Label: "headless spawn", Verdict: readiness.VerdictUnverifiable,
				Detail:   "probe skipped: the registered command's gog/op executable does not match the PATH-resolved binary (inspect: sbx mcp inspect " + config.GWServerName + ") — never executed",
				Evidence: "registered executable token not canonical; probe not executed"})
			g.Checks = append(g.Checks, gogRegistrationCheck(mcpOut, mcpOK, sbxPresent))
			g.Checks = append(g.Checks, gogAttachCheck(cfg, ctx, gogReg))
			return g
		}
		readyDetail := "registered command exposes tools (verified as-registered, via op run)"
		if !gogSpawnIsOpWrapped(argv) {
			readyDetail = "registered command exposes tools (verified as-registered) — spawned BARE (no op-refs involved)"
		}
		g.Checks = append(g.Checks, GogSpawnCheck(env, axis.ProbeListTools(env, trustedArgv),
			readyDetail,
			"the registered command returns 0 tools — keyring not headless"))
		g.Checks = append(g.Checks, gogRegistrationCheck(mcpOut, mcpOK, sbxPresent))
		g.Checks = append(g.Checks, gogAttachCheck(cfg, ctx, gogReg))
		return g
	}

	// 1. gog CLI installed (the reconstruction probe uses it). Not installed is
	// optional-NOT-CONFIGURED: an expected absence (a note), never a failure.
	// The registration + attachment checks are ALWAYS emitted regardless
	// (closure finding #2): a missing local gog executable says nothing about
	// whether the gateway already has gog registered, or whether a sandbox's
	// receipt already proves it attached — dropping those checks here would
	// silently hide real, independently-verifiable evidence.
	if _, err := env.LookPath("gog"); err != nil {
		g.Checks = append(g.Checks, readiness.Check{Label: "dependency CLI", Note: true, Verdict: readiness.VerdictUnverifiable,
			Detail: "not installed — optional; set up Google Workspace with: " + gogSetupHint})
		g.Checks = append(g.Checks, gogRegistrationCheck(mcpOut, mcpOK, sbxPresent))
		g.Checks = append(g.Checks, gogAttachCheck(cfg, ctx, gogReg))
		return g
	}
	g.Checks = append(g.Checks, readiness.Check{Label: "dependency CLI", Verdict: readiness.VerdictReady, Detail: "installed"})

	acct := GogAccount(cfg, env)
	opRefs := secret.FindOpRefs(env)

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
	g.Checks = append(g.Checks,
		readiness.Check{Label: "verifying", Note: true, Verdict: readiness.VerdictUnverifiable,
			Detail: fallbackWhy + " — verifies " + acctShown + " via " + refsShown},
		readiness.Check{Label: "note", Note: true, Verdict: readiness.VerdictUnverifiable,
			Detail: "must match the sbx-registered gog command (config.toml gog_account + op-refs.env)"})

	if acct == "" {
		// 2'. No account configured — optional-NOT-CONFIGURED: an expected
		// absence, a note (no ✗, no repair TODO — the setup command lives in the
		// detail for whoever wants to opt in).
		g.Checks = append(g.Checks, readiness.Check{Label: "account", Note: true, Verdict: readiness.VerdictUnverifiable,
			Detail: "not configured (gog_account unset) — set up: " + gogSetupHint})
		g.Checks = append(g.Checks, gogRegistrationCheck(mcpOut, mcpOK, sbxPresent))
		g.Checks = append(g.Checks, gogAttachCheck(cfg, ctx, gogReg))
		return g
	}

	if opRefs == "" {
		// Can't run the op-wrapped headless probe without op-refs.env. op-refs is
		// OPTIONAL for gog (it authenticates via OAuth; op-refs only injects a
		// headless keyring password when needed), so this is informational.
		g.Checks = append(g.Checks,
			readiness.Check{Label: "account", Verdict: readiness.VerdictUnverifiable,
				Detail: acct + " set (unconfirmed vs registration)"},
			readiness.Check{Label: "op-refs", Note: true, Verdict: readiness.VerdictUnverifiable,
				Detail: "op-refs.env not found — only needed if the gateway can't unlock gog's keyring headlessly"})
		g.Checks = append(g.Checks, gogRegistrationCheck(mcpOut, mcpOK, sbxPresent))
		g.Checks = append(g.Checks, gogAttachCheck(cfg, ctx, gogReg))
		return g
	}

	// 2. account authorized (interactive). 3. THE GOTCHA — headless spawn. The
	// auth check runs through the BOUNDED probe machinery so a hung `gog auth
	// doctor --check` can never wedge doctor.
	_, interTimedOut, interErr := env.RunTimed("gog", "--account", acct, "auth", "doctor", "--check")
	_, opErr := env.LookPath("op")
	head := gogHeadlessProbe(env, acct, opRefs)
	switch {
	case interTimedOut:
		// A timed-out auth check is UNVERIFIABLE, not "not authorized".
		g.Checks = append(g.Checks, readiness.Check{Label: "account", Verdict: readiness.VerdictUnverifiable,
			Detail: acct + " — `gog auth doctor --check` timed out; could not verify"})
	case interErr != nil:
		// Auth itself isn't set up — don't double-report the keyring below. Point
		// at the guided command, never the raw legacy auth recipe.
		g.Checks = append(g.Checks, readiness.Check{Label: "account", Verdict: readiness.VerdictTodo,
			Detail: acct + " not authorized",
			Todo:   gogSetupHint})
	case opErr != nil:
		// Interactive auth OK, but op is absent so we can't run the op-wrapped
		// probe. Say so rather than blaming the keyring.
		g.Checks = append(g.Checks,
			readiness.Check{Label: "account", Verdict: readiness.VerdictReady, Detail: acct + " authorized (interactive)"},
			readiness.Check{Label: "headless spawn", Verdict: readiness.VerdictUnverifiable,
				Detail: "can't verify the gateway spawn — op (1Password CLI) not found; install it so doctor can probe the real headless path"})
	case head.Status == axis.ProbeToolsOK:
		// Best-effort success: this account authenticates headlessly, but we could
		// NOT confirm it is the command the sbx gateway actually registered. That
		// is UNVERIFIABLE, not a failure: doctor genuinely does not know whether
		// it matches the real registration, so it renders as ⚠, never ✗, and
		// carries no fix-it TODO. Only the honest path above earns a confirmed ✓.
		g.Checks = append(g.Checks,
			readiness.Check{Label: "account", Verdict: readiness.VerdictUnverifiable,
				Detail: acct + " authorized (best-effort, unconfirmed vs registration)"},
			readiness.Check{Label: "headless spawn", Verdict: readiness.VerdictUnverifiable,
				Detail: "best-effort headless spawn succeeded, but the sbx-registered command could not be confirmed"})
	default:
		g.Checks = append(g.Checks,
			readiness.Check{Label: "account", Verdict: readiness.VerdictReady, Detail: acct + " authorized (interactive)"},
			GogSpawnCheck(env, head,
				"", // unreachable: axis.ProbeToolsOK handled above
				"auth OK in your shell but the gateway spawn gets 0 tools — keyring not headless"))
	}

	// 4. registered with the gateway. 5. in the configured MCP set?
	g.Checks = append(g.Checks, gogRegistrationCheck(mcpOut, mcpOK, sbxPresent))
	g.Checks = append(g.Checks, gogAttachCheck(cfg, ctx, gogReg))
	return g
}

// redactRegisteredCommand renders a registered MCP argv SAFELY for display: it
// keeps argv[0]'s basename plus recognizable subcommands/flag NAMES (run, mcp,
// gog, op, pix-host, --account, --env-file=…, etc.) and replaces every
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
		"run": true, "mcp": true, "gog": true, "op": true, "pix-host": true,
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

// RegisteredGogCommand asks sbx what command it ACTUALLY registered for the gog
// MCP server, so doctor can probe the real registration instead of a config
// reconstruction that may have drifted from what `make mcp-register` wired up.
// It tries, in order, current `sbx mcp inspect google-workspace`, legacy `get`,
// `sbx mcp ls -o json`, then the current
// `sbx mcp ls` plain table, returning the parsed argv. Returns (nil,false) when
// sbx is absent or exposes no complete command; the caller then falls back to
// the best-effort reconstruction. Every discovery subprocess is BOUNDED
// (probeRun) so a hung sbx can never wedge the caller.
func RegisteredGogCommand(env hostenv.Env) ([]string, bool) {
	if _, err := env.LookPath("sbx"); err != nil {
		return nil, false
	}
	if out, timedOut, err := env.RunTimed("sbx", "mcp", "inspect", config.GWServerName); err == nil && !timedOut {
		if argv, ok := parseGogCommandLine(env, out); ok {
			return argv, true
		}
	}
	if out, timedOut, err := env.RunTimed("sbx", "mcp", "get", config.GWServerName); err == nil && !timedOut {
		if argv, ok := parseGogCommandLine(env, out); ok {
			return argv, true
		}
	}
	if out, timedOut, err := env.RunTimed("sbx", "mcp", "ls", "-o", "json"); err == nil && !timedOut {
		if argv, ok := parseGogCommandJSON(env, out); ok {
			return argv, true
		}
	}
	if out, timedOut, err := env.RunTimed("sbx", "mcp", "ls"); err == nil && !timedOut {
		if argv, ok := parseGogCommandTable(env, out); ok {
			return argv, true
		}
	}
	return nil, false
}

// gogCommandLineRe matches a `command: <full command>` (or `command = ...`) line
// in `sbx mcp inspect google-workspace` output (or the legacy `get` output).
var gogCommandLineRe = regexp.MustCompile(`(?im)^\s*command\s*[:=]\s*(.+?)\s*$`)

// parseGogCommandLine extracts the registered argv from an `sbx mcp inspect google-workspace`
// (or legacy `get`)
// text dump: the `command:` line, split into fields. It only accepts an
// UNAMBIGUOUS, COMPLETE command (see gogCommandComplete). A shell-quoted line
// (which strings.Fields cannot split reliably), or a partial capture — just
// `op`, `op run`, or the command line when the args landed on a separate line —
// returns (nil,false) so RegisteredGogCommand falls through to the structured
// JSON parser rather than probing a truncated/wrong argv.
func parseGogCommandLine(env hostenv.Env, out string) ([]string, bool) {
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
	if !gogCommandComplete(env, fields) {
		return nil, false
	}
	return fields, true
}

// gogCommandComplete reports whether argv is a full, unambiguous gog spawn. gog
// can be registered TWO ways (see mcp.go serverCmd/addArgs): op-wrapped
// (`op run --env-file=… -- gog … mcp …`, when op-refs is present) or BARE
// (`gog … mcp …`, when op-refs is absent — 1Password is optional for gog). A
// command is complete in EITHER form: it resolves (unwrapping ONLY the exact
// launcher-generated `op run --no-masking --env-file=<refs> --` prefix — see
// mcp.UnwrapOpRun) to a binary whose basename is `gog` and whose args carry the
// `mcp` subcommand. A partial capture (`op`, `op run`, args on a separate
// line) or a wrapper that deviates from the launcher grammar does not, so the
// caller keeps looking rather than probe a truncated or drifted command.
func gogCommandComplete(env hostenv.Env, argv []string) bool {
	_, ok := mcp.GogSpawnArgv(env, argv, secret.FindOpRefs(env))
	return ok
}

// parseGogCommandTable extracts gog's argv from the current sbx plain table:
// NAME TYPE URL/COMMAND. Local command rows are whitespace-delimited today;
// quoted tokens are rejected because strings.Fields cannot recover them
// unambiguously. Completeness and wrapper validation use the same strict gate
// as the text and JSON readers.
func parseGogCommandTable(env hostenv.Env, out string) ([]string, bool) {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != config.GWServerName || fields[1] != "local" {
			continue
		}
		if strings.ContainsAny(line, "\"'") {
			return nil, false
		}
		argv := fields[2:]
		if !gogCommandComplete(env, argv) {
			return nil, false
		}
		return argv, true
	}
	return nil, false
}

// parseGogCommandJSON extracts the registered argv from `sbx mcp ls -o json`
// (an array of {name, command, args}). Returns (nil,false) when there is no gog
// entry or the JSON doesn't parse.
func parseGogCommandJSON(env hostenv.Env, out string) ([]string, bool) {
	var servers []struct {
		Name    string   `json:"name"`
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	if err := json.Unmarshal([]byte(out), &servers); err != nil {
		return nil, false
	}
	for _, s := range servers {
		if s.Name != config.GWServerName || strings.TrimSpace(s.Command) == "" {
			continue
		}
		argv := append([]string{s.Command}, s.Args...)
		// Same completeness bar as the line form: a JSON entry that does not resolve
		// to a `gog … mcp …` spawn (op-wrapped or bare) is not a confident command,
		// so return not-found and let doctor take the honest best-effort fallback.
		if !gogCommandComplete(env, argv) {
			return nil, false
		}
		return argv, true
	}
	return nil, false
}

// gogAttachCheck is check 5: gog's sandbox attachment. With a workspace
// sandbox context it is the SAME receipt-backed join row every other MCP
// server gets (mcpAttachCheck -> mcp.JoinMCPSandboxRow): preloaded/loaded receipt
// claims render ready; a registered server a COMPLETE valid receipt has no
// entry for is a verified registered-not-attached TODO (a sandbox created
// BEFORE gog was configured is NOT attached just because cfg now names it);
// everything else stays unverifiable. Without a sandbox context, config
// membership is stated as INTENT (preloads at the next create) — an
// informational note, never a ready attachment claim.
func gogAttachCheck(cfg *config.Config, ctx mcpSandboxContext, reg mcp.McpRegEvidence) readiness.Check {
	if ctx.mode == mcpAttachReceipt {
		return mcpAttachCheck(config.GWServerName, ctx, reg)
	}
	if mcp.Configured(cfg, config.GWServerName) {
		det := "in the configured MCP set — preloads at sandbox create (intent, not attachment)"
		if ctx.mode == mcpAttachSandboxAbsent {
			det = "sandbox " + ctx.sandbox + " not created yet — gog preloads at `pix run` create"
		} else if ctx.note != "" {
			det = ctx.note + " — attachment cannot be reported"
		}
		return readiness.Check{Label: "attached", Note: true, Verdict: readiness.VerdictUnverifiable, Detail: det}
	}
	return readiness.Check{Label: "attached", Note: true, Verdict: readiness.VerdictUnverifiable,
		Detail: "run `pix config set mcp " + config.GWServerName + "` to attach it"}
}
