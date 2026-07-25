// gworkspace.go is the PUBLIC Google Workspace surface: `pi-stack gworkspace
// setup|status|disable`, plus the `pi-stack setup --google-workspace` route
// into the same code.
//
// It is deliberately a FAÇADE. The transaction that actually authorizes an
// account, proves the headless spawn, registers with the sbx gateway and
// saves config lives in gog_setup.go and is NOT reimplemented here: its
// safety properties (tri-state registration snapshot refusal on `unknown`,
// per-route `--readonly` capability probe, register-then-save with rollback
// on save failure, zero-tools never green) are load-bearing and survive this
// file verbatim.
//
// Naming (the one rule this file exists to hold): the product is "Google
// Workspace", the CLI noun is `gworkspace`, the MCP registration/display name
// is `google-workspace` (gwServerName), and the config key is
// `google_workspace_account`. The external binary is still called `gog` and
// that name may appear ONLY in a dependency-install/upgrade fix line, in
// troubleshooting docs, and in code that execs it. Go identifiers that name
// the external binary (gogSetup, gogRegSnapshot, GogAccount) are internal and
// are not user-facing output.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"pi-stack/host/config"
)

const (
	// gwServerName is the MCP registration + display name. Everything that
	// registers, lists, probes, or removes the server uses this constant, so
	// the gateway name and the public name can never drift.
	gwServerName = "google-workspace"
	// gwInstallCmd is the ONE place the external binary's package name is
	// allowed to reach the user, per the naming rule above.
	gwInstallCmd = "brew install openclaw/tap/gogcli"
	// gwUpgradeCmd is its upgrade twin.
	gwUpgradeCmd = "brew upgrade openclaw/tap/gogcli"
	// gwAudienceURL is the literal Google Cloud console page where an OAuth
	// app's publication state (Testing vs In production) is read and changed.
	// Printed at setup AND re-rendered by `gworkspace status` on every run:
	// an app left in Testing expires its refresh tokens after 7 days, which is
	// a time-based silent failure a one-time setup message cannot control.
	gwAudienceURL = "https://console.cloud.google.com/auth/audience"
	// gwTestingTokenMaxAge is how long a refresh token issued by an app still
	// in Testing survives.
	gwTestingTokenMaxAge = 7 * 24 * time.Hour
)

const gworkspaceUsage = `usage: pi-stack gworkspace <setup|status|disable> [args]

  setup     guided Google Workspace onboarding: dependency check, OAuth client
            import, read-only authorization, headless proof, then registration
  status    what is configured, what is proven, and the OAuth publication
            state + token age (re-rendered every run)
  disable   remove the Pix-owned config + gateway registration (your Google
            credentials are left untouched)

Google Workspace is OPTIONAL and absent unless you set it up.
Run 'pi-stack gworkspace setup -h' for its flags.
`

const gworkspaceSetupUsage = `usage: pi-stack gworkspace setup [--account <email>] [--credentials <path>] [--yes]

Guides Google Workspace onboarding end to end:
  1. checks the dependency CLI is installed (exact install command if not),
     then validates your credentials path is a true regular file and imports
     it by invoking that CLI: this command never reads or prints its
     contents, and never copies it into pi-stack config
  2. probes the selected auth route's OWN subcommand help/flags for the
     read-only capability it needs at grant time (see step 3)
  3. preflights EVERY remaining predictable hard requirement BEFORE any
     authorization happens: sbx must be installed (it registers the server
     with the gateway; a missing sbx fails this command, it never reports a
     silent "would register"), config must load cleanly, and whatever
     registration already exists must be CONFIRMED absent, or present with a
     readable command. An unreadable/unlistable prior registration aborts
     HERE, before any authorization runs and before config is touched
  4. authorizes <email> REQUESTING READ-ONLY OAUTH SCOPES at grant time; if
     the installed dependency cannot advertise read-only for the selected
     route, this fails with upgrade guidance rather than authorizing without
     it (may open a browser; inherits this terminal)
  5. verifies interactive auth, THEN verifies headless tools with the EXACT
     hardened command the sbx gateway will actually spawn. Authorization that
     succeeds while the headless listing returns 0 tools registers NOTHING
     and saves NOTHING; an unverifiable probe is never reported as success
  6. on success: registers ` + gwServerName + ` with the sbx gateway FIRST,
     using the SAME captured command snapshot that was just verified, and
     only once that succeeds saves google_workspace_account + enables the
     server in the configured MCP set. A registration failure never touches
     the persisted config, and a save failure after a successful registration
     rolls the registration back

flags:
  --account <email>      the Google Workspace account to authorize
  --credentials <path>   path to your Desktop OAuth client JSON (regular file)
  --yes                  never prompt (fails instead of asking on a TTY)

On a real terminal, a missing --account/--credentials is prompted for.
Idempotent: re-running a healthy setup is safe and re-registers the server.

pi-stack does not independently inspect the OAuth scopes actually granted (no
stable scope-inspection surface exists to check against); this command
guarantees the grant REQUESTS read-only scopes, and the registered server's
own runtime flags are the backstop that blocks writes regardless.

No organization OAuth client is bundled or referenced here; bring your own.
`

