package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"pix/host/config"
)

// doctor_mcp.go — the MCP truth group (S05). Three axes, each verified from
// its own honest source and NEVER inferred from another:
//
//   - REGISTRATION truth comes from the bounded `sbx mcp ls` listing the
//     caller already fetched (mcpOut/mcpOK). `sbx mcp get <name>` may be used
//     to inspect the REGISTERED DEFINITION (the argv sbx would spawn) only —
//     never to infer sandbox attachment.
//   - ATTACHMENT truth comes from the launcher's own per-sandbox receipt
//     (sandboxmcpstate.go): durable evidence of a SUCCESSFUL pix action
//     (preload at create, or a live `pix mcp load`). Config membership is
//     an intent, not an attachment — it is never presented as one.
//   - AUTH truth (remote servers only) comes from a bounded native
//     `sbx mcp auth status <name>` probe. A local stdio server never gets a
//     native OAuth check — there is no control-plane auth for it.

// gatewayDownDetail / gatewayTODO describe the HOST condition where sbx IS
// present (secret ls succeeded) but `sbx mcp ls` failed. The MCP gateway is now
// the local data-plane one (always available, no SBX_MCP_URL), so a failed
// listing means the sbx daemon/gateway is unhealthy, not "gateway off". This is
// NOT "sbx unavailable": the CLI is here, only the MCP-registration listing failed.
const gatewayDownDetail = "sbx present but couldn't list MCP registrations — check the sbx daemon (sbx mcp status; sbx daemon status)"

// gogRegistrationCheck reports gog's sbx-gateway registration on the SAME
// tri-state evidence every other MCP server uses (mcpRegEvidenceFrom): a
// successful `sbx mcp ls` positively lacking gog is a verified register TODO;
// gog present is ready; a failed/absent listing is UNVERIFIABLE — never a
// false outstanding item invented from a probe that answered nothing. This
// replaces the legacy binary mcpCheck, which rendered every listing failure
// as a TODO.
func gogRegistrationCheck(mcpOut string, mcpOK, sbxPresent bool) check {
	switch mcpRegEvidenceFrom(mcpOut, mcpOK, gwServerName) {
	case mcpRegYes:
		return check{label: gwServerName, verdict: verdictReady, detail: "registered", evidence: "sbx mcp ls"}
	case mcpRegNo:
		return check{label: gwServerName, verdict: verdictTodo, detail: "not registered", todo: "pix mcp register"}
	default: // mcpRegUnknown: sbx absent, or present with the listing failing
		return mcpUnavailableCheck(gwServerName, sbxPresent)
	}
}

// mcpKind is the classification of one configured (or pack-declared) MCP name.
// Classification decides which repair command is honest and which extra probes
// (local spawn, native OAuth) apply — it must never be guessed.
type mcpKind int

const (
	// mcpKindUnknown: the local-name set (`pix-host mcp --list`) could not
	// be established, so doctor genuinely does not know whether this name is a
	// local stdio server or a remote one. FAIL CLOSED: no probe, no exec, no
	// type-specific repair command — recommending `pix mcp register` for a
	// remote name (or bundle/auth for a local one) would be a broken repair.
	mcpKindUnknown mcpKind = iota
	// mcpKindLocal: confirmed in `pix-host mcp --list` — a local stdio
	// server this host can spawn. `pix mcp register <name>` registers it;
	// its health may be probed by exec'ing the registered argv, but ONLY after
	// the canonical-executable gate (recognizedMCPArgv). Never OAuth-checked.
	mcpKindLocal
	// mcpKindCatalog: confirmed NON-local and in the shipped public catalog
	// bundle (mcpCatalogNames: notion/atlassian/granola). Registered via
	// `pix mcp bundle` (or native `sbx mcp add`), authenticated via
	// `pix mcp auth <name>`. Never spawned or exec'd locally.
	mcpKindCatalog
	// mcpKindPackRemote: the active pack declares this integration with a
	// remote endpoint URL. Registered via `pix mcp register <name>` (the
	// pack carries the URL); auth is the same native hosted-control-plane
	// OAuth as the catalog. Never spawned or exec'd locally.
	mcpKindPackRemote
	// mcpKindPackContainer: the active pack declares this integration as an
	// OCI container (manifest or image) the gateway runs on the host.
	// Registered via `pix mcp register <name>`. Doctor never runs the
	// container itself, and there is no native OAuth to check.
	mcpKindPackContainer
	// mcpKindCustom: confirmed NON-local, not in the shipped catalog, and not
	// pack-declared. pix has no command that can register it (`register`
	// is local-only, `bundle` is catalog-only) — the honest repair is native
	// `sbx mcp add` with the server's own URL/transport.
	mcpKindCustom
)

