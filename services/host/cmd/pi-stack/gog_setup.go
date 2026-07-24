// gog_setup.go implements `pi-stack gog setup` — the guided, PUBLIC path to
// wiring up Google Workspace via the `gog` host MCP server (docs/gog-setup.md).
//
// Scope is deliberately thin: this is an ORCHESTRATOR over the installed `gog`
// CLI, config, and the existing MCP registrar — never a credential store. It
// never reads or prints the supplied OAuth client JSON's contents (only its
// path, to pass as an argv), never embeds an organization's client, and
// authorization/import always run through the user's own installed `gog`.
//
// It is version-aware: `gog auth --help` is probed once and the FIRST
// supported route wins — the current one-shot `auth setup <email> --credentials
// <path> --login`, else the current two-step `auth credentials <path>` + `auth
// add <email>`, else the older `auth add-client <path>` + `--account <email>
// auth login`. If none of those subcommands are advertised, this prints the
// installed version + upgrade guidance rather than guessing an obsolete
// command.
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"pi-stack/host/config"
)

const gogUsage = `usage: pi-stack gog <setup> [args]

  setup   guided Google Workspace (gog) onboarding: CLI check, OAuth client
          import, account authorization, headless verification, config +
          gateway registration

Run 'pi-stack gog setup -h' for its flags.
`

const gogSetupUsage = `usage: pi-stack gog setup [--account <email>] [--credentials <path>] [--yes]

Guides Google Workspace (gog) onboarding end to end:
  1. checks the gog CLI is installed (exact install guidance if not)
  2. imports YOUR Desktop OAuth client JSON by invoking gog itself — this
     command never reads or prints its contents, and never copies it into
     pi-stack config
  3. authorizes <email> (may open a browser; inherits this terminal)
  4. verifies interactive auth, THEN verifies headless tools the same way
     the sbx gateway will actually spawn gog — a healthy interactive auth
     with zero headless tools is a documented trap (see docs/gog-setup.md)
     and FAILS this command with the exact fix, rather than claiming ready
  5. on success: saves gog_account, enables gog in the configured MCP set,
     and registers it with the sbx gateway

flags:
  --account <email>      the Google Workspace account to authorize
  --credentials <path>   path to your Desktop OAuth client JSON (regular file)
  --yes                  never prompt (fails instead of asking on a TTY)

On a real terminal, a missing --account/--credentials is prompted for.
Idempotent: re-running a healthy setup is safe and re-registers gog.

No organization OAuth client is bundled or referenced here — bring your own.
`

// runGogCmd is the `pi-stack gog` verb tree. Today it has one subcommand,
// `setup`; the tree exists so a later addition (e.g. `gog status`) has a home.
func runGogCmd(argv []string) {
	if wantsHelp(argv) {
		fmt.Print(gogUsage)
		return
	}
	if len(argv) == 0 {
		fmt.Fprint(os.Stderr, gogUsage)
		os.Exit(2)
	}
	switch argv[0] {
	case "setup":
		runGogSetupCmd(argv[1:])
	default:
		fmt.Fprintf(os.Stderr, "pi-stack gog: unknown subcommand %q (want: setup)\n", argv[0])
		os.Exit(2)
	}
}

// gogSetupOpts is the parsed `gog setup` flag set.
type gogSetupOpts struct {
	account     string
	credentials string
	assumeYes   bool
}

func parseGogSetupArgs(argv []string) (gogSetupOpts, error) {
	var o gogSetupOpts
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		next := func() (string, error) {
			if i+1 >= len(argv) {
				return "", fmt.Errorf("%s needs a value", a)
			}
			i++
			return argv[i], nil
		}
		switch a {
		case "--account":
			v, err := next()
			if err != nil {
				return o, err
			}
			o.account = v
		case "--credentials":
			v, err := next()
			if err != nil {
				return o, err
			}
			o.credentials = v
		case "--yes", "-y", "--non-interactive":
			o.assumeYes = true
		case "-h", "--help":
			return o, errHelpRequested
		default:
			return o, fmt.Errorf("unknown flag %q (see: pi-stack gog setup -h)", a)
		}
	}
	return o, nil
}

// runGogSetupCmd is the real `pi-stack gog setup` entry: parses flags, wires
// the real shellEnv (browser-opening auth steps inherit THIS process's stdio),
// and runs gogSetup.
func runGogSetupCmd(argv []string) {
	opts, err := parseGogSetupArgs(argv)
	if err != nil {
		if err == errHelpRequested {
			fmt.Print(gogSetupUsage)
			return
		}
		fmt.Fprintf(os.Stderr, "pi-stack gog setup: %v\n\n%s", err, gogSetupUsage)
		os.Exit(2)
	}
	tty := isTTY(os.Stdin)
	if opts.assumeYes {
		tty = false // --yes means "never prompt", even on a real terminal
	}
	if err := gogSetup(defaultShellEnv(), opts, os.Stdin, os.Stdout, tty); err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack gog setup: %v\n", err)
		os.Exit(1)
	}
}

