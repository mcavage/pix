// gworkspace.go is the PUBLIC Google Workspace surface: `pix gworkspace
// setup|status|disable`, plus the `pix setup --google-workspace` route
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

	"pix/host/config"
)

const (
	// gwServerName is the MCP registration + display name. Everything that
	// registers, lists, probes, or removes the server uses this constant, so
	// the gateway name and the public name can never drift.
	gwServerName           = "google-workspace"
	gwDocsCreateServerName = "google-docs-create"
	gwAccessCreateDocs     = "create-docs"
	// gwInstallCmd is the ONE place the external binary's package name is
	// allowed to reach the user, per the naming rule above.
	gwInstallCmd = "brew install openclaw/tap/gogcli"
	// gwUpgradeCmd is its upgrade twin.
	gwUpgradeCmd = "brew upgrade openclaw/tap/gogcli"
	// gwPermissionsURL is where a user can revoke credentials that Pix leaves
	// untouched during disable.
	gwPermissionsURL = "https://myaccount.google.com/permissions"
)

const gworkspaceUsage = `usage: pix gworkspace <setup|status|disable> [args]

  setup     guided read-only Google Workspace onboarding; --create-docs adds
            new-document creation without existing-document edits or sending
  status    what is configured and what is proven by a live headless probe
  disable   remove the Pix-owned config + gateway registration (your Google
            credentials are left untouched)

Google Workspace is OPTIONAL and absent unless you set it up.
Run 'pix gworkspace setup -h' for its flags.
`

const gworkspaceSetupUsage = `usage: pix gworkspace setup [--account <email>] [--credentials <path>] [--create-docs] [--yes]

Guides Google Workspace onboarding end to end:
  1. checks the dependency CLI is installed (exact install command if not),
     then validates your credentials path is a true regular file and imports
     it by invoking that CLI: this command never reads or prints its
     contents, and never copies it into pix config
  2. probes the selected auth route's OWN subcommand help/flags for the exact
     permission profile requested
  3. preflights EVERY remaining predictable hard requirement BEFORE any
     authorization happens: sbx must be installed (it registers the server
     with the gateway; a missing sbx fails this command, it never reports a
     silent "would register"), config must load cleanly, and whatever
     registration already exists must be CONFIRMED absent, or present with a
     readable command. An unreadable/unlistable prior registration aborts
     HERE, before any authorization runs and before config is touched
  4. authorizes <email>: read-only by default; --create-docs requests
     Gmail-read-only plus Drive-file-scoped document creation. If the
     dependency cannot advertise the needed flags, setup fails before OAuth
     (may open a browser; inherits this terminal)
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
  --create-docs          additionally expose create-new-Doc (never edit an
                         existing Doc; Gmail and Slack sending stay blocked)
  --yes                  never prompt (fails instead of asking on a TTY)

On a real terminal, a missing --account/--credentials is prompted for.
Idempotent: re-running a healthy setup is safe and re-registers the server.

pix does not independently inspect the OAuth scopes actually granted (no
stable scope-inspection surface exists to check against). The default grant
requests read-only scopes. --create-docs requests Gmail-read-only and
Drive-file-scoped access, while the agent surface exposes only new-document
creation. The ordinary registered server remains runtime read-only.

No organization OAuth client is bundled or referenced here; bring your own.
`

const gworkspaceStatusUsage = `usage: pix gworkspace status

Reports, from probes rather than config claims:
  - whether an account is configured (google_workspace_account)
  - whether the sbx gateway has ` + gwServerName + ` registered, and whether
    the registered command carries the hardened read-only flags
  - whether the headless spawn the gateway uses returns tools

exit: 0 ready · 1 needs setup · 3 could not be verified from here
`

const gworkspaceDisableUsage = `usage: pix gworkspace disable

Removes ONLY the Pix-owned pieces:
  - google_workspace_account and the ` + gwServerName + ` entry in config.toml
  - the ` + gwServerName + ` registration in the sbx gateway

Your Google OAuth credentials and tokens are left untouched on disk; revoke
them yourself at ` + gwPermissionsURL + ` if you want them gone.
A clean no-op (exit 0) when nothing is configured.
`