// classifyMCPServer applies the partition above to one configured name. Pack
// declarations win first (the pack states its own kind explicitly; nothing is
// exec'd on their account here). Everything else needs the confirmed local set:
// when it could not be established the result is mcpKindUnknown — fail closed,
// never a guess.
func classifyMCPServer(name string, containers map[string]packContainer, localSet map[string]bool, localKnown bool) mcpKind {
	if c, ok := containers[name]; ok {
		if c.RemoteURL != "" {
			return mcpKindPackRemote
		}
		if c.Manifest != "" || c.Image != "" {
			return mcpKindPackContainer
		}
	}
	if !localKnown {
		return mcpKindUnknown
	}
	if localSet[name] {
		return mcpKindLocal
	}
	if mcpCatalogNames[name] {
		return mcpKindCatalog
	}
	return mcpKindCustom
}

// mcpRegisterTodo is the type-correct registration repair command for a kind.
// Only kinds pix can actually register get a pix command; a custom
// server gets pointed at native sbx (with its own URL/transport — pix
// cannot invent one); unknown gets NOTHING (no repair command is safe to
// recommend without knowing which kind of server this is).
func mcpRegisterTodo(name string, kind mcpKind) string {
	switch kind {
	case mcpKindLocal, mcpKindPackRemote, mcpKindPackContainer:
		return "pix mcp register " + name
	case mcpKindCatalog:
		return "pix mcp bundle"
	case mcpKindCustom:
		return "sbx mcp add --help"
	default:
		return ""
	}
}

// mcpAttachMode says how much sandbox-attachment context doctor has.
type mcpAttachMode int

const (
	// mcpAttachNone: no workspace sandbox context here (workspace or state dir
	// unresolvable). Doctor may report registration/auth and that configured
	// servers preload at sandbox create — never attachment.
	mcpAttachNone mcpAttachMode = iota
	// mcpAttachSandboxAbsent: the derived sandbox is POSITIVELY absent from a
	// successful bounded `sbx ls` — there is nothing to be attached to yet.
	mcpAttachSandboxAbsent
	// mcpAttachReceipt: a sandbox context exists; attachment evidence comes
	// from the launcher receipt (and only from it).
	mcpAttachReceipt
)

// mcpSandboxContext is the resolved workspace-sandbox context the attachment
// checks read. sandbox comes from the SAME hardened workspace->sandbox
// resolver `pix mcp load` uses (resolveWorkspaceSandbox: a unique
// trustworthy receipt mapping wins, else the derived default name), so doctor
// can never report on a differently-named sandbox than the one that verb acts
// on — including a custom-named `run --name pix-demo` box.
type mcpSandboxContext struct {
	mode    mcpAttachMode
	sandbox string
	// workspace is the canonical workspace path the context was resolved for —
	// what the exact `pix mcp load <name> <workspace>` repair command
	// carries. Empty when no workspace resolved (mcpAttachNone).
	workspace string
	// note carries the reason attachment could not be resolved at all (an
	// ambiguous workspace->sandbox mapping) for the group's attachment note.
	note    string
	receipt *sandboxMCPReceipt
	status  sandboxMCPStateStatus
}

