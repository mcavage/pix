package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"pi-stack/host/config"
)

// doctor_mcp.go — the MCP truth group (S05). Three axes, each verified from
// its own honest source and NEVER inferred from another:
//
//   - REGISTRATION truth comes from the bounded `sbx mcp ls` listing the
//     caller already fetched (mcpOut/mcpOK). `sbx mcp get <name>` may be used
//     to inspect the REGISTERED DEFINITION (the argv sbx would spawn) only —
//     never to infer sandbox attachment.
//   - ATTACHMENT truth comes from the launcher's own per-sandbox receipt
//     (sandboxmcpstate.go): durable evidence of a SUCCESSFUL pi-stack action
//     (preload at create, or a live `pi-stack mcp load`). Config membership is
//     an intent, not an attachment — it is never presented as one.
//   - AUTH truth (remote servers only) comes from a bounded native
//     `sbx mcp auth status <name>` probe. A local stdio server never gets a
//     native OAuth check — there is no control-plane auth for it.

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

// mcpKind is the classification of one configured (or pack-declared) MCP name.
// Classification decides which repair command is honest and which extra probes
// (local spawn, native OAuth) apply — it must never be guessed.
type mcpKind int

const (
	// mcpKindUnknown: the local-name set (`pi-stack-host mcp --list`) could not
	// be established, so doctor genuinely does not know whether this name is a
	// local stdio server or a remote one. FAIL CLOSED: no probe, no exec, no
	// type-specific repair command — recommending `pi-stack mcp register` for a
	// remote name (or bundle/auth for a local one) would be a broken repair.
	mcpKindUnknown mcpKind = iota
	// mcpKindLocal: confirmed in `pi-stack-host mcp --list` — a local stdio
	// server this host can spawn. `pi-stack mcp register <name>` registers it;
	// its health may be probed by exec'ing the registered argv, but ONLY after
	// the canonical-executable gate (recognizedMCPArgv). Never OAuth-checked.
	mcpKindLocal
	// mcpKindCatalog: confirmed NON-local and in the shipped public catalog
	// bundle (mcpCatalogNames: notion/atlassian/granola). Registered via
	// `pi-stack mcp bundle` (or native `sbx mcp add`), authenticated via
	// `pi-stack mcp auth <name>`. Never spawned or exec'd locally.
	mcpKindCatalog
	// mcpKindPackRemote: the active pack declares this integration with a
	// remote endpoint URL. Registered via `pi-stack mcp register <name>` (the
	// pack carries the URL); auth is the same native hosted-control-plane
	// OAuth as the catalog. Never spawned or exec'd locally.
	mcpKindPackRemote
	// mcpKindPackContainer: the active pack declares this integration as an
	// OCI container (manifest or image) the gateway runs on the host.
	// Registered via `pi-stack mcp register <name>`. Doctor never runs the
	// container itself, and there is no native OAuth to check.
	mcpKindPackContainer
	// mcpKindCustom: confirmed NON-local, not in the shipped catalog, and not
	// pack-declared. pi-stack has no command that can register it (`register`
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
// Only kinds pi-stack can actually register get a pi-stack command; a custom
// server gets pointed at native sbx (with its own URL/transport — pi-stack
// cannot invent one); unknown gets NOTHING (no repair command is safe to
// recommend without knowing which kind of server this is).
func mcpRegisterTodo(name string, kind mcpKind) string {
	switch kind {
	case mcpKindLocal, mcpKindPackRemote, mcpKindPackContainer:
		return "pi-stack mcp register " + name
	case mcpKindCatalog:
		return "pi-stack mcp bundle"
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
// checks read. sandbox is derived with deriveSandboxName — the SAME canonical
// helper `pi-stack run` and `pi-stack mcp load` use, so doctor can never
// report on a differently-named sandbox than the one those verbs act on.
type mcpSandboxContext struct {
	mode    mcpAttachMode
	sandbox string
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
//     offline record of past successful pi-stack actions) is still consulted.
func resolveMCPSandboxContext(env shellEnv) mcpSandboxContext {
	if env.getwd == nil || env.stateDir == nil {
		return mcpSandboxContext{mode: mcpAttachNone}
	}
	ws, err := env.getwd()
	if err != nil || strings.TrimSpace(ws) == "" {
		return mcpSandboxContext{mode: mcpAttachNone}
	}
	name := deriveSandboxName(ws)
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
			return mcpSandboxContext{mode: mcpAttachSandboxAbsent, sandbox: name}
		}
	}
	sd, err := env.stateDir()
	if err != nil || strings.TrimSpace(sd) == "" {
		return mcpSandboxContext{mode: mcpAttachNone}
	}
	receipt, status, _ := readSandboxMCPReceipt(sd, name)
	return mcpSandboxContext{mode: mcpAttachReceipt, sandbox: name, receipt: receipt, status: status}
}

// mcpAttachGuidance is the exact, copy-pasteable pair of commands that would
// MAKE attachment true (and receipted). It lives in the detail/evidence of an
// unverifiable attachment check — never as a todo, because unverifiable means
// doctor does not KNOW the server is unattached.
func mcpAttachGuidance(name string) string {
	return "attach live with `pi-stack mcp load " + name + "` or recreate with `pi-stack run --replace`"
}

// mcpAttachCheck renders one registered server's sandbox-attachment evidence
// from the launcher receipt, via the SHARED join row (joinMCPSandboxRow,
// mcpjoin.go) so doctor and status derive attachment truth from ONE path.
// The receipt records SUCCESSFUL pi-stack actions (writeCreateReceipt after a
// create, appendLoadReceipt after a live load) — that is the ONLY thing that
// may claim ready here. Config membership is never attachment. Anything the
// receipt cannot vouch for (no entry, no receipt, or a receipt that is
// corrupt / wrong schema / wrong sandbox identity) is UNVERIFIABLE with the
// exact repair commands in the evidence — never a false claim in either
// direction. Registration is already confirmed by the caller (mcpRegYes).
func mcpAttachCheck(name string, ctx mcpSandboxContext) check {
	label := name + " attachment"
	guidance := mcpAttachGuidance(name)
	row := joinMCPSandboxRow(name, mcpRegYes, ctx.sandbox, ctx.receipt, ctx.status)
	switch row.State {
	case mcpJoinPreloaded:
		return check{label: label, verdict: verdictReady,
			detail:   "preloaded by pi-stack at create (sandbox " + ctx.sandbox + ")",
			evidence: row.Evidence}
	case mcpJoinLoaded:
		return check{label: label, verdict: verdictReady,
			detail:   "loaded by pi-stack (pi-stack mcp load, sandbox " + ctx.sandbox + ")",
			evidence: row.Evidence}
	case mcpJoinRegisteredNotAttached:
		return check{label: label, verdict: verdictUnverifiable,
			detail:   fmt.Sprintf("registered, but pi-stack has no record of attaching it to %s — %s", ctx.sandbox, guidance),
			evidence: row.Evidence}
	}
	// mcpJoinUnverifiable: the receipt itself is absent or untrustworthy.
	if ctx.status == sandboxMCPStateAbsent {
		return check{label: label, verdict: verdictUnverifiable,
			detail:   fmt.Sprintf("no launcher receipt for sandbox %s — attachment unverified; %s", ctx.sandbox, guidance),
			evidence: row.Evidence}
	}
	return check{label: label, verdict: verdictUnverifiable,
		detail: fmt.Sprintf("launcher receipt for sandbox %s is %s — not trusting it; %s",
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
		detail: "sbx unavailable here — registration cannot be verified (check from the host)"}
}

// mcpNotRegisteredCheck is a POSITIVELY VERIFIED registration gap (the bounded
// `sbx mcp ls` succeeded and lacks the name) with the type-correct repair.
// Always optional: an MCP server is an integration, never core.
func mcpNotRegisteredCheck(name string, kind mcpKind) check {
	detail := "not registered"
	switch kind {
	case mcpKindCatalog:
		detail = "not registered (shipped remote catalog server: register with `pi-stack mcp bundle`, then `pi-stack mcp auth " + name + "`)"
	case mcpKindPackRemote, mcpKindPackContainer:
		detail = "not registered (pack integration: register with `pi-stack mcp register " + name + "`)"
	case mcpKindCustom:
		detail = "not registered — a custom server pi-stack cannot register for you " +
			"(`pi-stack mcp register` is local-stdio-only; `pi-stack mcp bundle` covers only " +
			mcpCatalogSummary() + "). Register it natively with its own URL/transport: sbx mcp add"
	}
	return check{label: name, verdict: verdictTodo, detail: detail, todo: mcpRegisterTodo(name, kind)}
}

// mcpUnknownKindCheck is the fail-closed rendering when classification itself
// could not be established (`pi-stack-host mcp --list` unavailable): doctor
// must not guess local vs remote, so there is no probe, no exec, and NO repair
// command — none is safe to recommend.
func mcpUnknownKindCheck(name, mcpOut string, mcpOK, sbxPresent bool) check {
	if !mcpOK {
		return mcpUnavailableCheck(name, sbxPresent)
	}
	det := "could not determine whether this is a local stdio server or a remote one " +
		"(pi-stack-host mcp --list unavailable); no repair command can be safely recommended — " +
		"build/resolve pi-stack-host, then re-run"
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
//     the exact `pi-stack mcp auth <name>` command;
//   - an EXPLICIT policy/forbidden/access-denied signal -> denied (an org
//     decision, not a setup gap — no setup command can fix it);
//   - a timeout or transport/exec failure -> unverifiable, never a guess.
func mcpRemoteAuthCheck(env shellEnv, name string) check {
	out, timedOut, err := probeRun(env, "sbx", "mcp", "auth", "status", name)
	if timedOut {
		return check{label: name, verdict: verdictUnverifiable,
			detail: "registered; auth status timed out (sbx mcp auth status " + name + ") — could not verify"}
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
				detail: "registered but not authorized", todo: "pi-stack mcp auth " + name}
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
			detail: "registered but not authorized", todo: "pi-stack mcp auth " + name}
	default: // mcpAuthUnknown
		return check{label: name, verdict: verdictUnverifiable,
			detail: "registered; auth status unclear (sbx mcp auth status " + name + ") — could not verify"}
	}
}

// mcpServerChecks builds the check line(s) for one configured/pack server:
// the registration+health line, plus (when registered and a sandbox context
// exists) the receipt-backed attachment line.
func mcpServerChecks(env shellEnv, name string, kind mcpKind, mcpOut string, mcpOK, sbxPresent bool, ctx mcpSandboxContext) []check {
	if kind == mcpKindUnknown {
		// Fail closed BEFORE any probing: unknown classification never reads or
		// execs the registered definition and never picks a repair command.
		return []check{mcpUnknownKindCheck(name, mcpOut, mcpOK, sbxPresent)}
	}
	if !mcpOK {
		return []check{mcpUnavailableCheck(name, sbxPresent)}
	}
	if !mcpRegisteredIn(mcpOut, name) {
		return []check{mcpNotRegisteredCheck(name, kind)}
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
	if ctx.mode == mcpAttachReceipt {
		out = append(out, mcpAttachCheck(name, ctx))
	}
	return out
}

// retiredKeyCheck surfaces a stale retired config key (mcp_static/mcp_dynamic,
// the removed eager/lazy split): a verified, OPTIONAL leftover — the key is
// accepted-and-ignored, and the next config mutation (`pi-stack config
// set`/`unset`) rewrites config.toml without it. No todo command: any
// mutation drops it, and there is nothing to "fix" beyond that.
func retiredKeyCheck(key string) check {
	return check{
		label:   "config " + key,
		verdict: verdictTodo,
		detail: "retired config key — ignored (every configured MCP server now preloads at sandbox create); " +
			"the next `pi-stack config set`/`unset` rewrite drops it from config.toml",
		evidence: "retired key present in config.toml",
	}
}

// unknownKeyCheck surfaces an unrecognized config key — softer than a retired
// one: doctor cannot tell a typo from a newer pi-stack's key, so this is
// unverifiable info, never a verified failure.
func unknownKeyCheck(key string) check {
	return check{
		label:    "config " + key,
		verdict:  verdictUnverifiable,
		detail:   "unknown config key — ignored (a typo, or a key only a newer pi-stack understands)",
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
// an ABSOLUTE path equal to the canonical `pi-stack-host` followed by
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
	// Unwrap ONLY a trusted `op run … -- <cmd…>` prefix. A `--` behind any other
	// argv[0] is rejected: the probe execs the wrapper token, so unwrapping a
	// prefix like `/tmp/evil -- pi-stack-host mcp slack` would exec /tmp/evil.
	cmd, ok := unwrapOpRun(argv)
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
	// Basename alone ("pi-stack-host") is NOT enough — an absolute path
	// anywhere on disk with that basename (e.g. /tmp/malicious/pi-stack-host)
	// would satisfy a basename check. Require the CANONICAL binary registration
	// actually uses, and exec THAT token.
	hostTok, hostOK := trustedHostBinaryExecPath(env, cmd[0])
	if !hostOK {
		return nil, false
	}
	norm[innerStart] = hostTok
	return norm, true
}

// trustedHostBinaryExecPath is the canonical-pi-stack-host gate: mcp.go
// registration (registerServers/serverCmd) ALWAYS spawns the ABSOLUTE path
// hostBinaryResolver (findHostBinary) resolves — never a bare name. Trusting
// an absolute path's basename alone would let a malicious
// `/tmp/malicious/pi-stack-host mcp slack` registration pass. env.hostBinary
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
	if filepath.Base(tok) != "pi-stack-host" {
		return "", false
	}
	if !filepath.IsAbs(tok) {
		return "", false // never trust a bare/relative name for pi-stack-host
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
// which owns the single implementation. mcpLocalCheck's switch below has no
// case for the shared probeDeniedByPolicy outcome and falls through its
// `default:` to unverifiable, exactly as it already treated any unclassified
// probe failure before consolidation.

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
// a format pi-stack controls — see runMcpAuth) but conservative about
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
// it again would emit a duplicate `pi-stack mcp register`.
func mcpGroup(cfg *config.Config, env shellEnv, mcpOut string, mcpOK, sbxPresent bool) group {
	return mcpGroupWith(cfg, env, mcpOut, mcpOK, sbxPresent,
		activeContainerMCP(cfg), resolveMCPSandboxContext(env))
}

// mcpGroupWith is mcpGroup with the pack-integration set and the sandbox
// context injected, so tests drive both hermetically.
func mcpGroupWith(cfg *config.Config, env shellEnv, mcpOut string, mcpOK, sbxPresent bool,
	containers map[string]packContainer, ctx mcpSandboxContext) group {

	mcp := group{title: "MCP servers (via the sbx gateway)"}

	// The server set: configured servers first (order preserved), then any
	// pack-declared integration servers not already configured, deduped. gog
	// is excluded (owned by its own group).
	var names []string
	seen := map[string]bool{"gog": true}
	for _, m := range append(append([]string(nil), cfg.MCP...), packContainerNames(containers)...) {
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		names = append(names, m)
	}

	if len(names) == 0 {
		mcp.checks = append(mcp.checks, check{
			label:  "(none configured)",
			note:   true,
			detail: "add servers with `pi-stack config set mcp <server>`",
		})
	} else {
		// Classification source of truth: the same `pi-stack-host mcp --list`
		// registration itself uses. Bounded inside localMCPNames.
		localSet, localKnown := localMCPNames(env, env.hostBinary)
		anyRegistered := false
		for _, m := range names {
			kind := classifyMCPServer(m, containers, localSet, localKnown)
			mcp.checks = append(mcp.checks, mcpServerChecks(env, m, kind, mcpOut, mcpOK, sbxPresent, ctx)...)
			if mcpOK && kind != mcpKindUnknown && mcpRegisteredIn(mcpOut, m) {
				anyRegistered = true
			}
		}
		// Without a workspace sandbox context doctor reports registration/auth
		// only — plus the honest statement of what pi-stack WILL do (preload at
		// create), which is intent, never attachment.
		if anyRegistered && ctx.mode != mcpAttachReceipt {
			det := "no workspace sandbox context here — reporting registration/auth only; configured servers preload at sandbox create"
			if ctx.mode == mcpAttachSandboxAbsent {
				det = "sandbox " + ctx.sandbox + " not created yet — configured servers preload at `pi-stack run` create"
			}
			mcp.checks = append(mcp.checks, check{label: "attachment", note: true, detail: det})
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