const gworkspaceStatusUsage = `usage: pi-stack gworkspace status

Reports, from probes rather than config claims:
  - whether an account is configured (google_workspace_account)
  - whether the sbx gateway has ` + gwServerName + ` registered, and whether
    the registered command carries the hardened read-only flags
  - whether the headless spawn the gateway uses returns tools
  - the OAuth publication state you must confirm, and the age of the stored
    credentials, re-rendered EVERY run until the app is published

exit: 0 ready · 1 needs setup · 3 could not be verified from here
`

const gworkspaceDisableUsage = `usage: pi-stack gworkspace disable

Removes ONLY the Pix-owned pieces:
  - google_workspace_account and the ` + gwServerName + ` entry in config.toml
  - the ` + gwServerName + ` registration in the sbx gateway

Your Google OAuth credentials and tokens are left untouched on disk; revoke
them yourself at ` + gwAudienceURL + ` if you want them gone.
A clean no-op (exit 0) when nothing is configured.
`

// runGworkspaceCmd is the `pi-stack gworkspace` verb tree.
//
// The help gate is checked ONLY for the no-subcommand case, exactly as the
// former `gog` tree did: a blanket wantsHelp over the whole argv would catch
// `gworkspace setup -h` and print the noun-level usage instead of the
// subcommand's own.
func runGworkspaceCmd(argv []string) {
	if len(argv) == 0 {
		fmt.Fprint(os.Stderr, gworkspaceUsage)
		os.Exit(2)
	}
	if wantsHelp(argv[:1]) {
		fmt.Print(gworkspaceUsage)
		return
	}
	switch argv[0] {
	case "setup":
		runGworkspaceSetupCmd(argv[1:])
	case "status":
		runGworkspaceStatusCmd(argv[1:])
	case "disable":
		runGworkspaceDisableCmd(argv[1:])
	default:
		fmt.Fprintf(os.Stderr, "pi-stack gworkspace: unknown subcommand %q (want: setup, status, disable)\n", argv[0])
		os.Exit(2)
	}
}

// gworkspaceSetupOpts is the parsed `gworkspace setup` flag set. It is the
// same shape `pi-stack setup --google-workspace` builds, so both doors reach
// gogSetup with identical inputs.
type gworkspaceSetupOpts = gogSetupOpts

func parseGworkspaceSetupArgs(argv []string) (gworkspaceSetupOpts, error) {
	var o gworkspaceSetupOpts
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
		case a == "--account":
			v, err := next()
			if err != nil {
				return o, err
			}
			o.account = v
		case strings.HasPrefix(a, "--account="):
			o.account = strings.TrimPrefix(a, "--account=")
		case a == "--credentials":
			v, err := next()
			if err != nil {
				return o, err
			}
			o.credentials = v
		case strings.HasPrefix(a, "--credentials="):
			o.credentials = strings.TrimPrefix(a, "--credentials=")
		case a == "--yes", a == "-y", a == "--non-interactive":
			o.assumeYes = true
		case a == "-h", a == "--help":
			return o, errHelpRequested
		default:
			return o, fmt.Errorf("unknown flag %q (see: pi-stack gworkspace setup -h)", a)
		}
	}
	return o, nil
}