// resolveMCPSandboxContext derives the current workspace's sandbox name and
// reads its launcher MCP receipt. Every step degrades to mcpAttachNone (report
// registration/auth only) rather than guessing:
//   - no getwd/stateDir seam, or either fails -> no sandbox context;
//   - a SUCCESSFUL bounded `sbx ls` that positively lacks the sandbox ->
//     mcpAttachSandboxAbsent (nothing exists to be attached to);
//   - a failed/timed-out `sbx ls` -> existence unknown; the receipt (a local,
//     offline record of past successful pix actions) is still consulted.
func resolveMCPSandboxContext(env shellEnv) mcpSandboxContext {
	if env.getwd == nil || env.stateDir == nil {
		return mcpSandboxContext{mode: mcpAttachNone}
	}
	ws, err := env.getwd()
	if err != nil || strings.TrimSpace(ws) == "" {
		return mcpSandboxContext{mode: mcpAttachNone}
	}
	sd, err := env.stateDir()
	if err != nil || strings.TrimSpace(sd) == "" {
		return mcpSandboxContext{mode: mcpAttachNone}
	}
	canonWS := canonicalWorkspacePath(ws)
	// The hardened workspace->sandbox resolver — the SAME one `mcp load` uses.
	// A unique trustworthy receipt mapping names the box (custom `run --name`);
	// a clean no-mapping scan falls back to the derived default; an AMBIGUOUS
	// mapping resolves nothing (never report on an arbitrary box). An
	// UNTRUSTED store falls back to the derived name for read-only reporting —
	// that box's own receipt state still governs rendering (a corrupt receipt
	// there renders unverifiable, never trusted).
	res := resolveWorkspaceSandbox(sd, ws)
	var name string
	switch res.Outcome {
	case workspaceSandboxMapped, workspaceSandboxDefault:
		name = res.Sandbox
	case workspaceSandboxAmbiguous:
		return mcpSandboxContext{mode: mcpAttachNone, workspace: canonWS,
			note: "workspace->sandbox mapping unresolvable: " + res.Detail}
	default: // workspaceSandboxUntrusted
		name = deriveSandboxName(ws)
	}
	// Bounded existence probe. Only a SUCCESSFUL listing may conclude "absent";
	// a failed or timed-out one proves nothing and must not erase the receipt
	// context.
	if out, timedOut, lerr := probeRun(env, "sbx", "ls"); lerr == nil && !timedOut {
		found := false
		for _, line := range strings.Split(out, "\n") {
			if f := strings.Fields(line); len(f) >= 1 && f[0] == name {
				found = true
				break
			}
		}
		if !found {
			return mcpSandboxContext{mode: mcpAttachSandboxAbsent, sandbox: name, workspace: canonWS}
		}
	}
	receipt, status, _ := readSandboxMCPReceipt(sd, name)
	return mcpSandboxContext{mode: mcpAttachReceipt, sandbox: name, workspace: canonWS, receipt: receipt, status: status}
}

// mcpLoadTodoCommand is the exact, copy-pasteable live-attach command for a
// VERIFIED registered-not-attached gap: the same `pix mcp load NAME DIR`
// spelling status emits, carrying the canonical workspace when known. It
// delegates to run.go's mcpLoadCommand (shell-quoting name and workspace via
// shellQuoteArg, closure finding #3) so doctor and status can never drift on
// how the repair command is quoted.
func mcpLoadTodoCommand(name, workspace string) string {
	return mcpLoadCommand(name, workspace)
}

// mcpAttachGuidance is the exact, copy-pasteable pair of commands that would
// MAKE attachment true (and receipted). It lives in the detail/evidence of an
// unverifiable attachment check — never as a todo, because unverifiable means
// doctor does not KNOW the server is unattached. name is shell-quoted via
// shellQuoteArg (closure finding #3), consistent with every other generated
// mcp load command.
func mcpAttachGuidance(name string) string {
	return "attach live with `pix mcp load " + shellQuoteArg(name) + "` or recreate with `pix run --replace`"
}

