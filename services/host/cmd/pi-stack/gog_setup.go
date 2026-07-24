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
//
// Every route also requests read-only OAuth scopes explicitly, at grant time:
// the OAuth-granting step always carries gog's own --readonly flag, and that
// flag's presence is capability-probed against the SELECTED route's own
// subcommand help (not just the top-level subcommand names) before any
// interactive command runs. If the installed gog cannot advertise --readonly
// on that step, this fails with upgrade/manual guidance rather than silently
// authorizing without it or falling back to an older, unguarded route.
// pi-stack does not independently inspect the OAuth scopes actually granted
// (gog exposes no stable, parseable scope-inspection surface to check
// against) — the guarantee is that the grant REQUESTS read-only scopes, and
// the runtime backstop (`--gmail-no-send --wrap-untrusted --readonly
// --allow-tool read`, set at MCP-serve time in mcp.go) blocks writes
// regardless.
//
// `pi-stack gog setup` requires sbx: a missing sbx binary, or a failed `sbx
// mcp add`, is a hard failure — never a silent "would register" success. Its
// config write is also ordered to prevent drift: the account/MCP change is
// built in memory, actual sbx registration must succeed FIRST, and only then
// is config.toml saved; a registration failure never touches the persisted
// config, and a save failure AFTER a successful registration rolls the sbx
// side back (restoring whatever was registered before, or removing the new
// registration) rather than leaving config and the gateway to drift apart.
//
// R2-03/R2-04 (review round 2): every PREDICTABLE hard requirement — config
// loads cleanly, the gog CLI and its selected auth route are capable, the
// credentials path is a true regular file, sbx is on PATH, and whatever gog
// registration already exists can be confirmed — is checked BEFORE the first
// OAuth side effect (the interactive auth route below). The prior
// registration check in particular is TRI-STATE (gogRegSnapshot): confirmed
// absent, confirmed present with a restorable argv, or unknown (the bounded
// `sbx mcp ls` listing itself failed, or gog is listed but its registered
// command can't be parsed/read). Unknown is never treated as absent — this
// command refuses to authorize or overwrite the registration until it can be
// read, so an unreadable or momentarily-unlistable prior registration is
// never silently clobbered by a same-run rollback.
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
  1. checks the gog CLI is installed (exact install guidance if not), then
     validates your credentials path is a true regular file and imports it
     by invoking gog itself: this command never reads or prints its
     contents, and never copies it into pi-stack config
  2. probes the selected auth route's OWN subcommand help/flags for the
     read-only capability it needs at grant time (see step 3)
  3. preflights EVERY remaining predictable hard requirement BEFORE any
     authorization happens: sbx must be installed (it registers gog with
     the gateway; a missing sbx fails this command, it never reports a
     silent "would register"), config must load cleanly, and whatever gog
     registration already exists must be CONFIRMED, absent, or present
     with a readable command. An unreadable/unlistable prior registration
     (or a transiently unavailable sbx listing) aborts HERE, before any
     authorization runs and before config is touched, rather than risking
     that registration on a same-run rollback later
  4. authorizes <email> REQUESTING READ-ONLY OAUTH SCOPES at grant time
     (gog's --readonly flag on the OAuth-granting command); if the
     installed gog cannot advertise --readonly for the selected route, this
     fails with upgrade guidance rather than authorizing without it (may
     open a browser; inherits this terminal)
  5. verifies interactive auth, THEN verifies headless tools the same way
     the sbx gateway will actually spawn gog: direct and bare when
     1Password isn't set up, through the same op wrapper when it is. A
     healthy interactive auth with zero headless tools is a documented trap
     (see docs/gog-setup.md) and FAILS this command with the exact fix,
     rather than claiming ready; an unverifiable probe (timeout/exec error)
     is never reported as success either
  6. on success: registers gog with the sbx gateway FIRST, and only once
     that succeeds saves gog_account + enables gog in the configured MCP
     set: a registration failure never touches the persisted config, and a
     save failure after a successful registration rolls the registration
     back (to exactly the step 3 snapshot) rather than leaving config and
     the gateway out of sync

flags:
  --account <email>      the Google Workspace account to authorize
  --credentials <path>   path to your Desktop OAuth client JSON (regular file)
  --yes                  never prompt (fails instead of asking on a TTY)

On a real terminal, a missing --account/--credentials is prompted for.
Idempotent: re-running a healthy setup is safe and re-registers gog.

pi-stack does not independently inspect the OAuth scopes gog actually grants
(no stable scope-inspection surface exists to check against); this command
guarantees the grant REQUESTS read-only scopes; gog's own runtime flags
(--gmail-no-send --wrap-untrusted --readonly --allow-tool read) are the
backstop that blocks writes regardless of what was granted.

No organization OAuth client is bundled or referenced here; bring your own.
`

// runGogCmd is the `pi-stack gog` verb tree. Today it has one subcommand,
// `setup`; the tree exists so a later addition (e.g. `gog status`) has a home.
//
// DX-1: the help gate is checked ONLY for the no-subcommand case (`gog -h` /
// `gog --help` / `gog` with nothing after it) — it must NOT blanket-scan the
// whole argv the way wantsHelp does for a plain flag verb. wantsHelp(argv)
// matches -h/--help ANYWHERE in argv, so gating on it before dispatching would
// catch `gog setup -h` too and print the noun-level gogUsage instead of ever
// reaching runGogSetupCmd, which owns the detailed gogSetupUsage (flags,
// numbered steps). Once a subcommand token is present, dispatch to it first
// and let it parse/handle its OWN -h/--help.
func runGogCmd(argv []string) {
	if len(argv) == 0 {
		fmt.Fprint(os.Stderr, gogUsage)
		os.Exit(2)
	}
	if wantsHelp(argv[:1]) {
		fmt.Print(gogUsage)
		return
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

// gogAuthStep is one command in a gogAuthRoute. help is the argv (after
// "gog") whose OWN --help output must be probed (R1-12: the SUBCOMMAND's
// help, not just the top-level `gog auth --help` names) before the step is
// ever executed; requiredFlags are the exact flag tokens that help text must
// advertise for the step to be trusted. Every step that performs an actual
// OAuth grant (as opposed to merely importing a client) carries "--readonly"
// in requiredFlags (R1-02): pi-stack never authorizes an account without
// requesting read-only scopes at grant time. argv builds the actual command
// (after "gog") to run.
type gogAuthStep struct {
	help          []string
	requiredFlags []string
	argv          func(account, credentials string) []string
}

// gogAuthRoute is one supported way to import an OAuth client + authorize an
// account with the installed gog CLI, as an ordered sequence of steps run via
// env.runInteractive.
type gogAuthRoute struct {
	name  string
	steps []gogAuthStep
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
			steps: []gogAuthStep{{
				help:          []string{"auth", "setup", "--help"},
				requiredFlags: []string{"--credentials", "--login", "--readonly"},
				argv: func(account, credentials string) []string {
					return []string{"auth", "setup", account, "--credentials", credentials, "--login", "--readonly"}
				},
			}},
		}, true
	case subs["credentials"] && subs["add"]:
		return gogAuthRoute{
			name: "credentials+add",
			steps: []gogAuthStep{
				{
					help: []string{"auth", "credentials", "--help"},
					argv: func(_, credentials string) []string {
						return []string{"auth", "credentials", credentials}
					},
				},
				{
					help:          []string{"auth", "add", "--help"},
					requiredFlags: []string{"--readonly"},
					argv: func(account, _ string) []string {
						return []string{"auth", "add", account, "--readonly"}
					},
				},
			},
		}, true
	case subs["add-client"] && subs["login"]:
		return gogAuthRoute{
			name: "add-client+login (legacy)",
			steps: []gogAuthStep{
				{
					help: []string{"auth", "add-client", "--help"},
					argv: func(_, credentials string) []string {
						return []string{"auth", "add-client", credentials}
					},
				},
				{
					help:          []string{"auth", "login", "--help"},
					requiredFlags: []string{"--readonly"},
					argv: func(account, _ string) []string {
						return []string{"--account", account, "auth", "login", "--readonly"}
					},
				},
			},
		}, true
	}
	return gogAuthRoute{}, false
}

// gogAuthRouteCapable is the R1-02/R1-12 capability probe: it checks EVERY
// step's OWN subcommand help (not just the top-level `gog auth --help`
// listing chooseGogAuthRoute already used to pick a candidate by name) and
// confirms every one of that step's requiredFlags literally appears in it.
// This is what enforces read-only OAuth authorization at grant time — a step
// that performs an OAuth grant always requires "--readonly", so a route whose
// installed CLI cannot advertise that flag is never run. ok is false on the
// FIRST unsupported step, with a detail identifying exactly what is missing;
// gogSetup fails outright on !ok rather than silently trying an older,
// possibly-unsafe route (R1-02: "do not fall back to an unsafe legacy
// route").
func gogAuthRouteCapable(env shellEnv, route gogAuthRoute) (bool, string) {
	for _, step := range route.steps {
		if len(step.requiredFlags) == 0 {
			continue
		}
		args := append([]string{"gog"}, step.help...)
		helpOut, _, _ := probeRun(env, args[0], args[1:]...)
		for _, flag := range step.requiredFlags {
			if !strings.Contains(helpOut, flag) {
				subcmd := strings.Join(step.help[:len(step.help)-1], " ")
				return false, fmt.Sprintf("gog %s does not advertise %s", subcmd, flag)
			}
		}
	}
	return true, ""
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
	// gogPath is captured HERE and never re-resolved: it feeds the immutable
	// registrar snapshot built below (buildGogRegistrar), the single source for
	// both the headless probe and the eventual registration (R3, finding #1).
	gogPath, gogErr := env.lookPath("gog")
	if gogErr != nil {
		fmt.Fprintln(out, "gog CLI not found.")
		fmt.Fprintln(out, "  install it:  brew install gog   (or see https://gogcli.sh/install.html)")
		return fmt.Errorf("gog is not installed")
	}

	// R2-05: validate the credentials path BEFORE ever handing it to gog: must
	// be a TRUE regular file — Mode().IsRegular(), not merely "exists and isn't
	// a directory" (which a FIFO, socket, or device would also satisfy).
	// env.fileMode wraps os.Stat (not os.Lstat), so a symlink POINTING AT a
	// regular file is allowed (Stat reports the TARGET's mode), while a FIFO,
	// socket, device, or a symlink to any of those is rejected. This command
	// never opens/reads its contents — only checks the mode and passes the path
	// through as an argv token.
	if env.fileMode == nil {
		return fmt.Errorf("internal: shellEnv.fileMode not wired")
	}
	credMode, credOK := env.fileMode(credentials)
	if !credOK || !credMode.IsRegular() {
		return fmt.Errorf("credentials file not found (must be a regular file): %s", credentials)
	}

	// R1-14: every noninteractive gog probe here (help/version/auth check) runs
	// through the BOUNDED probe machinery (timeout + output cap), so a hung gog
	// can never wedge setup. Tests without env.probe fall back to env.run.
	helpOut, _, _ := probeRun(env, "gog", "auth", "--help")
	route, ok := chooseGogAuthRoute(helpOut)
	if !ok {
		verOut, _, _ := probeRun(env, "gog", "--version")
		fmt.Fprintln(out, "installed gog does not advertise a supported auth import/authorize surface")
		fmt.Fprintln(out, "  (looked for: auth setup | auth credentials+add | auth add-client+login)")
		fmt.Fprintf(out, "  installed version: %s\n", strings.TrimSpace(verOut))
		fmt.Fprintln(out, "  upgrade: brew upgrade gog   (or see https://gogcli.sh/install.html)")
		fmt.Fprintln(out, "  then re-run: pi-stack gog setup")
		return fmt.Errorf("no supported gog auth route for the installed gog version")
	}

	// R1-02/R1-12: probe the SELECTED route's own subcommand help (not just the
	// top-level `gog auth --help` names already used above) for the exact flags
	// each step needs, including "--readonly" on every step that performs an
	// OAuth grant. If the installed CLI cannot guarantee a read-only grant here,
	// fail with upgrade/manual guidance rather than falling back to an older,
	// possibly-unsafe route — pi-stack never authorizes without requesting
	// read-only scopes at grant time.
	if capable, detail := gogAuthRouteCapable(env, route); !capable {
		verOut, _, _ := probeRun(env, "gog", "--version")
		fmt.Fprintf(out, "installed gog cannot guarantee read-only OAuth authorization for the %q route:\n", route.name)
		fmt.Fprintf(out, "  %s\n", detail)
		fmt.Fprintf(out, "  installed version: %s\n", strings.TrimSpace(verOut))
		fmt.Fprintln(out, "  upgrade: brew upgrade gog   (or see https://gogcli.sh/install.html)")
		fmt.Fprintln(out, "  then re-run: pi-stack gog setup")
		fmt.Fprintln(out, "  pi-stack never falls back to an auth route it cannot confirm requests read-only scopes.")
		return fmt.Errorf("installed gog cannot guarantee read-only OAuth authorization (route: %s): %s", route.name, detail)
	}

	// R2-04: preflight every remaining PREDICTABLE hard requirement here, BEFORE
	// the first OAuth side effect (the interactive route steps just below) ever
	// runs — sbx must be on PATH, config must load cleanly, and the prior gog
	// registration snapshot must be CONFIRMED (never unknown). Any failure here
	// returns before a single runInteractive call and before config is touched,
	// so a botched preflight can never leave OAuth half-run or config mutated.
	if _, err := env.lookPath("sbx"); err != nil {
		return fmt.Errorf("sbx not found: pi-stack gog setup requires sbx to register the MCP server " +
			"(install: https://docs.docker.com/ai/sandboxes); install it, then re-run pi-stack gog setup")
	}
	// R1-08: load the candidate config change IN MEMORY only — cfg.Save() must
	// never run before the sbx registration it describes actually succeeds, or
	// a registration failure would leave a persisted config claiming gog is
	// registered when it is not (config/registration drift).
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	// R2-03: a TRI-STATE, bounded snapshot of whatever gog registration already
	// exists BEFORE this run would overwrite it. gogRegUnknown (the bounded `sbx
	// mcp ls` listing itself failed/timed out, OR gog is confirmed listed but
	// its registered command can't be parsed/read) is NEVER treated as "nothing
	// to restore" — this command refuses to authorize or overwrite until the
	// prior registration is actually readable, so it can never be silently lost
	// to a same-run rollback.
	snap := snapshotGogRegistration(env)
	if snap.state == gogRegUnknown {
		return fmt.Errorf("could not confirm the prior gog registration (sbx mcp ls/get did not resolve cleanly): " +
			"refusing to authorize or overwrite it until this is readable; check the sbx daemon (sbx mcp status), " +
			"then re-run pi-stack gog setup")
	}

	runInteractive := env.runInteractive
	if runInteractive == nil {
		runInteractive = func(name string, args ...string) error {
			return fmt.Errorf("internal: shellEnv.runInteractive not wired")
		}
	}

	fmt.Fprintf(out, "Importing your OAuth client + authorizing %s (gog auth route: %s, read-only scopes requested)...\n", account, route.name)
	fmt.Fprintln(out, "This may open a browser for you to sign in.")
	for _, step := range route.steps {
		argv := step.argv(account, credentials)
		fmt.Fprintf(out, "  running: gog %s\n", strings.Join(argv, " "))
		if err := runInteractive("gog", argv...); err != nil {
			return fmt.Errorf("gog %s: %w", strings.Join(argv, " "), err)
		}
	}

	if _, timedOut, err := probeRun(env, "gog", "--account", account, "auth", "doctor", "--check"); timedOut || err != nil {
		fmt.Fprintln(out, "authorization did not verify:")
		fmt.Fprintf(out, "  gog --account %s auth doctor --check\n", account)
		if timedOut {
			return fmt.Errorf("gog auth doctor --check timed out for %s", account)
		}
		return fmt.Errorf("gog auth doctor --check failed for %s: %w", account, err)
	}
	fmt.Fprintf(out, "interactive auth OK for %s\n", account)

	// R3 TOCTOU fix (finding #1): resolve the registrar snapshot EXACTLY ONCE,
	// right here — op, op-refs, and gogPath are captured into ONE immutable
	// mcpRegistrar and never re-resolved after this point. The exact same reg
	// feeds BOTH the headless probe below (reg.execArgv, byte-for-byte what
	// gogRegisteredArgv/gogHeadlessProbe would build from these same inputs) and
	// the eventual registration (registerGogRegistrar -> reg.addArgs), so a
	// concurrent PATH or op-refs.env mutation between probe and registration can
	// never cause a DIFFERENT command to be registered than the one that was
	// just proven healthy (previously: registerServers independently re-resolved
	// op/op-refs/gog after this probe, opening exactly that window).
	//
	// Verify headless tools the SAME host-side path doctor uses — and the SAME
	// exact argv/flags/op-wrapper that registration is about to register. This
	// is the documented gws-style trap: interactive auth in a logged-in shell
	// proves nothing about the bare env the sbx gateway spawns gog in.
	//
	// R1-06: when op/op-refs are unavailable this MUST NOT skip verification —
	// reg falls back to the bare hardened invocation (minus the op wrapper),
	// bounded by the same probeListTools machinery. "macOS system keychain" is
	// not an excuse to skip the test: a clean zero-tools result still fails, and
	// an exec/timeout result is unverifiable and must never be reported as
	// success. Nothing here mutates config/registration yet — that only happens
	// after `head` is confirmed healthy, below.
	reg := buildGogRegistrar(env, gogPath, account)
	head := probeListTools(env, reg.execArgv("gog"))
	switch head.status {
	case probeToolsOK:
		fmt.Fprintln(out, "headless tools OK (verified the same host-side path the sbx gateway/doctor use)")
	case probeNoTools:
		fmt.Fprintln(out, "interactive auth is healthy, but the headless path returns 0 tools.")
		fmt.Fprintln(out, "  this is the documented trap: the gateway spawns gog in a bare, non-interactive")
		fmt.Fprintln(out, "  env and can't unlock the keyring without help.")
		fmt.Fprintf(out, "  add GOG_KEYRING_BACKEND=file + GOG_KEYRING_PASSWORD + GOG_ACCOUNT + GOG_HOME to %s\n", defaultOpRefsPath(env))
		return fmt.Errorf("headless verification failed for %s: not registering until this is fixed", account)
	default: // probeTimedOut or probeError — unverifiable, never claimed as success
		fmt.Fprintf(out, "headless verification could not be confirmed (%s): not registering until this is fixed\n", head.detail)
		return fmt.Errorf("headless verification unverifiable for %s (%s): not registering until this is fixed", account, head.detail)
	}

	// R1-08/R2-04: sbx presence, config.Load(), and the prior registration
	// snapshot were ALL already confirmed above, before the interactive auth
	// route ran — nothing left to preflight here, just apply the change.
	cfg.SetGogAccount(account)
	// AC-08 idempotency: ALWAYS ensure gog is in the configured MCP set, even
	// when the account already matched (a healthy re-run must still leave gog
	// attached/registered deterministically — see the onboard.go fix this
	// mirrors).
	cfg.AddMCP("gog")

	// R1-08/R3: REGISTER FIRST, save second. registerGogRegistrar runs `sbx mcp
	// add gog ...` using the EXACT reg snapshot resolved above (no re-resolution
	// — the TOCTOU fix); only once that has genuinely succeeded do we persist
	// gog_account/mcp to disk. A registration failure here returns before
	// cfg.Save() is ever called, so the persisted config is left byte-for-byte
	// unchanged — there is no config/registration drift to roll back from.
	if err := registerGogRegistrar(reg, env, out); err != nil {
		return fmt.Errorf("registering gog with the sbx gateway: %w (finish later: pi-stack mcp register gog)", err)
	}

	if err := cfg.Save(); err != nil {
		// The sbx side is now AHEAD of the (about-to-remain) persisted config:
		// registration just succeeded but we couldn't record it. Roll the sbx
		// side back so the two never drift apart — restore whatever was
		// registered before this run (if anything), else remove the just-added
		// registration entirely. Any rollback failure is folded into the
		// returned error explicitly rather than swallowed.
		if rerr := gogSetupRollbackRegistration(env, snap); rerr != nil {
			return fmt.Errorf("saving config: %w; additionally, rollback of the gog registration failed: %v; fix by hand (sbx mcp get gog / sbx mcp rm gog)", err, rerr)
		}
		return fmt.Errorf("saving config: %w (gog registration rolled back so config and the gateway stay in sync)", err)
	}

	fmt.Fprintln(out, "")
	if len(resolveStaticMCP([]string{"gog"}, cfg)) > 0 {
		fmt.Fprintln(out, "gog is EAGERLY attached (in mcp_static): a fresh sandbox creation will have it in context from the start.")
	} else {
		fmt.Fprintln(out, "gog is dynamically discoverable by default (lean context): the in-VM agent finds + calls it on demand.")
	}
	fmt.Fprintln(out, "Existing sandbox? attach it live: pi-stack mcp load gog")
	return nil
}