// runGworkspaceCmd is the `pix gworkspace` verb tree.
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
		fmt.Fprintf(os.Stderr, "pix gworkspace: unknown subcommand %q (want: setup, status, disable)\n", argv[0])
		os.Exit(2)
	}
}

// gworkspaceSetupOpts is the parsed `gworkspace setup` flag set. It is the
// same shape `pix setup --google-workspace` builds, so both doors reach
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
		case a == "--create-docs":
			o.access = gwAccessCreateDocs
		case a == "--yes", a == "-y", a == "--non-interactive":
			o.assumeYes = true
		case a == "-h", a == "--help":
			return o, errHelpRequested
		default:
			return o, fmt.Errorf("unknown flag %q (see: pix gworkspace setup -h)", a)
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
		fmt.Fprintf(os.Stderr, "pix gworkspace setup: %v\n\n%s", err, gworkspaceSetupUsage)
		os.Exit(2)
	}
	tty := isTTY(os.Stdin)
	if opts.assumeYes {
		tty = false // --yes means "never prompt", even on a real terminal
	}
	if err := gworkspaceSetup(defaultShellEnv(), opts, os.Stdin, os.Stdout, tty); err != nil {
		fmt.Fprintf(os.Stderr, "pix gworkspace setup: %v\n", err)
		os.Exit(1)
	}
}

// gworkspaceSetup is the façade over the unchanged gog_setup.go transaction.
func gworkspaceSetup(env shellEnv, opts gworkspaceSetupOpts, in io.Reader, out io.Writer, tty bool) error {
	return gogSetup(env, opts, in, out, tty)
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
		fmt.Fprintf(os.Stderr, "pix gworkspace status: unexpected argument %q\n\n%s", argv[0], gworkspaceStatusUsage)
		os.Exit(2)
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix gworkspace status: loading config: %v\n", err)
		os.Exit(1)
	}
	os.Exit(gworkspaceStatus(cfg, defaultShellEnv(), os.Stdout))
}

// gworkspaceStatus renders the Google Workspace surface and returns the
// process exit code. Every green here comes from a probe: config membership
// is rendered as intent, never as readiness.
func gworkspaceStatus(cfg *config.Config, env shellEnv, out io.Writer) int {
	if cfg.GoogleWorkspaceAccess == gwAccessCreateDocs {
		fmt.Fprintln(out, "Google Workspace (optional; read access + create-new-Docs only)")
	} else {
		fmt.Fprintln(out, "Google Workspace (optional, read-only, via the host MCP gateway)")
	}

	acct := gogAccount(cfg, env)
	if acct == "" && !mcpConfigured(cfg, gwServerName) {
		fmt.Fprintln(out, "  · not configured — set it up: pix gworkspace setup")
		return 0
	}

	var checks []check
	if acct == "" {
		checks = append(checks, check{label: "account", verdict: verdictTodo,
			detail:   gwServerName + " is in the configured MCP set but no account is authorized",
			evidence: "google_workspace_account is unset",
			todo:     "pix gworkspace setup"})
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
				todo:     "pix gworkspace setup"})
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
			evidence: "sbx mcp inspect " + gwServerName + " did not resolve a command",
			todo:     "sbx mcp status"})
	}
	if cfg.GoogleWorkspaceAccess == gwAccessCreateDocs {
		if argv, ok := registeredMCPCommand(env, gwDocsCreateServerName); ok {
			if trusted, ok := recognizedMCPArgv(env, argv, gwDocsCreateServerName); ok {
				checks = append(checks, docsCreateSpawnCheck(probeListTools(env, trusted)))
			} else {
				checks = append(checks, check{label: "create Docs", verdict: verdictUnverifiable,
					detail: "registered command is not the canonical Pix host command", todo: "pix gworkspace setup --create-docs"})
			}
		} else {
			checks = append(checks, check{label: "create Docs", verdict: verdictTodo,
				detail: "create-new-Docs server is not registered", todo: "pix gworkspace setup --create-docs"})
		}
	}

	for _, c := range checks {
		fmt.Fprintf(out, "  %s %-15s %s\n", checkGlyph(c), c.label, c.detail)
		if c.todo != "" {
			fmt.Fprintf(out, "      fix: %s\n", c.todo)
		}
	}
	return gworkspaceExit(checks)
}