// gogAuthRoute is one supported way to import an OAuth client + authorize an
// account with the installed gog CLI. commands returns the argv sequence to
// run, in order, via env.runInteractive.
type gogAuthRoute struct {
	name     string
	commands func(account, credentials string) [][]string
}

// gogAuthSubcommands scans a `gog auth --help` listing for the subcommand
// names this file cares about, matching a subcommand at the start of a
// (trimmed) line followed by whitespace — the shape Cobra-style "Available
// Commands:" listings use. "add" deliberately does not match an "add-client"
// line (no whitespace follows "add" there).
func gogAuthSubcommands(help string) map[string]bool {
	want := map[string]bool{"setup": true, "credentials": true, "add": true, "add-client": true, "login": true}
	found := map[string]bool{}
	for _, line := range strings.Split(help, "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		for name := range want {
			if found[name] {
				continue
			}
			if trimmed == name {
				found[name] = true
				continue
			}
			rest := strings.TrimPrefix(trimmed, name)
			if rest != trimmed && len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\t') {
				found[name] = true
			}
		}
	}
	return found
}

// chooseGogAuthRoute picks the first supported route from a `gog auth --help`
// listing, in preference order: the current one-shot `setup`, the current
// two-step `credentials`+`add`, then the OLDER repo-documented `add-client`+
// `login`. ok is false when none of those surfaces are advertised — callers
// must not blindly exec an obsolete command in that case.
func chooseGogAuthRoute(help string) (gogAuthRoute, bool) {
	subs := gogAuthSubcommands(help)
	switch {
	case subs["setup"]:
		return gogAuthRoute{
			name: "setup",
			commands: func(account, credentials string) [][]string {
				return [][]string{{"auth", "setup", account, "--credentials", credentials, "--login"}}
			},
		}, true
	case subs["credentials"] && subs["add"]:
		return gogAuthRoute{
			name: "credentials+add",
			commands: func(account, credentials string) [][]string {
				return [][]string{
					{"auth", "credentials", credentials},
					{"auth", "add", account},
				}
			},
		}, true
	case subs["add-client"] && subs["login"]:
		return gogAuthRoute{
			name: "add-client+login (legacy)",
			commands: func(account, credentials string) [][]string {
				return [][]string{
					{"auth", "add-client", credentials},
					{"--account", account, "auth", "login"},
				}
			},
		}, true
	}
	return gogAuthRoute{}, false
}

// gogSetupAccountHealthy reports whether acct's gog auth is ALREADY confirmed
// healthy: interactive auth (gogAuthed) passes, and — only when the headless
// path can actually be probed (op installed + an op-refs.env resolves) —
// headless tools are also non-empty. When headless can't be probed at all it
// is NOT counted against health here (see docs/gog-setup.md: macOS system
// keychain skips that step entirely); `pi-stack gog setup`/doctor still surface
// that gap on their own. Used by `pi-stack setup --account` to decide whether
// to print the `pi-stack gog setup` follow-up — it never runs the OAuth flow
// itself.
func gogSetupAccountHealthy(env shellEnv, acct string) bool {
	if !gogAuthed(env, acct) {
		return false
	}
	opRefs := resolveOpRefs(env)
	if _, err := env.lookPath("op"); err != nil || opRefs == "" {
		return true // headless unverifiable here; not a confirmed unhealthy state
	}
	return gogHeadlessOK(env, acct, opRefs)
}