// gogSetupRollbackRegistration undoes a just-succeeded `sbx mcp add gog ...`
// when the config.Save() that was supposed to follow it fails (R1-08),
// so the sbx gateway and pi-stack's persisted config never drift apart:
//   - snap.state == gogRegPresent: snap.argv is the exact command that was
//     registered for gog BEFORE this run (captured by snapshotGogRegistration
//     before it was overwritten) — re-register it verbatim via `sbx mcp add`.
//   - otherwise (gogRegAbsent — gogRegUnknown is refused earlier by gogSetup's
//     preflight and never reaches here): nothing was registered before this
//     run — remove the new registration via `sbx mcp rm gog`.
//
// Every outcome is reported: nil on a confirmed rollback, a descriptive error
// (never silent) when the rollback itself fails, naming the exact command to
// run by hand.
func gogSetupRollbackRegistration(env shellEnv, snap gogRegSnapshot) error {
	if env.run == nil {
		return fmt.Errorf("internal: shellEnv.run not wired")
	}
	if snap.state == gogRegPresent {
		args := rawAddArgs("gog", snap.argv)
		if _, err := env.run("sbx", args...); err != nil {
			return fmt.Errorf("could not restore the prior gog registration: %w", err)
		}
		return nil
	}
	if _, err := env.run("sbx", "mcp", "rm", "gog"); err != nil {
		return fmt.Errorf("could not remove the new gog registration: %w", err)
	}
	return nil
}