// runGworkspaceSetupCmd parses flags, wires the real shellEnv (the
// browser-opening auth steps inherit THIS process's stdio), and runs the
// unchanged transaction.
func runGworkspaceSetupCmd(argv []string) {
	opts, err := parseGworkspaceSetupArgs(argv)
	if err != nil {
		if err == errHelpRequested {
			fmt.Print(gworkspaceSetupUsage)
			return
		}
		fmt.Fprintf(os.Stderr, "pi-stack gworkspace setup: %v\n\n%s", err, gworkspaceSetupUsage)
		os.Exit(2)
	}
	tty := isTTY(os.Stdin)
	if opts.assumeYes {
		tty = false // --yes means "never prompt", even on a real terminal
	}
	if err := gworkspaceSetup(defaultShellEnv(), opts, os.Stdin, os.Stdout, tty); err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack gworkspace setup: %v\n", err)
		os.Exit(1)
	}
}

// gworkspaceSetup is the façade over the unchanged gog_setup.go transaction.
// It adds exactly one thing the transaction does not own: the OAuth
// publication confirmation (AC-P0-315), printed with the literal Audience URL
// AFTER a proven, registered setup, because that trap is time-based and the
// user must know to look for it. Everything else is delegated verbatim.
func gworkspaceSetup(env shellEnv, opts gworkspaceSetupOpts, in io.Reader, out io.Writer, tty bool) error {
	if err := gogSetup(env, opts, in, out, tty); err != nil {
		return err
	}
	gworkspacePublicationNotice(out)
	return nil
}

// gworkspacePublicationNotice prints the OAuth publication trap in the exact
// words `gworkspace status` re-renders, so setup and status can never
// disagree about what the user has to do.
func gworkspacePublicationNotice(out io.Writer) {
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "confirm your OAuth app is published, or this stops working in 7 days:")
	fmt.Fprintln(out, "  open: "+gwAudienceURL)
	fmt.Fprintln(out, "  an app left in Testing expires its refresh tokens after 7 days;")
	fmt.Fprintln(out, "  `pi-stack gworkspace status` re-checks the token age every run.")
}

// ---------------------------------------------------------------------------
// status
// ---------------------------------------------------------------------------