func docsCreateSpawnCheck(res probeResult) check {
	switch res.status {
	case probeToolsOK:
		return check{label: "create Docs", verdict: verdictReady,
			detail:   "create-new-Docs tool exposed (existing Docs remain immutable)",
			evidence: fmt.Sprintf("--list-tools returned %s", plural(res.tools, "tool"))}
	case probeNoTools:
		return check{label: "create Docs", verdict: verdictTodo,
			detail: "create-new-Docs server returned 0 tools", todo: "pix gworkspace setup --create-docs"}
	case probeDeniedByPolicy:
		return check{label: "create Docs", verdict: verdictDenied,
			detail: "create-new-Docs spawn was refused by policy"}
	default:
		return check{label: "create Docs", verdict: verdictUnverifiable,
			detail: "probe " + res.detail + " — could not verify", todo: "sbx mcp inspect " + gwDocsCreateServerName}
	}
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
		fmt.Fprintf(os.Stderr, "pix gworkspace disable: unexpected argument %q\n\n%s", argv[0], gworkspaceDisableUsage)
		os.Exit(2)
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix gworkspace disable: loading config: %v\n", err)
		os.Exit(1)
	}
	if err := gworkspaceDisable(cfg, defaultShellEnv(), os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "pix gworkspace disable: %v\n", err)
		os.Exit(1)
	}
}

// gworkspaceDisable removes ONLY the two pieces pix owns: the config
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
	configured := strings.TrimSpace(cfg.GogAccount) != "" || mcpConfigured(cfg, gwServerName) || mcpConfigured(cfg, gwDocsCreateServerName)
	snap := snapshotGogRegistration(env)
	docsSnap := snapshotMCPRegistration(env, gwDocsCreateServerName)

	if !configured && snap.state == gogRegAbsent && docsSnap.state == gogRegAbsent {
		fmt.Fprintln(out, "Google Workspace is not configured; nothing to remove.")
		return nil
	}
	if snap.state == gogRegUnknown || docsSnap.state == gogRegUnknown {
		return fmt.Errorf("could not confirm the %s registration (sbx mcp ls did not resolve cleanly): "+
			"refusing to remove config while the gateway state is unreadable; check the sbx daemon (sbx mcp status), "+
			"then re-run pix gworkspace disable", gwServerName)
	}

	if snap.state == gogRegPresent {

		if _, err := env.Run("sbx", "mcp", "rm", gwServerName); err != nil {
			return fmt.Errorf("removing the %s registration: %w (remove it by hand: sbx mcp rm %s)", gwServerName, err, gwServerName)
		}
		fmt.Fprintln(out, "  removed registration: "+gwServerName)
	}
	if docsSnap.state == gogRegPresent {
		if _, err := env.Run("sbx", "mcp", "rm", gwDocsCreateServerName); err != nil {
			return fmt.Errorf("removing the %s registration: %w", gwDocsCreateServerName, err)
		}
		fmt.Fprintln(out, "  removed registration: "+gwDocsCreateServerName)
	}

	changed := false
	if strings.TrimSpace(cfg.GogAccount) != "" {
		cfg.SetGogAccount("")
		changed = true
	}
	if cfg.GoogleWorkspaceAccess != "" {
		cfg.SetGoogleWorkspaceAccess("")
		changed = true
	}
	if cfg.RemoveMCP(gwServerName) {
		changed = true
	}
	if cfg.RemoveMCP(gwDocsCreateServerName) {
		changed = true
	}
	if changed {
		if err := cfg.Save(); err != nil {
			return fmt.Errorf("saving config: %w (the gateway registration was already removed; re-run pix gworkspace disable)", err)
		}
		fmt.Fprintln(out, "  cleared config: google_workspace_account, mcp "+gwServerName)
	}

	fmt.Fprintln(out, "Google Workspace is off. Your Google credentials were left untouched.")
	fmt.Fprintln(out, "  revoke them yourself if you want them gone: "+gwPermissionsURL)
	return nil
}