// mcpAttachCheck renders one server's sandbox-attachment evidence from the
// launcher receipt, via the SHARED join row (joinMCPSandboxRow, mcpjoin.go)
// so doctor and status derive attachment truth from ONE path. The receipt
// records SUCCESSFUL pix actions (writeCreateReceipt after a create,
// appendLoadReceipt after a live load) — that is the ONLY thing that may
// claim ready here. Config membership is never attachment. reg is the
// CURRENT registration tri-state (mcpRegEvidenceFrom) — a positive receipt
// claim renders ready REGARDLESS of reg (see mcpjoin.go's PRECEDENCE doc):
// registration is a separate, present-tense fact that cannot prove a
// sandbox was ever unloaded. Anything the receipt cannot vouch for (no
// entry, no receipt, or a receipt that is corrupt / wrong schema / wrong
// sandbox identity) is UNVERIFIABLE with the exact repair commands in the
// evidence — never a false claim in either direction.
func mcpAttachCheck(name string, ctx mcpSandboxContext, reg mcpRegEvidence) check {
	label := name + " attachment"
	guidance := mcpAttachGuidance(name)
	row := joinMCPSandboxRow(name, reg, ctx.sandbox, ctx.receipt, ctx.status)
	switch row.State {
	case mcpJoinPreloaded:
		return check{label: label, verdict: verdictReady,
			detail:   "preloaded by pix at create (sandbox " + ctx.sandbox + ")",
			evidence: row.Evidence}
	case mcpJoinLoaded:
		return check{label: label, verdict: verdictReady,
			detail:   "loaded by pix (pix mcp load, sandbox " + ctx.sandbox + ")",
			evidence: row.Evidence}
	case mcpJoinRegisteredNotAttached:
		// A POSITIVE, verified gap (registration confirmed + a COMPLETE valid
		// receipt with no entry): a verified OPTIONAL todo with the exact
		// live-attach command — consistent with status's row todo. Partial or
		// absent receipts never reach this state (they stay unverifiable).
		return check{label: label, verdict: verdictTodo,
			detail:   fmt.Sprintf("registered, but pix has no record of attaching it to %s; attach live, or recreate with `pix run --replace`", ctx.sandbox),
			todo:     mcpLoadTodoCommand(name, ctx.workspace),
			evidence: row.Evidence}
	case mcpJoinNotRegistered:
		return check{label: label, verdict: verdictUnverifiable,
			detail:   fmt.Sprintf("not currently registered, and the receipt has no positive claim for it either; attachment cannot be claimed for %s; %s", ctx.sandbox, guidance),
			evidence: row.Evidence}
	}
	// mcpJoinUnverifiable: the receipt is absent, untrustworthy, or PARTIAL
	// (valid but load-only — it proves only the loads it lists, so a name it
	// doesn't list is unverifiable, never "positively not attached"), or
	// registration itself is unknowable.
	if ctx.status == sandboxMCPStateOK && ctx.receipt.IsPartial() {
		return check{label: label, verdict: verdictUnverifiable,
			detail: fmt.Sprintf("launcher receipt for sandbox %s is partial (load-only, no create record); preload state unknown; %s",
				ctx.sandbox, guidance),
			evidence: row.Evidence}
	}
	if ctx.status == sandboxMCPStateAbsent {
		return check{label: label, verdict: verdictUnverifiable,
			detail:   fmt.Sprintf("no launcher receipt for sandbox %s; attachment unverified; %s", ctx.sandbox, guidance),
			evidence: row.Evidence}
	}
	if reg == mcpRegUnknown {
		return check{label: label, verdict: verdictUnverifiable,
			detail:   fmt.Sprintf("registration listing unavailable for sandbox %s; attachment unverified; %s", ctx.sandbox, guidance),
			evidence: row.Evidence}
	}
	return check{label: label, verdict: verdictUnverifiable,
		detail: fmt.Sprintf("launcher receipt for sandbox %s is %s; not trusting it; %s",
			ctx.sandbox, ctx.status, guidance),
		evidence: row.Evidence}
}

// mcpRegisteredIn reports registration truth from the bounded `sbx mcp ls`
// output ONLY — never from config, a receipt, or a definition inspection.
func mcpRegisteredIn(mcpOut, name string) bool {
	return grepWord(mcpOut, name)
}

// mcpUnavailableCheck is the shared degrade when the registration listing
// itself failed: sbx absent (in-sandbox) vs sbx present but the gateway
// listing failing (daemon unhealthy). Doctor knows NOTHING about this server
// then — unverifiable, no repair command.
func mcpUnavailableCheck(name string, sbxPresent bool) check {
	if sbxPresent {
		return check{label: name, verdict: verdictUnverifiable, detail: gatewayDownDetail}
	}
	return check{label: name, verdict: verdictUnverifiable,
		detail: "sbx unavailable here; registration cannot be verified (check from the host)"}
}

// mcpNotRegisteredCheck is a POSITIVELY VERIFIED registration gap (the bounded
// `sbx mcp ls` succeeded and lacks the name) with the type-correct repair.
// Always optional: an MCP server is an integration, never core.
func mcpNotRegisteredCheck(name string, kind mcpKind) check {
	detail := "not registered"
	switch kind {
	case mcpKindCatalog:
		detail = "not registered (shipped remote catalog server: register with `pix mcp bundle`, then `pix mcp auth " + name + "`)"
	case mcpKindPackRemote, mcpKindPackContainer:
		detail = "not registered (pack integration: register with `pix mcp register " + name + "`)"
	case mcpKindCustom:
		detail = "not registered; a custom server pix cannot register for you " +
			"(`pix mcp register` is local-stdio-only; `pix mcp bundle` covers only " +
			mcpCatalogSummary() + "). Register it natively with its own URL/transport: sbx mcp add"
	}
	return check{label: name, verdict: verdictTodo, detail: detail, todo: mcpRegisterTodo(name, kind)}
}

// mcpUnknownKindCheck is the fail-closed rendering when classification itself
// could not be established (`pix-host mcp --list` unavailable): doctor
// must not guess local vs remote, so there is no probe, no exec, and NO repair
// command — none is safe to recommend.
func mcpUnknownKindCheck(name, mcpOut string, mcpOK, sbxPresent bool) check {
	if !mcpOK {
		return mcpUnavailableCheck(name, sbxPresent)
	}
	det := "could not determine whether this is a local stdio server or a remote one " +
		"(pix-host mcp --list unavailable); no repair command can be safely recommended; " +
		"build/resolve pix-host, then re-run"
	if mcpRegisteredIn(mcpOut, name) {
		det = "registered; " + det
	} else {
		det = "not seen in `sbx mcp ls`; " + det
	}
	return check{label: name, verdict: verdictUnverifiable, detail: det, evidence: "classification unknown"}
}

