// slack.go is the `pix slack setup|status|disable` CLI: the guided path for
// wiring up the slack MCP server (services/host/slack.go). It has TWO ways to
// get a Slack credential:
//
//   - `--token-ref op://vault/item/field` (this file, slackSetupStatic): take
//     a token you (or an org-owned exchange service) already put in
//     1Password, resolve it, verify LIVE that it authenticates and prove
//     whose identity it is (Slack's auth.test). It never talks to Slack's
//     authorize endpoint and never asks for a client secret — a raw token is
//     never accepted as a flag value, never written to disk by this command,
//     and never printed.
//   - no --token-ref (slack_oauth.go, slackSetupPKCE): run a LOCAL PKCE OAuth
//     grant (still no client secret, ever — PKCE is a PUBLIC client) against
//     a Slack app's client_id, storing the resulting rotating credential in a
//     1Password document.
//
// Both paths end the same way: pin the verified identity
// (SLACK_TEAM_ID/SLACK_USER_ID) so a later silent token swap is detectable,
// register the server, and save config. See docs/design/slack-setup.md.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"pix/host/config"
)

// slackServerName is the MCP registration + display name, matching
// services/host/slack.go's `mcp slack` subcommand.
const slackServerName = "slack"

// slackAuthTestURL is Slack's identity-proving Web API method: it needs no
// scope beyond a valid token, so it is safe to call as a pure verification
// step before anything is written.
const slackAuthTestURL = "https://slack.com/api/auth.test"

// slackIdentity is what a live auth.test call resolves a token to.
type slackIdentity struct {
	team, teamID, user, userID string
}