// gogSetup is the hermetically-testable core of `pi-stack gog setup`. All OS
// contact (lookPath/run/statFile/runInteractive) goes through env; account/
// credentials prompting is gated on tty (never on a bare non-TTY run, never
// when --yes was given).
func gogSetup(env shellEnv, opts gogSetupOpts, in io.Reader, out io.Writer, tty bool) error {
	// A single shared bufio.Reader across BOTH prompts: promptLine's own
	// bufio.NewReader-per-call would silently drop the second answer here (its
	// first read can buffer past the first line on a fully-buffered io.Reader,
	// e.g. in tests, though a real interactive terminal delivers one line per
	// Read and never shows the bug). reset.go/onboard.go only ever prompt once
	// per promptLine call, so this is new here, not a pre-existing regression.
	reader := bufio.NewReader(in)
	prompt := func(msg string) string {
		fmt.Fprint(out, msg)
		line, _ := reader.ReadString('\n')
		return strings.TrimSpace(line)
	}

	account := strings.TrimSpace(opts.account)
	if account == "" && tty {
		account = prompt("Google Workspace account (email): ")
	}
	if account == "" {
		return fmt.Errorf("--account <email> is required (non-interactive, or no answer given)")
	}

	credentials := strings.TrimSpace(opts.credentials)
	if credentials == "" && tty {
		credentials = prompt("Path to your Desktop OAuth client JSON: ")
	}
	if credentials == "" {
		return fmt.Errorf("--credentials <path> is required (non-interactive, or no answer given)")
	}

	if env.lookPath == nil {
		return fmt.Errorf("internal: shellEnv.lookPath not wired")
	}
	if _, err := env.lookPath("gog"); err != nil {
		fmt.Fprintln(out, "gog CLI not found.")
		fmt.Fprintln(out, "  install it:  brew install gog   (or see https://gogcli.sh/install.html)")
		return fmt.Errorf("gog is not installed")
	}

	// Validate the credentials path BEFORE ever handing it to gog: must be a
	// regular file. This command never opens/reads it — only checks existence
	// and passes the path through as an argv token.
	if env.statFile == nil || !env.statFile(credentials) {
		return fmt.Errorf("credentials file not found (must be a regular file): %s", credentials)
	}

	helpOut, _ := env.run("gog", "auth", "--help")
	route, ok := chooseGogAuthRoute(helpOut)
	if !ok {
		verOut, _ := env.run("gog", "--version")
		fmt.Fprintln(out, "installed gog does not advertise a supported auth import/authorize surface")
		fmt.Fprintln(out, "  (looked for: auth setup | auth credentials+add | auth add-client+login)")
		fmt.Fprintf(out, "  installed version: %s\n", strings.TrimSpace(verOut))
		fmt.Fprintln(out, "  upgrade: brew upgrade gog   (or see https://gogcli.sh/install.html)")
		fmt.Fprintln(out, "  then re-run: pi-stack gog setup")
		return fmt.Errorf("no supported gog auth route for the installed gog version")
	}

	runInteractive := env.runInteractive
	if runInteractive == nil {
		runInteractive = func(name string, args ...string) error {
			return fmt.Errorf("internal: shellEnv.runInteractive not wired")
		}
	}

	fmt.Fprintf(out, "Importing your OAuth client + authorizing %s (gog auth route: %s)...\n", account, route.name)
	fmt.Fprintln(out, "This may open a browser for you to sign in.")
	for _, argv := range route.commands(account, credentials) {
		fmt.Fprintf(out, "  running: gog %s\n", strings.Join(argv, " "))
		if err := runInteractive("gog", argv...); err != nil {
			return fmt.Errorf("gog %s: %w", strings.Join(argv, " "), err)
		}
	}

	if _, err := env.run("gog", "--account", account, "auth", "doctor", "--check"); err != nil {
		fmt.Fprintln(out, "authorization did not verify:")
		fmt.Fprintf(out, "  gog --account %s auth doctor --check\n", account)
		return fmt.Errorf("gog auth doctor --check failed for %s: %w", account, err)
	}
	fmt.Fprintf(out, "interactive auth OK for %s\n", account)

	// Verify headless tools the SAME host-side path doctor uses. This is the
	// documented gws-style trap: interactive auth in a logged-in shell proves
	// nothing about the bare env the sbx gateway spawns gog in.
	opRefs := resolveOpRefs(env)
	_, opErr := env.lookPath("op")
	canProbeHeadless := opErr == nil && opRefs != ""
	if canProbeHeadless {
		if gogHeadlessOK(env, account, opRefs) {
			fmt.Fprintln(out, "headless tools OK (verified the same host-side path the sbx gateway/doctor use)")
		} else {
			fmt.Fprintln(out, "interactive auth is healthy, but the headless path returns 0 tools.")
			fmt.Fprintln(out, "  this is the documented trap: the gateway spawns gog in a bare, non-interactive")
			fmt.Fprintln(out, "  env and can't unlock the keyring without help.")
			fmt.Fprintf(out, "  add GOG_KEYRING_BACKEND=file + GOG_KEYRING_PASSWORD + GOG_ACCOUNT + GOG_HOME to %s\n", defaultOpRefsPath(env))
			return fmt.Errorf("headless verification failed for %s — not registering until this is fixed", account)
		}
	} else {
		fmt.Fprintln(out, "headless verification skipped (no op + op-refs.env found) — fine on macOS system")
		fmt.Fprintln(out, "  keychain; if the gateway later returns 0 tools, see docs/gog-setup.md step 3.")
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	cfg.SetGogAccount(account)
	// AC-08 idempotency: ALWAYS ensure gog is in the configured MCP set, even
	// when the account already matched (a healthy re-run must still leave gog
	// attached/registered deterministically — see the onboard.go fix this
	// mirrors).
	cfg.AddMCP("gog")
	if err := cfg.Save(); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}

	if err := registerServers(cfg, env, out, []string{"gog"}, hostBinaryResolver); err != nil {
		return fmt.Errorf("registering gog with the sbx gateway: %w (finish later: pi-stack mcp register gog)", err)
	}

	fmt.Fprintln(out, "")
	if len(resolveStaticMCP([]string{"gog"}, cfg)) > 0 {
		fmt.Fprintln(out, "gog is EAGERLY attached (in mcp_static) — a fresh sandbox creation will have it in context from the start.")
	} else {
		fmt.Fprintln(out, "gog is dynamically discoverable by default (lean context) — the in-VM agent finds + calls it on demand.")
	}
	fmt.Fprintln(out, "Existing sandbox? attach it live: pi-stack mcp load gog")
	return nil
}