// mcpLocalCheck is the HONEST local stdio check: registered -> spawns ->
// returns N tools. It reads the definition sbx ACTUALLY registered for <name>
// (bounded) and probes THAT — but ONLY after the canonical-executable gate:
// the registered argv is never exec'd unless recognizedMCPArgv approves and
// normalizes it (see its doc). Outcomes degrade honestly: unreadable command
// or untrusted shape -> unverifiable (registration stays stated, never a false
// green health claim); a timeout/exec failure -> unverifiable; a clean spawn
// with zero tools -> a verified headless-creds TODO.
func mcpLocalCheck(env shellEnv, name, mcpOut string) check {
	argv, ok := registeredMCPCommand(env, name)
	if !ok {
		return check{label: name, verdict: verdictUnverifiable,
			detail: "registered (tool probe unavailable: couldn't read the registered command)"}
	}
	trusted, ok := recognizedMCPArgv(env, argv, name)
	if !ok {
		return check{label: name, verdict: verdictUnverifiable,
			detail: "registered (probe skipped: unrecognized/untrusted command, never executed; inspect: sbx mcp get " + name + ")"}
	}
	res := probeListTools(env, trusted)
	switch res.status {
	case probeToolsOK:
		return check{label: name, verdict: verdictReady,
			detail: fmt.Sprintf("registered, spawns %s", plural(res.tools, "tool"))}
	case probeNoTools:
		return check{label: name, verdict: verdictTodo,
			detail: "registered but the spawned command returns 0 tools — headless creds/keyring",
			todo:   "review the registered command: sbx mcp get " + name}
	case probeDeniedByPolicy:
		// An EXPLICIT policy/permission refusal is a positive denial — an org
		// decision, not a setup gap, and never collapsed into unverifiable.
		return check{label: name, verdict: verdictDenied,
			detail:   "registered, but the spawn is positively refused by policy/permission (sbx mcp get " + name + ")",
			evidence: "denied"}
	default: // probeTimedOut / probeError
		return check{label: name, verdict: verdictUnverifiable,
			detail: "registered but the tool probe " + res.detail + "; could not verify"}
	}
}

// mcpRemoteAuthCheck is the remote-server auth axis: registration is already
// confirmed by the caller, and authorization may be claimed ONLY from a
// bounded native `sbx mcp auth status <name>` probe. Outcomes:
//   - a positive status -> ready ("registered, authorized");
//   - a bare 401/unauthorized/not-authenticated -> a verified auth TODO with
//     the exact `pix mcp auth <name>` command;
//   - an EXPLICIT policy/forbidden/access-denied signal -> denied (an org
//     decision, not a setup gap — no setup command can fix it);
//   - a timeout or transport/exec failure -> unverifiable, never a guess.
func mcpRemoteAuthCheck(env shellEnv, name string) check {
	out, timedOut, err := probeRun(env, "sbx", "mcp", "auth", "status", name)
	if timedOut {
		return check{label: name, verdict: verdictUnverifiable,
			detail: "registered; auth status timed out (sbx mcp auth status " + name + "); could not verify"}
	}
	// EXPLICIT denial signals win regardless of exit code: a policy denial is
	// a positive refusal, not a credential gap.
	if classifyProbeFailure(out, err) == probeDenied {
		return check{label: name, verdict: verdictDenied,
			detail:   "registered, but access is denied by policy (sbx mcp auth status " + name + ")",
			evidence: "denied"}
	}
	if err != nil {
		if classifyProbeFailure(out, err) == probeAuthTodo {
			return check{label: name, verdict: verdictTodo,
				detail: "registered but not authorized", todo: "pix mcp auth " + name}
		}
		return check{label: name, verdict: verdictUnverifiable,
			detail: "registered; auth status could not be verified (sbx mcp auth status " + name + ")"}
	}
	switch mcpAuthStatus(out) {
	case mcpAuthOK:
		return check{label: name, verdict: verdictReady, detail: "registered, authorized",
			evidence: "sbx mcp auth status " + name}
	case mcpAuthFailed:
		return check{label: name, verdict: verdictTodo,
			detail: "registered but not authorized", todo: "pix mcp auth " + name}
	default: // mcpAuthUnknown
		return check{label: name, verdict: verdictUnverifiable,
			detail: "registered; auth status unclear (sbx mcp auth status " + name + "); could not verify"}
	}
}

