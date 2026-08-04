package doctor

import (
	"encoding/json"
	"fmt"
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

// GogSpawnCheck builds the "headless spawn" check from a structured probe
// result: shared by workflow/gworkspace's registered-command and best-effort
// reconstruction paths, with per-path ready/zero-tools wording.
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

// gogGroup builds doctor's Google Workspace group down to the same two facts
// every other MCP server's group renders: registration with the sbx gateway
// (gogRegistrationCheck) and sandbox attachment via the shared receipt-backed
// join row (gogAttachCheck). gog keeps its own group rather than joining the
// generic MCP servers group because `pix-host mcp --list` never lists it
// (mcp.LocalMCPNames' documented special case), so the generic classifier
// cannot place it. The elaborate hardened-flags/headless-spawn/account-auth
// probing that used to live here belonged to the retired built-in onboarding
// flow (the old `pix gworkspace setup` OAuth dance) and is gone from doctor;
// GogSpawnCheck/RegisteredGogCommand/GogHardenedFlags/GogHeadlessOK remain
// exported only because workflow/gworkspace (a surviving leaf, retired later
// alongside its own package) still calls them directly.
func gogGroup(cfg *config.Config, env hostenv.Env, mcpOut string, mcpOK, sbxPresent bool, ctx mcpSandboxContext) readiness.Group {
	g := readiness.Group{Title: "Google Workspace (optional, via host MCP — read-only)"}
	gogReg := mcp.McpRegEvidenceFrom(mcpOut, mcpOK, config.GWServerName)
	g.Checks = append(g.Checks, gogRegistrationCheck(mcpOut, mcpOK, sbxPresent))
	g.Checks = append(g.Checks, gogAttachCheck(cfg, ctx, gogReg))
	return g
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
