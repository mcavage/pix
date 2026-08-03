package main

import (
	"fmt"
	"pix/host/cli"
	"pix/host/hostenv"
	"pix/host/mcp"
	"pix/host/readiness"
	"pix/host/readiness/axis"
	"pix/host/secret"
	"pix/host/sys"
	"pix/host/workflow/pack"
	"strings"

	"pix/host/config"
	"pix/host/workspace"
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
// tri-state evidence every other MCP server uses (mcp.McpRegEvidenceFrom): a
// successful `sbx mcp ls` positively lacking gog is a verified register TODO;
// gog present is ready; a failed/absent listing is UNVERIFIABLE — never a
// false outstanding item invented from a probe that answered nothing. This
// replaces the legacy binary mcpCheck, which rendered every listing failure
// as a TODO.
func gogRegistrationCheck(mcpOut string, mcpOK, sbxPresent bool) readiness.Check {
	switch mcp.McpRegEvidenceFrom(mcpOut, mcpOK, config.GWServerName) {
	case mcp.McpRegYes:
		return readiness.Check{Label: config.GWServerName, Verdict: readiness.VerdictReady, Detail: "registered", Evidence: "sbx mcp ls"}
	case mcp.McpRegNo:
		return readiness.Check{Label: config.GWServerName, Verdict: readiness.VerdictTodo, Detail: "not registered", Todo: "pix mcp register"}
	default: // mcp.McpRegUnknown: sbx absent, or present with the listing failing
		return mcpUnavailableCheck(config.GWServerName, sbxPresent)
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
	// the canonical-executable gate (mcp.RecognizedArgv). Never OAuth-checked.
	mcpKindLocal
	// mcpKindCatalog: confirmed NON-local and in the shipped public catalog
	// bundle (mcp.McpCatalogNames: notion/atlassian/granola). Registered via
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
func classifyMCPServer(name string, containers map[string]config.MCPContainer, localSet map[string]bool, localKnown bool) mcpKind {
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
	if mcp.McpCatalogNames[name] {
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
// resolver `pix mcp load` uses (workspace.ResolveSandbox: a unique
// trustworthy receipt mapping wins, else the derived default name), so doctor
// can never report on a differently-named sandbox than the one that verb acts
// on — including a custom-named `run --name pix-demo` box.
type mcpSandboxContext struct {
	mode    mcpAttachMode
	sandbox string
	// workspace is the canonical workspace path the context was resolved for —
	// what the exact `pix mcp load <name> <workspace>` repair command
	// carries. Empty when no workspace resolved (mcpAttachNone).
	ws string
	// note carries the reason attachment could not be resolved at all (an
	// ambiguous workspace->sandbox mapping) for the group's attachment note.
	note    string
	receipt *workspace.MCPReceipt
	status  workspace.MCPStateStatus
}

// resolveMCPSandboxContext derives the current workspace's sandbox name and
// reads its launcher MCP receipt. Every step degrades to mcpAttachNone (report
// registration/auth only) rather than guessing:
//   - no getwd/stateDir seam, or either fails -> no sandbox context;
//   - a SUCCESSFUL bounded `sbx ls` that positively lacks the sandbox ->
//     mcpAttachSandboxAbsent (nothing exists to be attached to);
//   - a failed/timed-out `sbx ls` -> existence unknown; the receipt (a local,
//     offline record of past successful pix actions) is still consulted.
func resolveMCPSandboxContext(env hostenv.Env) mcpSandboxContext {

	ws, err := env.Getwd()
	if err != nil || strings.TrimSpace(ws) == "" {
		return mcpSandboxContext{mode: mcpAttachNone}
	}
	sd, err := env.StateDir()
	if err != nil || strings.TrimSpace(sd) == "" {
		return mcpSandboxContext{mode: mcpAttachNone}
	}
	canonWS := workspace.CanonicalPath(ws)
	// The hardened workspace->sandbox resolver — the SAME one `mcp load` uses.
	// A unique trustworthy receipt mapping names the box (custom `run --name`);
	// a clean no-mapping scan falls back to the derived default; an AMBIGUOUS
	// mapping resolves nothing (never report on an arbitrary box). An
	// UNTRUSTED store falls back to the derived name for read-only reporting —
	// that box's own receipt state still governs rendering (a corrupt receipt
	// there renders unverifiable, never trusted).
	res := workspace.ResolveSandbox(sd, ws)
	var name string
	switch res.Outcome {
	case workspace.SandboxMapped, workspace.SandboxDefault:
		name = res.Sandbox
	case workspace.SandboxAmbiguous:
		return mcpSandboxContext{mode: mcpAttachNone, ws: canonWS,
			note: "workspace->sandbox mapping unresolvable: " + res.Detail}
	default: // workspace.WorkspaceSandboxUntrusted
		name = workspace.DeriveSandboxName(ws)
	}
	// Bounded existence probe. Only a SUCCESSFUL listing may conclude "absent";
	// a failed or timed-out one proves nothing and must not erase the receipt
	// context.
	if out, timedOut, lerr := env.RunTimed("sbx", "ls"); lerr == nil && !timedOut {
		found := false
		for _, line := range strings.Split(out, "\n") {
			if f := strings.Fields(line); len(f) >= 1 && f[0] == name {
				found = true
				break
			}
		}
		if !found {
			return mcpSandboxContext{mode: mcpAttachSandboxAbsent, sandbox: name, ws: canonWS}
		}
	}
	receipt, status, _ := workspace.ReadMCPReceipt(sd, name)
	return mcpSandboxContext{mode: mcpAttachReceipt, sandbox: name, ws: canonWS, receipt: receipt, status: status}
}

// mcpLoadTodoCommand is the exact, copy-pasteable live-attach command for a
// VERIFIED registered-not-attached gap: the same `pix mcp load NAME DIR`
// spelling status emits, carrying the canonical workspace when known. It
// delegates to run.go's mcpLoadCommand (shell-quoting name and workspace via
// sys.ShellQuote, closure finding #3) so doctor and status can never drift on
// how the repair command is quoted.
func mcpLoadTodoCommand(name, ws string) string {
	return mcpLoadCommand(name, ws)
}

// mcpAttachCheck renders one server's sandbox-attachment evidence from the
// launcher receipt, via the SHARED join row (mcp.JoinMCPSandboxRow, mcpjoin.go)
// so doctor and status derive attachment truth from ONE path. The receipt
// records SUCCESSFUL pix actions (workspace.WriteCreateReceipt after a create,
// workspace.AppendLoadReceipt after a live load) — that is the ONLY thing that may
// claim ready here. Config membership is never attachment. reg is the
// CURRENT registration tri-state (mcp.McpRegEvidenceFrom) — a positive receipt
// claim renders ready REGARDLESS of reg (see mcpjoin.go's PRECEDENCE doc):
// registration is a separate, present-tense fact that cannot prove a
// sandbox was ever unloaded. Anything the receipt cannot vouch for (no
// entry, no receipt, or a receipt that is corrupt / wrong schema / wrong
// sandbox identity) is UNVERIFIABLE with the exact repair commands in the
// evidence — never a false claim in either direction.
func mcpAttachCheck(name string, ctx mcpSandboxContext, reg mcp.McpRegEvidence) readiness.Check {
	label := name + " attachment"
	guidance := mcp.McpAttachGuidance(name)
	row := mcp.JoinMCPSandboxRow(name, reg, ctx.sandbox, ctx.receipt, ctx.status)
	switch row.State {
	case mcp.McpJoinPreloaded:
		return readiness.Check{Label: label, Verdict: readiness.VerdictReady,
			Detail:   "preloaded by pix at create (sandbox " + ctx.sandbox + ")",
			Evidence: row.Evidence}
	case mcp.McpJoinLoaded:
		return readiness.Check{Label: label, Verdict: readiness.VerdictReady,
			Detail:   "loaded by pix (pix mcp load, sandbox " + ctx.sandbox + ")",
			Evidence: row.Evidence}
	case mcp.McpJoinRegisteredNotAttached:
		// A POSITIVE, verified gap (registration confirmed + a COMPLETE valid
		// receipt with no entry): a verified OPTIONAL todo with the exact
		// live-attach command — consistent with status's row todo. Partial or
		// absent receipts never reach this state (they stay unverifiable).
		return readiness.Check{Label: label, Verdict: readiness.VerdictTodo,
			Detail:   fmt.Sprintf("registered, but pix has no record of attaching it to %s; attach live, or recreate with `pix run --replace`", ctx.sandbox),
			Todo:     mcpLoadTodoCommand(name, ctx.ws),
			Evidence: row.Evidence}
	case mcp.McpJoinNotRegistered:
		return readiness.Check{Label: label, Verdict: readiness.VerdictUnverifiable,
			Detail:   fmt.Sprintf("not currently registered, and the receipt has no positive claim for it either; attachment cannot be claimed for %s; %s", ctx.sandbox, guidance),
			Evidence: row.Evidence}
	}
	// mcp.McpJoinUnverifiable: the receipt is absent, untrustworthy, or PARTIAL
	// (valid but load-only — it proves only the loads it lists, so a name it
	// doesn't list is unverifiable, never "positively not attached"), or
	// registration itself is unknowable.
	if ctx.status == workspace.MCPStateOK && ctx.receipt.IsPartial() {
		return readiness.Check{Label: label, Verdict: readiness.VerdictUnverifiable,
			Detail: fmt.Sprintf("launcher receipt for sandbox %s is partial (load-only, no create record); preload state unknown; %s",
				ctx.sandbox, guidance),
			Evidence: row.Evidence}
	}
	if ctx.status == workspace.MCPStateAbsent {
		return readiness.Check{Label: label, Verdict: readiness.VerdictUnverifiable,
			Detail:   fmt.Sprintf("no launcher receipt for sandbox %s; attachment unverified; %s", ctx.sandbox, guidance),
			Evidence: row.Evidence}
	}
	if reg == mcp.McpRegUnknown {
		return readiness.Check{Label: label, Verdict: readiness.VerdictUnverifiable,
			Detail:   fmt.Sprintf("registration listing unavailable for sandbox %s; attachment unverified; %s", ctx.sandbox, guidance),
			Evidence: row.Evidence}
	}
	return readiness.Check{Label: label, Verdict: readiness.VerdictUnverifiable,
		Detail: fmt.Sprintf("launcher receipt for sandbox %s is %s; not trusting it; %s",
			ctx.sandbox, ctx.status, guidance),
		Evidence: row.Evidence}
}

// mcpUnavailableCheck is the shared degrade when the registration listing
// itself failed: sbx absent (in-sandbox) vs sbx present but the gateway
// listing failing (daemon unhealthy). Doctor knows NOTHING about this server
// then — unverifiable, no repair command.
func mcpUnavailableCheck(name string, sbxPresent bool) readiness.Check {
	if sbxPresent {
		return readiness.Check{Label: name, Verdict: readiness.VerdictUnverifiable, Detail: gatewayDownDetail}
	}
	return readiness.Check{Label: name, Verdict: readiness.VerdictUnverifiable,
		Detail: "sbx unavailable here; registration cannot be verified (check from the host)"}
}

// mcpNotRegisteredCheck is a POSITIVELY VERIFIED registration gap (the bounded
// `sbx mcp ls` succeeded and lacks the name) with the type-correct repair.
// Always optional: an MCP server is an integration, never core.
func mcpNotRegisteredCheck(name string, kind mcpKind) readiness.Check {
	detail := "not registered"
	switch kind {
	case mcpKindCatalog:
		detail = "not registered (shipped remote catalog server: register with `pix mcp bundle`, then `pix mcp auth " + name + "`)"
	case mcpKindPackRemote, mcpKindPackContainer:
		detail = "not registered (pack integration: register with `pix mcp register " + name + "`)"
	case mcpKindCustom:
		detail = "not registered; a custom server pix cannot register for you " +
			"(`pix mcp register` is local-stdio-only; `pix mcp bundle` covers only " +
			mcp.McpCatalogSummary() + "). Register it natively with its own URL/transport: sbx mcp add"
	}
	return readiness.Check{Label: name, Verdict: readiness.VerdictTodo, Detail: detail, Todo: mcpRegisterTodo(name, kind)}
}

// mcpUnknownKindCheck is the fail-closed rendering when classification itself
// could not be established (`pix-host mcp --list` unavailable): doctor
// must not guess local vs remote, so there is no probe, no exec, and NO repair
// command — none is safe to recommend.
func mcpUnknownKindCheck(name, mcpOut string, mcpOK, sbxPresent bool) readiness.Check {
	if !mcpOK {
		return mcpUnavailableCheck(name, sbxPresent)
	}
	det := "could not determine whether this is a local stdio server or a remote one " +
		"(pix-host mcp --list unavailable); no repair command can be safely recommended; " +
		"build/resolve pix-host, then re-run"
	if mcp.McpRegisteredIn(mcpOut, name) {
		det = "registered; " + det
	} else {
		det = "not seen in `sbx mcp ls`; " + det
	}
	return readiness.Check{Label: name, Verdict: readiness.VerdictUnverifiable, Detail: det, Evidence: "classification unknown"}
}

// mcpLocalCheck is the HONEST local stdio check: registered -> spawns ->
// returns N tools. It reads the definition sbx ACTUALLY registered for <name>
// (bounded) and probes THAT — but ONLY after the canonical-executable gate:
// the registered argv is never exec'd unless mcp.RecognizedArgv approves and
// normalizes it (see its doc). Outcomes degrade honestly: unreadable command
// or untrusted shape -> unverifiable (registration stays stated, never a false
// green health claim); a timeout/exec failure -> unverifiable; a clean spawn
// with zero tools -> a verified headless-creds TODO.
func mcpLocalCheck(env hostenv.Env, name, mcpOut string) readiness.Check {
	argv, ok := mcp.RegisteredCommand(env, name)
	if !ok {
		return readiness.Check{Label: name, Verdict: readiness.VerdictUnverifiable,
			Detail: "registered (tool probe unavailable: couldn't read the registered command)"}
	}
	trusted, ok := mcp.RecognizedArgv(env, argv, name, secret.FindOpRefs(env))
	if !ok {
		return readiness.Check{Label: name, Verdict: readiness.VerdictUnverifiable,
			Detail: "registered (probe skipped: unrecognized/untrusted command, never executed; inspect: sbx mcp inspect " + name + ")"}
	}
	res := axis.ProbeListTools(env, trusted)
	switch res.Status {
	case axis.ProbeToolsOK:
		return readiness.Check{Label: name, Verdict: readiness.VerdictReady,
			Detail: fmt.Sprintf("registered, spawns %s", cli.Plural(res.Tools, "tool"))}
	case axis.ProbeNoTools:
		return readiness.Check{Label: name, Verdict: readiness.VerdictTodo,
			Detail: "registered but the spawned command returns 0 tools — headless creds/keyring",
			Todo:   "review the registered command: sbx mcp inspect " + name}
	case axis.ProbeDeniedByPolicy:
		// An EXPLICIT policy/permission refusal is a positive denial — an org
		// decision, not a setup gap, and never collapsed into unverifiable.
		return readiness.Check{Label: name, Verdict: readiness.VerdictDenied,
			Detail:   "registered, but the spawn is positively refused by policy/permission (sbx mcp inspect " + name + ")",
			Evidence: "denied"}
	default: // probeTimedOut / axis.ProbeError
		return readiness.Check{Label: name, Verdict: readiness.VerdictUnverifiable,
			Detail: "registered but the tool probe " + res.Detail + "; could not verify"}
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
func mcpRemoteAuthCheck(env hostenv.Env, name string) readiness.Check {
	out, timedOut, err := env.RunTimed("sbx", "mcp", "auth", "status", name)
	if timedOut {
		return readiness.Check{Label: name, Verdict: readiness.VerdictUnverifiable,
			Detail: "registered; auth status timed out (sbx mcp auth status " + name + "); could not verify"}
	}
	// EXPLICIT denial signals win regardless of exit code: a policy denial is
	// a positive refusal, not a credential gap.
	if sys.ClassifyProbeFailure(out, err) == sys.ProbeDenied {
		return readiness.Check{Label: name, Verdict: readiness.VerdictDenied,
			Detail:   "registered, but access is denied by policy (sbx mcp auth status " + name + ")",
			Evidence: "denied"}
	}
	if err != nil {
		if sys.ClassifyProbeFailure(out, err) == sys.ProbeAuthTodo {
			return readiness.Check{Label: name, Verdict: readiness.VerdictTodo,
				Detail: "registered but not authorized", Todo: "pix mcp auth " + name}
		}
		return readiness.Check{Label: name, Verdict: readiness.VerdictUnverifiable,
			Detail: "registered; auth status could not be verified (sbx mcp auth status " + name + ")"}
	}
	switch mcp.McpAuthStatus(out) {
	case mcp.McpAuthOK:
		return readiness.Check{Label: name, Verdict: readiness.VerdictReady, Detail: "registered, authorized",
			Evidence: "sbx mcp auth status " + name}
	case mcp.McpAuthFailed:
		return readiness.Check{Label: name, Verdict: readiness.VerdictTodo,
			Detail: "registered but not authorized", Todo: "pix mcp auth " + name}
	default: // mcpAuthUnknown
		return readiness.Check{Label: name, Verdict: readiness.VerdictUnverifiable,
			Detail: "registered; auth status unclear (sbx mcp auth status " + name + "); could not verify"}
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
func mcpServerChecks(env hostenv.Env, name string, kind mcpKind, mcpOut string, mcpOK, sbxPresent bool, ctx mcpSandboxContext) []readiness.Check {
	reg := mcp.McpRegEvidenceFrom(mcpOut, mcpOK, name)
	attach := func(cks []readiness.Check) []readiness.Check {
		if ctx.mode == mcpAttachReceipt {
			cks = append(cks, mcpAttachCheck(name, ctx, reg))
		}
		return cks
	}
	if kind == mcpKindUnknown {
		// Fail closed BEFORE any probing: unknown classification never reads or
		// execs the registered definition and never picks a repair command.
		return attach([]readiness.Check{mcpUnknownKindCheck(name, mcpOut, mcpOK, sbxPresent)})
	}
	if !mcpOK {
		return attach([]readiness.Check{mcpUnavailableCheck(name, sbxPresent)})
	}
	if !mcp.McpRegisteredIn(mcpOut, name) {
		return attach([]readiness.Check{mcpNotRegisteredCheck(name, kind)})
	}
	var out []readiness.Check
	switch kind {
	case mcpKindLocal:
		out = append(out, mcpLocalCheck(env, name, mcpOut))
	case mcpKindCatalog, mcpKindPackRemote:
		out = append(out, mcpRemoteAuthCheck(env, name))
	default: // mcpKindPackContainer, mcpKindCustom
		// Registration is confirmed; there is nothing further doctor can
		// honestly verify here (no trusted local spawn, no native OAuth).
		out = append(out, readiness.Check{Label: name, Verdict: readiness.VerdictReady, Detail: "registered",
			Evidence: "sbx mcp ls"})
	}
	return attach(out)
}

// retiredKeyCheck surfaces a stale retired config key (mcp_static/mcp_dynamic,
// the removed eager/lazy split): a verified, OPTIONAL leftover — the key is
// accepted-and-ignored, and the next config mutation (`pix config
// set`/`unset`) rewrites config.toml without it. No todo command: any
// mutation drops it, and there is nothing to "fix" beyond that.
func retiredKeyCheck(key string) readiness.Check {
	return readiness.Check{
		Label:   "config " + key,
		Verdict: readiness.VerdictTodo,
		Detail: "retired config key; ignored (every configured MCP server now preloads at sandbox create); " +
			"the next `pix config set`/`unset` rewrite drops it from config.toml",
		Evidence: "retired key present in config.toml",
	}
}

// unknownKeyCheck surfaces an unrecognized config key — softer than a retired
// one: doctor cannot tell a typo from a newer pix's key, so this is
// unverifiable info, never a verified failure.
func unknownKeyCheck(key string) readiness.Check {
	return readiness.Check{
		Label:    "config " + key,
		Verdict:  readiness.VerdictUnverifiable,
		Detail:   "unknown config key; ignored (a typo, or a key only a newer pix understands)",
		Evidence: "unknown key present in config.toml",
	}
}

// trustedExecPath, mcp.TrustedGogSpawn, probeStatus/axis.ProbeResult, axis.ProbeListTools,
// and classifyProbeErr are SHARED with doctor_gog.go — see doctor_probe.go,
// which owns the single implementation. mcpLocalCheck maps the shared
// axis.ProbeDeniedByPolicy outcome to readiness.VerdictDenied (an explicit policy refusal is
// a positive denial, same as the remote auth axis), and everything else
// unclassified to unverifiable.

// mcpGroup builds the MCP-servers cluster for every configured server plus
// every active-pack integration server. gog is DELIBERATELY skipped — the
// dedicated gog group already owns its registration check + TODO, so probing
// it again would emit a duplicate `pix mcp register`.
func mcpGroup(cfg *config.Config, env hostenv.Env, mcpOut string, mcpOK, sbxPresent bool, ctx mcpSandboxContext) readiness.Group {
	return mcpGroupWith(cfg, env, mcpOut, mcpOK, sbxPresent,
		pack.ActiveContainerMCP(cfg), ctx)
}

// mcpGroupWith is mcpGroup with the pack-integration set and the sandbox
// context injected, so tests drive both hermetically.
func mcpGroupWith(cfg *config.Config, env hostenv.Env, mcpOut string, mcpOK, sbxPresent bool,
	containers map[string]config.MCPContainer, ctx mcpSandboxContext) readiness.Group {

	group := readiness.Group{Title: "MCP servers (via the sbx gateway)"}

	// The server set: configured servers first (order preserved), then any
	// pack-declared integration servers not already configured, then — when a
	// sandbox receipt context exists — any name the receipt independently
	// proves provenance for that current intent doesn't already name
	// (Preloaded, then Loads; receipt order; deduped). This keeps a transient
	// `run --pack` mix-in or a since-switched pack's historical MCP
	// provenance visible on THIS sandbox. gog is excluded throughout (owned
	// by its own dedicated group).
	exclude := map[string]bool{config.GWServerName: true}
	currentIntent := mcp.McpCurrentIntentNames(cfg.MCP, containers, exclude)
	var receipt *workspace.MCPReceipt
	if ctx.mode == mcpAttachReceipt {
		receipt = ctx.receipt
	}
	names, receiptOnly := mcp.McpConfiguredUniverse(currentIntent, receipt, exclude)

	if len(names) == 0 {
		group.Checks = append(group.Checks, readiness.Check{
			Label:   "(none configured)",
			Note:    true,
			Verdict: readiness.VerdictUnverifiable,
			Detail:  "add servers with `pix config set mcp <server>`",
		})
	} else {
		// Classification source of truth: the same `pix-host mcp --list`
		// registration itself uses. Bounded inside mcp.LocalMCPNames.
		localSet, localKnown := mcp.LocalMCPNames(env, env.HostBinary)
		anyRegistered := false
		for _, m := range names {
			kind := classifyMCPServer(m, containers, localSet, localKnown)
			cks := mcpServerChecks(env, m, kind, mcpOut, mcpOK, sbxPresent, ctx)
			if receiptOnly[m] {
				for i := range cks {
					cks[i] = annotateReceiptOnlyCheck(cks[i], m)
				}
			}
			group.Checks = append(group.Checks, cks...)
			if mcpOK && kind != mcpKindUnknown && mcp.McpRegisteredIn(mcpOut, m) {
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
			group.Checks = append(group.Checks, readiness.Check{Label: "attachment", Note: true, Verdict: readiness.VerdictUnverifiable, Detail: det})
		}
	}

	// Config hygiene: stale retired keys (the removed mcp_static/mcp_dynamic
	// split) are a verified optional leftover; unknown keys are softer info.
	for _, k := range cfg.RetiredKeys() {
		group.Checks = append(group.Checks, retiredKeyCheck(k))
	}
	for _, k := range cfg.UnknownKeys() {
		group.Checks = append(group.Checks, unknownKeyCheck(k))
	}
	return group
}

// annotateReceiptOnlyCheck labels a check for a name that is NOT part of the
// current cfg.MCP/pack-integration intent but appears solely because this
// sandbox's own receipt proves it was preloaded or loaded here — sandbox
// PROVENANCE, never current preload intent. Evidence-only: it never changes
// verdict, label, or todo.
func annotateReceiptOnlyCheck(c readiness.Check, name string) readiness.Check {
	note := "sandbox provenance only (from this sandbox's receipt); " + name + " is not part of the current cfg.MCP/pack"
	c.Evidence = c.EvidenceString() + "; " + note
	return c
}