// mcpServerChecks builds the check line(s) for one configured/pack server:
// the registration+health line, plus (whenever a sandbox receipt context
// exists) the receipt-backed attachment line — the attachment line is added
// REGARDLESS of the registration outcome above it, because a valid receipt's
// positive claim must remain visible even when the server reads as currently
// unregistered or unclassifiable (mcpjoin.go's PRECEDENCE): registration
// truth is a separate, present-tense fact that never proves the sandbox was
// unloaded.
func mcpServerChecks(env shellEnv, name string, kind mcpKind, mcpOut string, mcpOK, sbxPresent bool, ctx mcpSandboxContext) []check {
	reg := mcpRegEvidenceFrom(mcpOut, mcpOK, name)
	attach := func(cks []check) []check {
		if ctx.mode == mcpAttachReceipt {
			cks = append(cks, mcpAttachCheck(name, ctx, reg))
		}
		return cks
	}
	if kind == mcpKindUnknown {
		// Fail closed BEFORE any probing: unknown classification never reads or
		// execs the registered definition and never picks a repair command.
		return attach([]check{mcpUnknownKindCheck(name, mcpOut, mcpOK, sbxPresent)})
	}
	if !mcpOK {
		return attach([]check{mcpUnavailableCheck(name, sbxPresent)})
	}
	if !mcpRegisteredIn(mcpOut, name) {
		return attach([]check{mcpNotRegisteredCheck(name, kind)})
	}
	var out []check
	switch kind {
	case mcpKindLocal:
		out = append(out, mcpLocalCheck(env, name, mcpOut))
	case mcpKindCatalog, mcpKindPackRemote:
		out = append(out, mcpRemoteAuthCheck(env, name))
	default: // mcpKindPackContainer, mcpKindCustom
		// Registration is confirmed; there is nothing further doctor can
		// honestly verify here (no trusted local spawn, no native OAuth).
		out = append(out, check{label: name, verdict: verdictReady, detail: "registered",
			evidence: "sbx mcp ls"})
	}
	return attach(out)
}

// retiredKeyCheck surfaces a stale retired config key (mcp_static/mcp_dynamic,
// the removed eager/lazy split): a verified, OPTIONAL leftover — the key is
// accepted-and-ignored, and the next config mutation (`pix config
// set`/`unset`) rewrites config.toml without it. No todo command: any
// mutation drops it, and there is nothing to "fix" beyond that.
func retiredKeyCheck(key string) check {
	return check{
		label:   "config " + key,
		verdict: verdictTodo,
		detail: "retired config key; ignored (every configured MCP server now preloads at sandbox create); " +
			"the next `pix config set`/`unset` rewrite drops it from config.toml",
		evidence: "retired key present in config.toml",
	}
}

// unknownKeyCheck surfaces an unrecognized config key — softer than a retired
// one: doctor cannot tell a typo from a newer pix's key, so this is
// unverifiable info, never a verified failure.
func unknownKeyCheck(key string) check {
	return check{
		label:    "config " + key,
		verdict:  verdictUnverifiable,
		detail:   "unknown config key; ignored (a typo, or a key only a newer pix understands)",
		evidence: "unknown key present in config.toml",
	}
}