// gogRegState is the TRI-STATE result of snapshotting whatever gog
// registration exists before gogSetup would overwrite it (R2-03).
type gogRegState int

const (
	// gogRegAbsent: the bounded `sbx mcp ls` listing was read successfully and
	// confirmed gog is NOT in it — safe to treat as "nothing to restore".
	gogRegAbsent gogRegState = iota
	// gogRegPresent: the bounded listing confirmed gog IS registered, and its
	// command argv was successfully read back — safe to restore verbatim.
	gogRegPresent
	// gogRegUnknown: gog's presence could not be confirmed either way (the
	// listing probe itself failed or timed out), OR it is confirmed present but
	// its registered command could not be parsed/read. Either way this must
	// NEVER be treated as absent: gogSetup's preflight aborts on this state
	// rather than risk losing, or silently overwriting, an unreadable prior
	// registration.
	gogRegUnknown
)

// gogRegSnapshot is the result of snapshotGogRegistration: state, plus (only
// when state == gogRegPresent) the exact argv that was registered.
type gogRegSnapshot struct {
	state gogRegState
	argv  []string
}

// snapshotGogRegistration takes a TRI-STATE, bounded snapshot of the gog MCP
// registration BEFORE gogSetup would overwrite it (R2-03). The prior
// registeredGogCommand-based rollback collapsed three genuinely different
// situations into a single (nil,false): confirmed absent, confirmed present
// but unparseable, and "the sbx probe itself failed" — all three read as
// "nothing to restore", so a registration that was merely unreadable (a
// quoted command, an unexpected shape) or hit a transient sbx hiccup could be
// silently clobbered by rollback's bare `sbx mcp rm gog`.
//
// This distinguishes presence FIRST, via the bounded, PLAIN `sbx mcp ls`
// listing (independent of whether the detailed argv parses — exactly the
// listing doctor's own mcpCheck/mcpProbeCheck already use via grepWord), so
// "present but unreadable" is never mistaken for "confirmed absent":
//   - the listing probe fails or times out               -> gogRegUnknown
//   - the listing succeeds and gog is NOT in it           -> gogRegAbsent
//   - the listing succeeds, gog IS in it, but the detailed
//     command can't be read/parsed (registeredGogCommand's
//     own `sbx mcp get gog` + `sbx mcp ls -o json` probes
//     both come up empty, quoted, or malformed)            -> gogRegUnknown
//   - the listing succeeds, gog IS in it, and the detailed
//     command parses cleanly                               -> gogRegPresent(argv)
func snapshotGogRegistration(env shellEnv) gogRegSnapshot {
	if env.lookPath == nil {
		return gogRegSnapshot{state: gogRegUnknown}
	}
	if _, err := env.lookPath("sbx"); err != nil {
		// Defensive only: gogSetup's own preflight already refuses to run past a
		// missing sbx (R1-08/R2-04), so a caller reaching here with sbx absent is
		// unexpected — report unknown rather than guessing absent.
		return gogRegSnapshot{state: gogRegUnknown}
	}
	listOut, timedOut, err := probeRun(env, "sbx", "mcp", "ls")
	if err != nil || timedOut {
		return gogRegSnapshot{state: gogRegUnknown}
	}
	if !grepWord(listOut, "gog") {
		return gogRegSnapshot{state: gogRegAbsent}
	}
	if argv, ok := registeredGogCommand(env); ok {
		return gogRegSnapshot{state: gogRegPresent, argv: argv}
	}
	return gogRegSnapshot{state: gogRegUnknown}
}