// gworkspaceExit maps the worst verdict in a rendered status to this
// command's process exit code, following the shared contract: 1 for a
// verified gap, 3 for something that could not be checked from here, 0 when
// everything asked for is proven. A verified failure outranks an
// unverifiable.
func gworkspaceExit(checks []check) int {
	worst := 0
	for _, c := range checks {
		if c.note {
			continue // notes are context, never a verdict
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

func runGworkspaceStatusCmd(argv []string) {
	if wantsHelp(argv) {
		fmt.Print(gworkspaceStatusUsage)
		return
	}
	if len(argv) > 0 {
		fmt.Fprintf(os.Stderr, "pi-stack gworkspace status: unexpected argument %q\n\n%s", argv[0], gworkspaceStatusUsage)
		os.Exit(2)
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack gworkspace status: loading config: %v\n", err)
		os.Exit(1)
	}
	os.Exit(gworkspaceStatus(cfg, defaultShellEnv(), os.Stdout, time.Now()))
}

// gworkspaceStatus renders the Google Workspace surface and returns the
// process exit code. Every green here comes from a probe: config membership
// is rendered as intent, never as readiness.
func gworkspaceStatus(cfg *config.Config, env shellEnv, out io.Writer, now time.Time) int {
	fmt.Fprintln(out, "Google Workspace (optional, read-only, via the host MCP gateway)")

	acct := gogAccount(cfg, env)
	if acct == "" && !mcpConfigured(cfg, gwServerName) {
		fmt.Fprintln(out, "  · not configured — set it up: pi-stack gworkspace setup")
		return 0
	}

	var checks []check
	if acct == "" {
		checks = append(checks, check{label: "account", verdict: verdictTodo,
			detail:   gwServerName + " is in the configured MCP set but no account is authorized",
			evidence: "google_workspace_account is unset",
			todo:     "pi-stack gworkspace setup"})
	} else {
		checks = append(checks, check{label: "account", verdict: verdictReady,
			detail: acct, evidence: "google_workspace_account = " + acct})
	}

	// Registration + headless proof, read from what sbx ACTUALLY registered.
	if argv, ok := registeredGogCommand(env); ok {
		if missing := gogMissingHardenedFlags(env, argv); len(missing) > 0 {
			checks = append(checks, check{label: "read-only", verdict: verdictTodo,
				detail:   "registered command is missing hardened read-only flags: " + strings.Join(missing, " "),
				evidence: "registered argv lacks " + strings.Join(missing, " "),
				todo:     "pi-stack gworkspace setup"})
		} else {
			checks = append(checks, check{label: "read-only", verdict: verdictReady,
				detail:   "registered command carries the hardened read-only flags",
				evidence: strings.Join(gogHardenedFlags, " ") + " present in the registered argv"})
		}
		trustedArgv, trusted := trustedGogSpawn(env, argv)
		if !trusted {
			checks = append(checks, check{label: "headless spawn", verdict: verdictUnverifiable,
				detail:   "probe skipped: the registered command's executable does not match the PATH-resolved binary — never executed",
				evidence: "registered executable token not canonical; probe not executed"})
		} else {
			checks = append(checks, gogSpawnCheck(env, probeListTools(env, trustedArgv),
				"registered command exposes tools (verified as-registered)",
				"the registered command returns 0 tools — keyring not headless"))
		}
	} else {
		checks = append(checks, check{label: "registration", verdict: verdictUnverifiable,
			detail:   "could not read the " + gwServerName + " registration from sbx",
			evidence: "sbx mcp get " + gwServerName + " did not resolve a command",
			todo:     "sbx mcp status"})
	}

	// The 7-day Testing trap, re-rendered EVERY run until the app is
	// published. This is a recurring surface on purpose: a one-time setup
	// message cannot control a time-based silent failure.
	checks = append(checks, gworkspaceTokenAgeCheck(env, now))

	for _, c := range checks {
		fmt.Fprintf(out, "  %s %-15s %s\n", checkGlyph(c), c.label, c.detail)
		if c.todo != "" {
			fmt.Fprintf(out, "      fix: %s\n", c.todo)
		}
	}
	fmt.Fprintln(out, "  publication: confirm the app is published at "+gwAudienceURL)
	return gworkspaceExit(checks)
}

// gworkspaceTokenAgeCheck reports how old the stored OAuth credentials are,
// against the 7-day lifetime an app still in Testing gives its refresh
// tokens. It is UNVERIFIABLE, never a failure: pi-stack cannot read Google's
// publication state, so it reports the observation (the age) and names the
// condition that resolves it (publishing the app), which is exactly what an
// unverifiable verdict is required to carry.
func gworkspaceTokenAgeCheck(env shellEnv, now time.Time) check {
	path, age, ok := gworkspaceCredentialAge(env, now)
	if !ok {
		return check{label: "token age", verdict: verdictUnverifiable,
			detail:   "could not read the stored credentials' age; resolved by publishing the app at " + gwAudienceURL,
			evidence: "no readable credential file under the Google Workspace home"}
	}
	days := int(age.Hours() / 24)
	if age >= gwTestingTokenMaxAge {
		return check{label: "token age", verdict: verdictUnverifiable,
			detail: fmt.Sprintf("credentials are %s old, past the %d-day Testing limit; if the app is still in Testing it has already stopped working",
				plural(days, "day"), int(gwTestingTokenMaxAge.Hours()/24)),
			evidence: path + " last written " + plural(days, "day") + " ago",
			todo:     "pi-stack gworkspace setup"}
	}
	return check{label: "token age", verdict: verdictUnverifiable,
		detail: fmt.Sprintf("credentials are %s old; an app still in Testing expires them at %d days",
			plural(days, "day"), int(gwTestingTokenMaxAge.Hours()/24)),
		evidence: path + " last written " + plural(days, "day") + " ago"}
}

// gworkspaceCredentialAge finds the stored credential file and returns its
// age. It reads only the file's modification time — never its contents.
func gworkspaceCredentialAge(env shellEnv, now time.Time) (string, time.Duration, bool) {
	if env.fileModTime == nil {
		return "", 0, false
	}
	for _, p := range gworkspaceCredentialPaths(env) {
		if mt, ok := env.fileModTime(p); ok {
			age := now.Sub(mt)
			if age < 0 {
				age = 0
			}
			return p, age, true
		}
	}
	return "", 0, false
}

// ---------------------------------------------------------------------------
// disable
// ---------------------------------------------------------------------------

func runGworkspaceDisableCmd(argv []string) {
	if wantsHelp(argv) {
		fmt.Print(gworkspaceDisableUsage)
		return
	}
	if len(argv) > 0 {
		fmt.Fprintf(os.Stderr, "pi-stack gworkspace disable: unexpected argument %q\n\n%s", argv[0], gworkspaceDisableUsage)
		os.Exit(2)
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack gworkspace disable: loading config: %v\n", err)
		os.Exit(1)
	}
	if err := gworkspaceDisable(cfg, defaultShellEnv(), os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack gworkspace disable: %v\n", err)
		os.Exit(1)
	}
}

// gworkspaceDisable removes ONLY the two pieces pi-stack owns: the config
// entries it wrote (google_workspace_account + the MCP set membership) and
// the gateway registration it created. It never touches the user's OAuth
// client, tokens, or the dependency CLI's home — those are the user's
// credentials, not ours, and deleting someone's Google grant because they
// turned an integration off is not a reversible act.
//
// Order mirrors setup in reverse: unregister FIRST, then save. A save that
// fails after a successful unregister leaves config claiming a server the
// gateway no longer has, which `status` reports honestly as a missing
// registration; the reverse order would leave a persisted "disabled" config
// while the gateway still spawns the server, which is the dangerous drift.
func gworkspaceDisable(cfg *config.Config, env shellEnv, out io.Writer) error {
	configured := strings.TrimSpace(cfg.GogAccount) != "" || mcpConfigured(cfg, gwServerName)
	snap := snapshotGogRegistration(env)

	if !configured && snap.state == gogRegAbsent {
		fmt.Fprintln(out, "Google Workspace is not configured; nothing to remove.")
		return nil
	}
	if snap.state == gogRegUnknown {
		return fmt.Errorf("could not confirm the %s registration (sbx mcp ls did not resolve cleanly): "+
			"refusing to remove config while the gateway state is unreadable; check the sbx daemon (sbx mcp status), "+
			"then re-run pi-stack gworkspace disable", gwServerName)
	}

	if snap.state == gogRegPresent {
		if env.run == nil {
			return fmt.Errorf("internal: shellEnv.run not wired")
		}
		if _, err := env.run("sbx", "mcp", "rm", gwServerName); err != nil {
			return fmt.Errorf("removing the %s registration: %w (remove it by hand: sbx mcp rm %s)", gwServerName, err, gwServerName)
		}
		fmt.Fprintln(out, "  removed registration: "+gwServerName)
	}

	changed := false
	if strings.TrimSpace(cfg.GogAccount) != "" {
		cfg.SetGogAccount("")
		changed = true
	}
	if cfg.RemoveMCP(gwServerName) {
		changed = true
	}
	if changed {
		if err := cfg.Save(); err != nil {
			return fmt.Errorf("saving config: %w (the gateway registration was already removed; re-run pi-stack gworkspace disable)", err)
		}
		fmt.Fprintln(out, "  cleared config: google_workspace_account, mcp "+gwServerName)
	}

	fmt.Fprintln(out, "Google Workspace is off. Your Google credentials were left untouched.")
	fmt.Fprintln(out, "  revoke them yourself if you want them gone: "+gwAudienceURL)
	return nil
}

// gworkspaceCredentialPaths are the candidate locations of the dependency
// CLI's stored token, most specific first. $GOG_HOME wins when set; otherwise
// the documented default under the user's config dir.
func gworkspaceCredentialPaths(env shellEnv) []string {
	var roots []string
	if env.getenv != nil {
		if h := strings.TrimSpace(env.getenv("GOG_HOME")); h != "" {
			roots = append(roots, h)
		}
	}
	if env.homeDir != nil {
		if h := strings.TrimSpace(env.homeDir()); h != "" {
			roots = append(roots, h+"/.config/gog")
		}
	}
	var paths []string
	for _, r := range roots {
		paths = append(paths, r+"/tokens.json", r+"/credentials.json", r+"/keyring.json")
	}
	return paths
}