// registeredMCPCommand asks sbx for the DEFINITION actually registered for
// <name> — the argv the gateway would spawn — so doctor can probe the real
// registration for a local stdio server. Definition inspection ONLY: nothing
// here says anything about sandbox attachment (that is the receipt's job).
// It tries `sbx mcp get <name>` then `sbx mcp ls -o json`, both BOUNDED via
// probeRun so a hung sbx degrades to "couldn't read the registered command",
// never a wedged doctor. Returns (nil,false) when sbx is absent or exposes no
// command.
func registeredMCPCommand(env shellEnv, name string) ([]string, bool) {
	if env.lookPath == nil {
		return nil, false
	}
	if _, err := env.lookPath("sbx"); err != nil {
		return nil, false
	}
	if out, timedOut, err := probeRun(env, "sbx", "mcp", "get", name); err == nil && !timedOut {
		if argv, ok := parseMCPCommandLine(out); ok {
			return argv, true
		}
	}
	if out, timedOut, err := probeRun(env, "sbx", "mcp", "ls", "-o", "json"); err == nil && !timedOut {
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
func recognizedMCPArgv(env shellEnv, argv []string, name string) ([]string, bool) {
	if norm, ok := trustedGogSpawn(env, argv); ok {
		return norm, true
	}
	// Unwrap ONLY the exact launcher-generated `op run --no-masking
	// --env-file=<refs> --` wrapper grammar (unwrapOpRun). A `--` behind any
	// other prefix — a foreign argv[0], another op subcommand, an alternate
	// env file, extra options — is rejected: the probe execs these tokens.
	cmd, ok := unwrapOpRun(env, argv)
	if !ok {
		return nil, false
	}
	norm := append([]string(nil), argv...)
	innerStart := len(argv) - len(cmd)
	if innerStart > 0 {
		// An op-wrapped command must run the SAME op binary env.lookPath finds —
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
	hostTok, hostOK := trustedHostBinaryExecPath(env, cmd[0])
	if !hostOK {
		return nil, false
	}
	norm[innerStart] = hostTok
	return norm, true
}

// trustedHostBinaryExecPath is the canonical-pix-host gate: mcp.go
// registration (registerServers/serverCmd) ALWAYS spawns the ABSOLUTE path
// hostBinaryResolver (findHostBinary) resolves — never a bare name. Trusting
// an absolute path's basename alone would let a malicious
// `/tmp/malicious/pix-host mcp slack` registration pass. env.hostBinary
// is the injected/hermetic trust seam mirroring hostBinaryResolver, so this
// compares against the SAME canonical answer the real registration used. tok
// must be absolute AND byte-equal (cleaned) to the resolved binary — STRICT
// equality only. Symlink resolution is deliberately NOT consulted: blessing an
// alternate symlink path at check time and exec'ing it afterwards is a
// check-then-exec race an attacker wins by swapping the link between the two.
// On success it returns the RESOLVER's canonical token — the only thing the
// caller may exec. An unresolvable canonical answer (env.hostBinary nil or
// erroring) fails CLOSED: never fall back to trusting the basename alone.
func trustedHostBinaryExecPath(env shellEnv, tok string) (string, bool) {
	if filepath.Base(tok) != "pix-host" {
		return "", false
	}
	if !filepath.IsAbs(tok) {
		return "", false // never trust a bare/relative name for pix-host
	}
	if env.hostBinary == nil {
		return "", false
	}
	canonical, err := env.hostBinary()
	if err != nil || canonical == "" || !filepath.IsAbs(canonical) {
		return "", false
	}
	if filepath.Clean(tok) != filepath.Clean(canonical) {
		return "", false
	}
	return filepath.Clean(canonical), true
}

// trustedExecPath, trustedGogSpawn, probeStatus/probeResult, probeListTools,
// and classifyProbeErr are SHARED with doctor_gog.go — see doctor_probe.go,
// which owns the single implementation. mcpLocalCheck maps the shared
// probeDeniedByPolicy outcome to verdictDenied (an explicit policy refusal is
// a positive denial, same as the remote auth axis), and everything else
// unclassified to unverifiable.

// mcpAuthResult is the outcome mcpAuthStatus classifies a `sbx mcp auth
// status <name>` probe into. mcpAuthUnknown covers output doctor cannot
// confidently parse as either a pass or a fail — it must never guess (a
// misread failure would recommend a repair command that doesn't apply, and a
// misread success would silently hide a real auth gap).
type mcpAuthResult int

const (
	mcpAuthUnknown mcpAuthResult = iota
	mcpAuthOK
	mcpAuthFailed
)

// mcpAuthStatus parses `sbx mcp auth status <name>` output (name-scoped: sbx
// prints only this server's state) into the tri-state above. It is
// deliberately lenient about exact wording (this is a passthrough to sbx, not
// a format pix controls — see runMcpAuth) but conservative about
// ambiguity: a negative phrase anywhere wins over a positive one, and neither
// present at all is unknown rather than a guess.
func mcpAuthStatus(out string) mcpAuthResult {
	lower := strings.ToLower(out)
	for _, neg := range []string{"not authenticated", "unauthenticated", "not authorized", "unauthorized", "needs auth", "not logged in", "expired", "no token", "401"} {
		if strings.Contains(lower, neg) {
			return mcpAuthFailed
		}
	}
	for _, pos := range []string{"authenticated", "authorized", "logged in", " ok", "\tok"} {
		if strings.Contains(lower, pos) {
			return mcpAuthOK
		}
	}
	if strings.TrimSpace(lower) == "ok" {
		return mcpAuthOK
	}
	return mcpAuthUnknown
}

// mcpGroup builds the MCP-servers cluster for every configured server plus
// every active-pack integration server. gog is DELIBERATELY skipped — the
// dedicated gog group already owns its registration check + TODO, so probing
// it again would emit a duplicate `pix mcp register`.
func mcpGroup(cfg *config.Config, env shellEnv, mcpOut string, mcpOK, sbxPresent bool, ctx mcpSandboxContext) group {
	return mcpGroupWith(cfg, env, mcpOut, mcpOK, sbxPresent,
		activeContainerMCP(cfg), ctx)
}

// mcpGroupWith is mcpGroup with the pack-integration set and the sandbox
// context injected, so tests drive both hermetically.
func mcpGroupWith(cfg *config.Config, env shellEnv, mcpOut string, mcpOK, sbxPresent bool,
	containers map[string]packContainer, ctx mcpSandboxContext) group {

	mcp := group{title: "MCP servers (via the sbx gateway)"}

	// The server set: configured servers first (order preserved), then any
	// pack-declared integration servers not already configured, then — when a
	// sandbox receipt context exists — any name the receipt independently
	// proves provenance for that current intent doesn't already name
	// (Preloaded, then Loads; receipt order; deduped). This keeps a transient
	// `run --pack` mix-in or a since-switched pack's historical MCP
	// provenance visible on THIS sandbox. gog is excluded throughout (owned
	// by its own dedicated group).
	exclude := map[string]bool{gwServerName: true}
	currentIntent := mcpCurrentIntentNames(cfg.MCP, containers, exclude)
	var receipt *sandboxMCPReceipt
	if ctx.mode == mcpAttachReceipt {
		receipt = ctx.receipt
	}
	names, receiptOnly := mcpConfiguredUniverse(currentIntent, receipt, exclude)

	if len(names) == 0 {
		mcp.checks = append(mcp.checks, check{
			label:   "(none configured)",
			note:    true,
			verdict: verdictUnverifiable,
			detail:  "add servers with `pix config set mcp <server>`",
		})
	} else {
		// Classification source of truth: the same `pix-host mcp --list`
		// registration itself uses. Bounded inside localMCPNames.
		localSet, localKnown := localMCPNames(env, env.hostBinary)
		anyRegistered := false
		for _, m := range names {
			kind := classifyMCPServer(m, containers, localSet, localKnown)
			cks := mcpServerChecks(env, m, kind, mcpOut, mcpOK, sbxPresent, ctx)
			if receiptOnly[m] {
				for i := range cks {
					cks[i] = annotateReceiptOnlyCheck(cks[i], m)
				}
			}
			mcp.checks = append(mcp.checks, cks...)
			if mcpOK && kind != mcpKindUnknown && mcpRegisteredIn(mcpOut, m) {
				anyRegistered = true
			}
		}
		// Without a workspace sandbox context doctor reports registration/auth
		// only — plus the honest statement of what pix WILL do (preload at
		// create), which is intent, never attachment.
		if anyRegistered && ctx.mode != mcpAttachReceipt {
			det := "no workspace sandbox context here; reporting registration/auth only; configured servers preload at sandbox create"
			if ctx.mode == mcpAttachSandboxAbsent {
				det = "sandbox " + ctx.sandbox + " not created yet; configured servers preload at `pix run` create"
			} else if ctx.note != "" {
				det = ctx.note + "; reporting registration/auth only"
			}
			mcp.checks = append(mcp.checks, check{label: "attachment", note: true, verdict: verdictUnverifiable, detail: det})
		}
	}

	// Config hygiene: stale retired keys (the removed mcp_static/mcp_dynamic
	// split) are a verified optional leftover; unknown keys are softer info.
	for _, k := range cfg.RetiredKeys() {
		mcp.checks = append(mcp.checks, retiredKeyCheck(k))
	}
	for _, k := range cfg.UnknownKeys() {
		mcp.checks = append(mcp.checks, unknownKeyCheck(k))
	}
	return mcp
}

// annotateReceiptOnlyCheck labels a check for a name that is NOT part of the
// current cfg.MCP/pack-integration intent but appears solely because this
// sandbox's own receipt proves it was preloaded or loaded here — sandbox
// PROVENANCE, never current preload intent. Evidence-only: it never changes
// verdict, label, or todo.
func annotateReceiptOnlyCheck(c check, name string) check {
	note := "sandbox provenance only (from this sandbox's receipt); " + name + " is not part of the current cfg.MCP/pack"
	c.evidence = c.evidenceString() + "; " + note
	return c
}

// packContainerNames returns the pack-integration server names in a stable
// (sorted) order, so the group renders deterministically.
func packContainerNames(containers map[string]packContainer) []string {
	if len(containers) == 0 {
		return nil
	}
	names := make([]string, 0, len(containers))
	for n := range containers {
		names = append(names, n)
	}
	// small n; insertion sort avoids an import for sort in this file's diff
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	return names
}