// liveSlackAuthTest is the real (non-hermetic) implementation wired by
// defaultShellEnv: a bare HTTPS POST to auth.test with the token as a bearer.
// It never logs the token and returns Slack's own error string (never the
// token) on failure.
func liveSlackAuthTest(token string) (slackIdentity, error) {
	req, err := http.NewRequest(http.MethodPost, slackAuthTestURL, nil)
	if err != nil {
		return slackIdentity{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return slackIdentity{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return slackIdentity{}, fmt.Errorf("Slack auth.test HTTP %s", resp.Status)
	}
	var body struct {
		OK     bool   `json:"ok"`
		Error  string `json:"error"`
		Team   string `json:"team"`
		TeamID string `json:"team_id"`
		User   string `json:"user"`
		UserID string `json:"user_id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return slackIdentity{}, fmt.Errorf("decoding Slack auth.test response: %w", err)
	}
	if !body.OK {
		e := body.Error
		if e == "" {
			e = "unknown_error"
		}
		return slackIdentity{}, fmt.Errorf("auth.test: %s", e)
	}
	return slackIdentity{team: body.Team, teamID: body.TeamID, user: body.User, userID: body.UserID}, nil
}

const slackUsage = `usage: pix slack <setup|status|disable> [args]

  setup     wire up an EXISTING personal Slack token: verify it live, pin its
            identity, register it, save config
  status    the token ref, the live identity it resolves to, whether that
            matches the pinned identity, and gateway registration
  disable   remove the Pix-owned registration + config + refs (does NOT
            revoke the token at Slack)

Slack is OPTIONAL and absent unless you set it up. SLACK_TOKEN is always a
single named person's xoxp- user token, never a shared team/bot token — see
docs/design/slack-setup.md.
Run 'pix slack setup -h' for its flags.
`

const slackSetupUsage = `usage: pix slack setup --token-ref op://vault/item/field [--yes]
       pix slack setup [--client-id ID] [--redirect-uri URI] [--vault NAME] [--yes]

With --token-ref: wires up a Slack token you already hold in 1Password:
  1. requires --token-ref to be an op://vault/item/field reference — a pasted
     token is refused outright, and is never echoed back in the refusal
  2. resolves it via op read, and requires the resolved value start with
     xoxp- (a personal Slack user token; a bot/other token type is refused)
  3. verifies it LIVE against Slack's auth.test and prints the team/user it
     resolves to (never the token itself)
  4. asks you to confirm that identity (unless --yes)
  5. writes SLACK_TOKEN plus the non-secret SLACK_TEAM_ID/SLACK_USER_ID
     identity pins, registers ` + slackServerName + ` with the sbx gateway
     (requires sbx — a missing sbx fails this command, it never reports a
     silent "would register"), then saves config. A registration failure
     leaves the refs written but nothing else changed; a save failure after a
     successful registration rolls the registration back.

This --token-ref path does NOT itself perform the OAuth grant that produces
the token — it accepts a token minted elsewhere and verifies it. Obtaining an
xoxp- token still needs either your own Slack app (see the PKCE path below,
with no --token-ref), or an org-owned callback/exchange service so a second
person never needs the app's client secret directly. See
docs/design/slack-setup.md for minimal scopes and revocation.

flags:
  --token-ref <ref>   op://vault/item/field pointing at your xoxp- token
  --yes                never prompt (fails instead of asking on a TTY)

Idempotent: re-running with the same (or a rotated) token re-verifies and
re-registers.
` + slackOAuthPKCESetupUsage

const slackStatusUsage = `usage: pix slack status

Reports, from probes rather than config claims:
  - whether SLACK_TOKEN is a filled op:// ref (in op-refs.env)
  - the identity it resolves to RIGHT NOW (auth.test), not just "a token is set"
  - whether that identity still matches SLACK_TEAM_ID/SLACK_USER_ID, the pin
    recorded the last time 'pix slack setup' ran
  - whether ` + slackServerName + ` is registered with the sbx gateway

Registration is NOT the same as sandbox attachment: a sandbox already
running does not see slack until it is recreated (pix run --replace) or
attached live (pix mcp load slack) — status says so explicitly rather than
collapsing the two into one boolean.

exit: 0 ready · 1 needs setup · 3 could not be verified from here
`

const slackDisableUsage = `usage: pix slack disable

Removes ONLY the Pix-owned pieces:
  - the ` + slackServerName + ` registration in the sbx gateway
  - the ` + slackServerName + ` entry in config.toml's mcp list
  - the SLACK_TOKEN, SLACK_TEAM_ID, and SLACK_USER_ID refs in op-refs.env

This does NOT revoke the token at Slack. Revoke it yourself (Slack's "Apps"
management page, or an org service's auth.revoke) if that's what you want —
a gateway-managed slack process that already resolved the old token at spawn
may keep using it until it is restarted, regardless of what this removes.
A clean no-op (exit 0) when nothing is configured.
`

// runSlackCmd is the `pix slack` verb tree.
func runSlackCmd(argv []string) {
	if len(argv) == 0 {
		fmt.Fprint(os.Stderr, slackUsage)
		os.Exit(2)
	}
	if wantsHelp(argv[:1]) {
		fmt.Print(slackUsage)
		return
	}
	switch argv[0] {
	case "setup":
		runSlackSetupCmd(argv[1:])
	case "status":
		runSlackStatusCmd(argv[1:])
	case "disable":
		runSlackDisableCmd(argv[1:])
	default:
		fmt.Fprintf(os.Stderr, "pix slack: unknown subcommand %q (want: setup, status, disable)\n", argv[0])
		os.Exit(2)
	}
}

// ---------------------------------------------------------------------------
// setup
// ---------------------------------------------------------------------------

type slackSetupOpts struct {
	tokenRef  string
	assumeYes bool

	// clientID/redirectURI/vault configure the PKCE path (used when tokenRef
	// is empty). vaultExplicit distinguishes an explicit --vault from the
	// "Private" default, so a rerun can prefer an already-configured
	// Slack.OAuthVaultID instead of silently switching vaults.
	clientID      string
	redirectURI   string
	vault         string
	vaultExplicit bool
}

func parseSlackSetupArgs(argv []string) (slackSetupOpts, error) {
	o := slackSetupOpts{vault: "Private"}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		next := func() (string, error) {
			if i+1 >= len(argv) {
				return "", fmt.Errorf("%s needs a value", a)
			}
			i++
			return argv[i], nil
		}
		switch {
		case a == "--token-ref":
			v, err := next()
			if err != nil {
				return o, err
			}
			o.tokenRef = v
		case strings.HasPrefix(a, "--token-ref="):
			o.tokenRef = strings.TrimPrefix(a, "--token-ref=")
		case a == "--yes", a == "-y", a == "--non-interactive":
			o.assumeYes = true
		case a == "--client-id":
			v, err := next()
			if err != nil {
				return o, err
			}
			o.clientID = v
		case strings.HasPrefix(a, "--client-id="):
			o.clientID = strings.TrimPrefix(a, "--client-id=")
		case a == "--redirect-uri":
			v, err := next()
			if err != nil {
				return o, err
			}
			o.redirectURI = v
		case strings.HasPrefix(a, "--redirect-uri="):
			o.redirectURI = strings.TrimPrefix(a, "--redirect-uri=")
		case a == "--vault":
			v, err := next()
			if err != nil {
				return o, err
			}
			o.vault, o.vaultExplicit = v, true
		case strings.HasPrefix(a, "--vault="):
			o.vault, o.vaultExplicit = strings.TrimPrefix(a, "--vault="), true
		case a == "-h", a == "--help":
			return o, errHelpRequested
		default:
			// Do not echo an accidentally pasted token back to stderr.
			if strings.HasPrefix(a, "xox") || config.LooksSecretShaped("SLACK_TOKEN", a) {
				return o, fmt.Errorf("unexpected secret-shaped argument [REDACTED] (pass an op:// ref with --token-ref; see: pix slack setup -h)")
			}
			return o, fmt.Errorf("unknown flag %q (see: pix slack setup -h)", a)
		}
	}
	return o, nil
}

func runSlackSetupCmd(argv []string) {
	opts, err := parseSlackSetupArgs(argv)
	if err != nil {
		if err == errHelpRequested {
			fmt.Print(slackSetupUsage)
			return
		}
		fmt.Fprintf(os.Stderr, "pix slack setup: %v\n\n%s", err, slackSetupUsage)
		os.Exit(2)
	}
	tty := isTTY(os.Stdin)
	if opts.assumeYes {
		tty = false // --yes means "never prompt", even on a real terminal
	}
	if err := slackSetup(defaultShellEnv(), opts, os.Stdin, os.Stdout, tty, hostBinaryResolver); err != nil {
		fmt.Fprintf(os.Stderr, "pix slack setup: %v\n", err)
		os.Exit(1)
	}
}

// slackSetup is `pix slack setup`'s dispatcher: --token-ref runs the static
// (already-issued token) path, and its absence runs the localhost PKCE OAuth
// path (slack_oauth.go). Both share the same preflight/register/rollback
// helpers so their observable failure discipline matches.
func slackSetup(env shellEnv, opts slackSetupOpts, in io.Reader, out io.Writer, tty bool, hostResolver func() (string, error)) error {
	if strings.TrimSpace(opts.tokenRef) != "" {
		return slackSetupStatic(env, opts, in, out, tty, hostResolver)
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	return slackSetupPKCE(env, cfg, opts, defaultSlackOAuthDeps(), in, out, tty, hostResolver)
}

// slackSetupStatic is the hermetically-testable core of the --token-ref path.
// Every early-return path below runs BEFORE anything is written or
// registered, so a rejected ref, an unresolved ref, a non-xoxp- token, a
// failed live auth.test, or a declined confirmation all leave op-refs.env,
// the sbx gateway, and config.toml completely untouched.
func slackSetupStatic(env shellEnv, opts slackSetupOpts, in io.Reader, out io.Writer, tty bool, hostResolver func() (string, error)) error {
	ref := normalizeOpRef(strings.TrimSpace(opts.tokenRef))
	if !strings.HasPrefix(ref, "op://") {
		return fmt.Errorf("--token-ref must be an op://vault/item/field reference — " +
			"pix never accepts or stores a pasted token directly")
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Snapshot gateway state before touching refs. Never bless or overwrite an
	// arbitrary command that merely happens to be registered as "slack".
	wasRegistered, err := slackRegistrationPreflight(env)
	if err != nil {
		return err
	}
	token, ok := opReadNonEmpty(env, ref)
	if !ok {
		return fmt.Errorf("could not resolve --token-ref via op read; check the reference and that op is signed in")
	}
	if !strings.HasPrefix(token, "xoxp-") {
		return fmt.Errorf("--token-ref does not resolve to a personal xoxp- Slack user token; " +
			"pix slack only accepts a personal user token, never a bot/other token type (see docs/design/slack-setup.md)")
	}
	if env.slackAuthTest == nil {
		return fmt.Errorf("internal: shellEnv.slackAuthTest not wired")
	}
	id, err := env.slackAuthTest(token)
	if err != nil {
		return fmt.Errorf("Slack auth.test failed: %w (the token did not authenticate — check it hasn't been revoked)", err)
	}
	if strings.TrimSpace(id.teamID) == "" || strings.TrimSpace(id.userID) == "" {
		return fmt.Errorf("Slack auth.test returned no team_id or user_id; refusing to create an identity pin")
	}
	fmt.Fprintf(out, "Slack identity: %s (user %s) on team %s (%s)\n", id.user, id.userID, id.team, id.teamID)

	if !opts.assumeYes {
		if !tty {
			return fmt.Errorf("refusing to wire up Slack non-interactively without --yes")
		}
		if !confirmYN(in, out, fmt.Sprintf("Wire up Slack as %s on %s? [y/N]: ", id.user, id.team), false) {
			fmt.Fprintln(out, "aborted; nothing written")
			return nil
		}
	}

	if err := runSecretSet(env, out, "SLACK_TOKEN", ref); err != nil {
		return fmt.Errorf("writing SLACK_TOKEN: %w", err)
	}
	if err := runSecretSet(env, out, "SLACK_TEAM_ID", id.teamID); err != nil {
		return fmt.Errorf("writing SLACK_TEAM_ID: %w", err)
	}
	if err := runSecretSet(env, out, "SLACK_USER_ID", id.userID); err != nil {
		return fmt.Errorf("writing SLACK_USER_ID: %w", err)
	}

	// REGISTER FIRST, save second — mirrors gworkspace's commit order.
	// registerServers itself hard-fails (errSbxUnavailable) when sbx is
	// absent rather than silently reporting a no-op success; a failure here
	// returns before cfg.AddMCP/Save ever runs.
	if err := slackRegisterAndSave(cfg, env, out, hostResolver, wasRegistered, ""); err != nil {
		return err
	}

	fmt.Fprintln(out, "Slack is set up.")
	slackPrintAttachmentNote(out)
	return nil
}

// ---------------------------------------------------------------------------
// status
// ---------------------------------------------------------------------------

// slackExit maps the worst verdict in a rendered status to this command's
// process exit code: 1 for a verified gap, 3 for something that could not be
// checked from here, 0 when everything is proven. Mirrors gworkspaceExit's
// contract exactly (a verified failure outranks an unverifiable).
func slackExit(checks []check) int {
	worst := 0
	for _, c := range checks {
		if c.note {
			continue
		}
		switch c.verdict {
		case verdictTodo, verdictDenied:
			return 1
		case verdictUnverifiable:
			worst = 3
		}
	}
	return worst
}

func runSlackStatusCmd(argv []string) {
	if wantsHelp(argv) {
		fmt.Print(slackStatusUsage)
		return
	}
	if len(argv) > 0 {
		fmt.Fprintf(os.Stderr, "pix slack status: unexpected argument %q\n\n%s", argv[0], slackStatusUsage)
		os.Exit(2)
	}
	os.Exit(slackStatus(defaultShellEnv(), os.Stdout))
}

// slackIdentityPins reads the literal (non-ref) SLACK_TEAM_ID/SLACK_USER_ID
// values straight out of op-refs.env content — they are allowlisted
// non-secret literals (config.NonSecretOpRefsKeys), never op:// refs.
func slackIdentityPins(content string) (teamID, userID string) {
	for _, r := range parseOpRefs(content) {
		switch r.key {
		case "SLACK_TEAM_ID":
			teamID = r.value
		case "SLACK_USER_ID":
			userID = r.value
		}
	}
	return
}

// slackStatus renders the Slack surface and returns the process exit code.
// Every green here comes from a probe: a ref being present is never conflated
// with the token it points at actually resolving to the identity it claims.
func slackStatus(env shellEnv, out io.Writer) int {
	fmt.Fprintln(out, "Slack (optional, personal xoxp- user token, via the host MCP gateway)")

	path, content, exists := opRefsContent(env)
	var ref string
	var refFilled bool
	if exists {
		for _, r := range parseOpRefs(content) {
			if r.key == "SLACK_TOKEN" {
				ref, refFilled = r.value, r.isRef && !r.placeholder
			}
		}
	}

	var checks []check
	switch {
	case ref == "":
		checks = append(checks, check{label: "token ref", verdict: verdictTodo,
			detail: "SLACK_TOKEN is not set in " + path, evidence: "no SLACK_TOKEN line in op-refs.env",
			todo: "pix slack setup --token-ref op://vault/item/field"})
	case !refFilled:
		checks = append(checks, check{label: "token ref", verdict: verdictTodo,
			detail: "SLACK_TOKEN is set but is not a filled op:// ref", evidence: "placeholder or non-ref value in op-refs.env",
			todo: "pix slack setup --token-ref op://vault/item/field"})
	default:
		checks = append(checks, check{label: "token ref", verdict: verdictReady, detail: ref})
	}

	var id slackIdentity
	haveIdentity := false
	if refFilled {
		token, ok := opReadNonEmpty(env, ref)
		switch {
		case !ok:
			checks = append(checks, check{label: "resolution", verdict: verdictUnverifiable,
				detail: "could not resolve " + ref + " via op read", evidence: "op read failed or returned empty"})
		case !strings.HasPrefix(token, "xoxp-"):
			checks = append(checks, check{label: "resolution", verdict: verdictTodo,
				detail:   ref + " does not resolve to a personal xoxp- user token",
				evidence: "resolved value lacks the xoxp- prefix", todo: "pix slack setup --token-ref op://vault/item/field"})
		case env.slackAuthTest == nil:
			checks = append(checks, check{label: "identity", verdict: verdictUnverifiable,
				detail: "cannot verify live identity (no auth.test probe wired here)"})
		default:
			got, aerr := env.slackAuthTest(token)
			if aerr != nil {
				checks = append(checks, check{label: "identity", verdict: verdictTodo,
					detail: "auth.test failed: " + aerr.Error(), todo: "pix slack setup --token-ref op://vault/item/field"})
			} else {
				id, haveIdentity = got, true
				checks = append(checks, check{label: "identity", verdict: verdictReady,
					detail: fmt.Sprintf("%s (user %s) on %s (%s)", got.user, got.userID, got.team, got.teamID)})
			}
		}
	}

	pinTeam, pinUser := slackIdentityPins(content)
	switch {
	case pinTeam == "" && pinUser == "":
		checks = append(checks, check{label: "identity pin", note: true, verdict: verdictUnverifiable,
			detail: "no SLACK_TEAM_ID/SLACK_USER_ID pin recorded yet (set by pix slack setup)"})
	case !haveIdentity:
		checks = append(checks, check{label: "identity pin", verdict: verdictUnverifiable,
			detail: fmt.Sprintf("pinned to team %s / user %s, but the live identity could not be confirmed above", pinTeam, pinUser)})
	case pinTeam == id.teamID && pinUser == id.userID:
		checks = append(checks, check{label: "identity pin", verdict: verdictReady,
			detail: "matches the identity pinned at setup"})
	default:
		checks = append(checks, check{label: "identity pin", verdict: verdictTodo,
			detail: fmt.Sprintf("live identity (team %s / user %s) does not match the pin (team %s / user %s) — "+
				"the token was likely swapped; re-run pix slack setup if this is expected",
				id.teamID, id.userID, pinTeam, pinUser),
			todo: "pix slack setup --token-ref op://vault/item/field"})
	}

	registeredArgv, registrationExists := registeredMCPCommand(env, slackServerName)
	_, registrationTrusted := recognizedMCPArgv(env, registeredArgv, slackServerName)
	switch {
	case registrationExists && registrationTrusted:
		checks = append(checks, check{label: "registration", verdict: verdictReady,
			detail: "canonical Pix host command registered with the sbx gateway", evidence: "sbx mcp get " + slackServerName})
	case registrationExists:
		checks = append(checks, check{label: "registration", verdict: verdictTodo,
			detail: "a server named slack is registered, but it is not the canonical Pix host command",
			todo:   "inspect it with: sbx mcp get slack"})
	case env.lookPath == nil:
		checks = append(checks, check{label: "registration", verdict: verdictUnverifiable,
			detail: "sbx unavailable here; registration cannot be verified"})
	default:
		if _, err := env.lookPath("sbx"); err != nil {
			checks = append(checks, check{label: "registration", verdict: verdictUnverifiable,
				detail: "sbx unavailable here; registration cannot be verified"})
		} else {
			checks = append(checks, check{label: "registration", verdict: verdictTodo,
				detail: "not registered with the sbx gateway", todo: "pix slack setup --token-ref op://vault/item/field"})
		}
	}

	checks = append(checks, check{label: "attachment", note: true, verdict: verdictUnverifiable,
		detail: "registration is NOT the same as attachment: a sandbox already running won't see slack " +
			"until it's recreated (pix run --replace) or attached live (pix mcp load slack)"})

	for _, c := range checks {
		fmt.Fprintf(out, "  %s %-13s %s\n", checkGlyph(c), c.label, c.detail)
		if c.todo != "" {
			fmt.Fprintf(out, "      fix: %s\n", c.todo)
		}
	}
	return slackExit(checks)
}

// ---------------------------------------------------------------------------
// disable
// ---------------------------------------------------------------------------

func runSlackDisableCmd(argv []string) {
	if wantsHelp(argv) {
		fmt.Print(slackDisableUsage)
		return
	}
	if len(argv) > 0 {
		fmt.Fprintf(os.Stderr, "pix slack disable: unexpected argument %q\n\n%s", argv[0], slackDisableUsage)
		os.Exit(2)
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix slack disable: loading config: %v\n", err)
		os.Exit(1)
	}
	if err := slackDisable(cfg, defaultShellEnv(), os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "pix slack disable: %v\n", err)
		os.Exit(1)
	}
}

// slackRegistrationPresence takes a bounded, tri-state read of whatever slack
// registration exists: (false, nil) confirmed absent (or sbx isn't installed
// here at all), (true, nil) confirmed present, or a non-nil err when the
// gateway state could not be read at all — in which case the caller must
// refuse to touch config rather than risk silently dropping a registration
// disable can't see.
func slackRegistrationPresence(env shellEnv) (present bool, err error) {
	if env.lookPath == nil {
		return false, fmt.Errorf("could not confirm the %s registration (no shell environment wired): "+
			"refusing to remove config while the gateway state is unreadable", slackServerName)
	}
	if _, lerr := env.lookPath("sbx"); lerr != nil {
		return false, nil // sbx isn't installed here: nothing is registered from this host's POV
	}
	out, timedOut, rerr := probeRun(env, "sbx", "mcp", "ls")
	if timedOut || rerr != nil {
		return false, fmt.Errorf("could not confirm the %s registration (sbx mcp ls did not resolve cleanly): "+
			"refusing to remove config while the gateway state is unreadable; check the sbx daemon (sbx mcp status), "+
			"then re-run pix slack disable", slackServerName)
	}
	return mcpRegisteredIn(out, slackServerName), nil
}

// slackDisable removes ONLY the pieces pix owns: the gateway registration,
// the mcp config entry, and the SLACK_TOKEN/SLACK_TEAM_ID/SLACK_USER_ID refs.
// It never revokes the token at Slack — see the printed notice at the end.
func slackDisable(cfg *config.Config, env shellEnv, out io.Writer) error {
	_, content, exists := opRefsContent(env)
	hasManagedRef := false
	if exists {
		for _, r := range parseOpRefs(content) {
			switch r.key {
			case "SLACK_TOKEN", "SLACK_TEAM_ID", "SLACK_USER_ID":
				hasManagedRef = true
			}
		}
	}
	configured := hasManagedRef || mcpConfigured(cfg, slackServerName)

	registered, err := slackRegistrationPresence(env)
	if err != nil {
		return err
	}
	if registered {
		argv, ok := registeredMCPCommand(env, slackServerName)
		if _, trusted := recognizedMCPArgv(env, argv, slackServerName); !ok || !trusted {
			return fmt.Errorf("a server named slack is registered, but it is not the canonical Pix host command; refusing to remove it (inspect: sbx mcp get slack)")
		}
	}

	if !configured && !registered {
		fmt.Fprintln(out, "Slack is not configured; nothing to remove.")
		return nil
	}

	if registered {
		if env.run == nil {
			return fmt.Errorf("internal: shellEnv.run not wired")
		}
		if _, err := env.run("sbx", "mcp", "rm", slackServerName); err != nil {
			return fmt.Errorf("removing the %s registration: %w (remove it by hand: sbx mcp rm %s)",
				slackServerName, err, slackServerName)
		}
		fmt.Fprintln(out, "  removed registration: "+slackServerName)
	}

	if cfg.RemoveMCP(slackServerName) {
		if err := cfg.Save(); err != nil {
			return fmt.Errorf("saving config: %w (the gateway registration was already removed; re-run pix slack disable)", err)
		}
		fmt.Fprintln(out, "  cleared config: mcp "+slackServerName)
	}

	for _, key := range []string{"SLACK_TOKEN", "SLACK_TEAM_ID", "SLACK_USER_ID"} {
		if err := runSecretRm(env, out, key); err != nil {
			return fmt.Errorf("removing %s: %w", key, err)
		}
	}

	fmt.Fprintln(out, "Slack is off here. This does NOT revoke the token at Slack — revoke it")
	fmt.Fprintln(out, "yourself (Slack's Apps management page, or an org service's auth.revoke) if")
	fmt.Fprintln(out, "that's what you want. A gateway-managed slack process that already resolved")
	fmt.Fprintln(out, "the token at spawn may keep using it until restarted, regardless of this.")
	fmt.Fprintln(out, "See docs/design/slack-setup.md#rollback-and-revocation.")
	return nil
}
