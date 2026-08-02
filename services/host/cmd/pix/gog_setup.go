// gog_setup.go implements `pix gworkspace setup` (this file's historical name
// predates the gworkspace rename) — the guided, PUBLIC path to wiring up
// Google Workspace via the `google-workspace` host MCP server. The `gog`
// binary is a LOCAL
// stdio MCP server the sbx gateway spawns on the HOST, so sbx's native
// `sbx mcp auth` (hosted-control-plane OAuth for REMOTE catalog servers) does
// NOT apply to its Google grant: the grant runs through the installed gog CLI
// itself, right here.
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
// pix does not independently inspect the OAuth scopes actually granted
// (gog exposes no stable, parseable scope-inspection surface to check
// against) — the guarantee is that the grant REQUESTS read-only scopes, and
// the runtime backstop (`--gmail-no-send --wrap-untrusted --readonly
// --allow-tool read`, set at MCP-serve time in mcp.go's mcp.GogHardenedArgv)
// blocks writes regardless.
//
// `pix gworkspace setup` requires sbx: a missing sbx binary, or a failed `sbx
// mcp add`, is a hard failure — never a silent "would register" success. Its
// config write is also ordered to prevent drift: the account/MCP change is
// built in memory, actual sbx registration must succeed FIRST, and only then
// is config.toml saved; a registration failure never touches the persisted
// config, and a save failure AFTER a successful registration rolls the sbx
// side back (restoring whatever was registered before, or removing the new
// registration) rather than leaving config and the gateway to drift apart.
//
// Every PREDICTABLE hard requirement — config loads cleanly, the gog CLI and
// its selected auth route are capable, the credentials path is a true regular
// file, sbx is on PATH, and whatever gog registration already exists can be
// confirmed — is checked BEFORE the first OAuth side effect (the interactive
// auth route below). The prior registration check in particular is TRI-STATE
// (gogRegSnapshot): confirmed absent, confirmed present with a restorable
// argv, or unknown (the bounded `sbx mcp ls` listing itself failed, or gog is
// listed but its registered command can't be parsed/read). Unknown is never
// treated as absent — this command refuses to authorize or overwrite the
// registration until it can be read, so an unreadable or momentarily-
// unlistable prior registration is never silently clobbered by a same-run
// rollback.
package main

import (
	"bufio"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/mcp"
	"pix/host/secret"
	"pix/host/workflow/onboard"
)

// The `gog` verb layer that used to live here (gogUsage, gogSetupUsage,
// runGogCmd, parseGogSetupArgs, runGogSetupCmd) is DELETED, not renamed: the
// public surface is `pix gworkspace setup|status|disable` (gworkspace.go)
// and `pix setup --google-workspace`, which are façades over this file's
// unchanged transaction. There is exactly one writer.

// gogSetupOpts is the parsed Google Workspace setup flag set, shared by both
// public doors (`gworkspace setup` and `setup --google-workspace`).
type gogSetupOpts struct {
	account     string
	credentials string
	assumeYes   bool
	access      string
}

// gogAuthStep is one command in a gogAuthRoute. help is the argv (after
// "gog") whose OWN --help output must be probed (the SUBCOMMAND's help, not
// just the top-level `gog auth --help` names) before the step is ever
// executed; requiredFlags are the exact flag tokens that help text must
// advertise for the step to be trusted. Every step that performs an actual
// OAuth grant (as opposed to merely importing a client) carries "--readonly"
// in requiredFlags: pix never authorizes an account without requesting
// read-only scopes at grant time. argv builds the actual command (after
// "gog") to run.
type gogAuthStep struct {
	help          []string
	requiredFlags []string
	argv          func(account, credentials string) []string
}

// gogAuthRoute is one supported way to import an OAuth client + authorize an
// account with the installed gog CLI, as an ordered sequence of steps run via
// env.RunInteractive.
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
// two-step `credentials`+`add`, then the OLDER `add-client`+`login`. ok is
// false when none of those surfaces are advertised — callers must not blindly
// exec an obsolete command in that case.
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

const gogWorkspaceServices = "gmail,calendar,drive,docs,sheets,contacts"

// chooseCreateDocsAuthRoute requires gog's scoped auth-add surface. The older
// one-shot and legacy login routes cannot express Gmail-read-only + Drive-file
// scope independently, so Pix refuses them for this opt-in profile.
func chooseCreateDocsAuthRoute(help string) (gogAuthRoute, bool) {
	subs := gogAuthSubcommands(help)
	if !subs["credentials"] || !subs["add"] {
		return gogAuthRoute{}, false
	}
	return gogAuthRoute{name: "credentials+add", steps: []gogAuthStep{
		{
			help: []string{"auth", "credentials", "--help"},
			argv: func(_, credentials string) []string {
				return []string{"auth", "credentials", credentials}
			},
		},
		{
			help:          []string{"auth", "add", "--help"},
			requiredFlags: []string{"--services", "--drive-scope", "--gmail-scope", "--force-consent"},
			argv: func(account, _ string) []string {
				return []string{"auth", "add", account,
					"--services", gogWorkspaceServices,
					"--drive-scope", "file",
					"--gmail-scope", "readonly",
					"--force-consent"}
			},
		},
	}}, true
}

// gogAuthRouteCapable is the capability probe: it checks EVERY step's OWN
// subcommand help (not just the top-level `gog auth --help` listing
// chooseGogAuthRoute already used to pick a candidate by name) and confirms
// every one of that step's requiredFlags literally appears in it. This is
// what enforces read-only OAuth authorization at grant time — a step that
// performs an OAuth grant always requires "--readonly", so a route whose
// installed CLI cannot advertise that flag is never run. ok is false on the
// FIRST unsupported step, with a detail identifying exactly what is missing;
// gogSetup fails outright on !ok rather than silently trying an older,
// possibly-unsafe route. Every help probe is BOUNDED (probeRun).
func gogAuthRouteCapable(env hostenv.Env, route gogAuthRoute) (bool, string) {
	for _, step := range route.steps {
		if len(step.requiredFlags) == 0 {
			continue
		}
		args := append([]string{"gog"}, step.help...)
		helpOut, _, _ := env.RunTimed(args[0], args[1:]...)
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
// healthy: interactive auth (onboard.GogAuthed) passes, and — only when the headless
// path can actually be probed (op installed + an op-refs.env resolves) —
// headless tools are also non-empty. When headless can't be probed at all it
// is NOT counted against health here (macOS system keychain setups skip that
// step entirely); `pix gworkspace setup`/doctor still surface that gap on
// their own. The seam a future `pix setup --account` follow-up (S08) uses to
// decide whether to print the `pix gworkspace setup` hint — it never runs the
// OAuth flow itself.
func gogSetupAccountHealthy(env hostenv.Env, acct string) bool {
	if !onboard.GogAuthed(env, acct) {
		return false
	}
	opRefs := secret.FindOpRefs(env)
	if _, err := env.LookPath("op"); err != nil || opRefs == "" {
		return true // headless unverifiable here; not a confirmed unhealthy state
	}
	return gogHeadlessOK(env, acct, opRefs)
}

// gogSetup is the hermetically-testable core of `pix gworkspace setup`. All OS
// contact (lookPath/run/statFile/runInteractive) goes through env; account/
// credentials prompting is gated on tty (never on a bare non-TTY run, never
// when --yes was given).
func gogSetup(env hostenv.Env, opts gogSetupOpts, in io.Reader, out io.Writer, tty bool) error {
	// A single shared bufio.Reader across BOTH prompts: promptLine's own
	// bufio.NewReader-per-call would silently drop the second answer here (its
	// first read can buffer past the first line on a fully-buffered io.Reader,
	// e.g. in tests, though a real interactive terminal delivers one line per
	// Read and never shows the bug).
	reader := bufio.NewReader(in)
	prompt := func(msg string) string {
		fmt.Fprint(out, msg)
		line, _ := reader.ReadString('\n')
		return strings.TrimSpace(line)
	}

	account := strings.TrimSpace(opts.account)
	createDocs := strings.TrimSpace(opts.access) == gwAccessCreateDocs
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
	// Interactive prompts do not pass through a shell, so a leading ~/ would
	// otherwise be statted literally. Expand only the current user's home form;
	// do not perform environment-variable, command, glob, or ~other expansion.
	if credentials == "~" || strings.HasPrefix(credentials, "~/") {
		if strings.TrimSpace(env.HomeDir()) == "" {
			return fmt.Errorf("cannot expand credentials path %q: user home directory is unavailable", credentials)
		}
		home := strings.TrimSpace(env.HomeDir())
		if credentials == "~" {
			credentials = home
		} else {
			credentials = filepath.Join(home, strings.TrimPrefix(credentials, "~/"))
		}
	}

	// gogPath is captured HERE and never re-resolved: it feeds the immutable
	// registrar snapshot built below (mcp.BuildGogRegistrar), the single source for
	// both the headless probe and the eventual registration.
	gogPath, gogErr := env.LookPath("gog")
	if gogErr != nil {
		fmt.Fprintln(out, "the Google Workspace dependency CLI is not installed.")
		fmt.Fprintln(out, "  install it:  "+config.GWInstallCmd)
		return fmt.Errorf("the Google Workspace dependency CLI is not installed (%s)", config.GWInstallCmd)
	} // Validate the credentials path BEFORE ever handing it to gog: must be a
	// TRUE regular file — Mode().IsRegular(), not merely "exists and isn't a
	// directory" (which a FIFO, socket, or device would also satisfy).

	credMode, credOK := env.Mode(credentials)
	if !credOK || !credMode.IsRegular() {
		return fmt.Errorf("credentials file not found (must be a regular file): %s", credentials)
	}

	snapshotPath, cleanup, err := snapshotGogCredentials(credentials)
	if err != nil {
		return fmt.Errorf("snapshotting credentials: %w", err)
	}
	defer cleanup()
	credentials = snapshotPath // use the snapshot path for all gog steps

	// Every noninteractive gog probe here (help/version/auth check) runs
	// through the BOUNDED probe machinery (timeout + output cap), so a hung gog
	// can never wedge setup. Tests without env.probe fall back to env.Run.
	helpOut, _, _ := env.RunTimed("gog", "auth", "--help")
	var route gogAuthRoute
	var ok bool
	if createDocs {
		route, ok = chooseCreateDocsAuthRoute(helpOut)
	} else {
		route, ok = chooseGogAuthRoute(helpOut)
	}
	if !ok {
		verOut, _, _ := env.RunTimed("gog", "--version")
		fmt.Fprintln(out, "the installed Google Workspace dependency CLI does not advertise a supported auth surface")
		fmt.Fprintln(out, "  (looked for: auth setup | auth credentials+add | auth add-client+login)")
		fmt.Fprintf(out, "  installed version: %s\n", strings.TrimSpace(verOut))
		fmt.Fprintln(out, "  upgrade: "+gwUpgradeCmd)
		fmt.Fprintln(out, "  then re-run: pix gworkspace setup")
		return fmt.Errorf("no supported auth route for the installed gog version")
	}

	// Probe the SELECTED route's own subcommand help (not just the top-level
	// `gog auth --help` names already used above) for the exact flags each step
	// needs, including "--readonly" on every step that performs an OAuth grant.
	// If the installed CLI cannot guarantee a read-only grant here, fail with
	// upgrade/manual guidance rather than falling back to an older, possibly-
	// unsafe route — pix never authorizes without requesting read-only
	// scopes at grant time.
	if capable, detail := gogAuthRouteCapable(env, route); !capable {
		verOut, _, _ := env.RunTimed("gog", "--version")
		profile := "read-only"
		if createDocs {
			profile = "the create-new-Docs permission profile"
		}
		fmt.Fprintf(out, "the installed Google Workspace dependency CLI cannot guarantee %s OAuth authorization for the %q route:\n", profile, route.name)
		fmt.Fprintf(out, "  %s\n", detail)
		fmt.Fprintf(out, "  installed version: %s\n", strings.TrimSpace(verOut))
		fmt.Fprintln(out, "  upgrade: "+gwUpgradeCmd)
		fmt.Fprintln(out, "  then re-run: pix gworkspace setup")
		fmt.Fprintln(out, "  pix never falls back to an auth route whose requested permissions it cannot confirm.")
		return fmt.Errorf("the installed Google Workspace dependency CLI cannot guarantee %s OAuth authorization (route: %s): %s", profile, route.name, detail)
	}

	// Preflight every remaining PREDICTABLE hard requirement here, BEFORE the
	// first OAuth side effect (the interactive route steps just below) ever
	// runs — sbx must be on PATH, config must load cleanly, and the prior gog
	// registration snapshot must be CONFIRMED (never unknown). Any failure here
	// returns before a single runInteractive call and before config is touched,
	// so a botched preflight can never leave OAuth half-run or config mutated.
	if _, err := env.LookPath("sbx"); err != nil {
		return fmt.Errorf("sbx not found: pix gworkspace setup requires sbx to register the MCP server " +
			"(install: https://docs.docker.com/ai/sandboxes); install it, then re-run pix gworkspace setup")
	}
	var hostPath string
	if createDocs {
		if env.HostBinary != nil {
			hostPath, err = env.HostBinary()
		} else {
			hostPath, err = env.LookPath("pix-host")
		}
		if err != nil || strings.TrimSpace(hostPath) == "" {
			return fmt.Errorf("pix-host not found: required for the create-new-Docs capability; reinstall Pix, then re-run pix gworkspace setup")
		}
	}
	// Load the candidate config change IN MEMORY only — cfg.Save() must never
	// run before the sbx registration it describes actually succeeds, or a
	// registration failure would leave a persisted config claiming gog is
	// registered when it is not (config/registration drift).
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	// A TRI-STATE, bounded snapshot of whatever gog registration already exists
	// BEFORE this run would overwrite it. gogRegUnknown (the bounded `sbx mcp
	// ls` listing itself failed/timed out, OR gog is confirmed listed but its
	// registered command can't be parsed/read) is NEVER treated as "nothing to
	// restore" — this command refuses to authorize or overwrite until the prior
	// registration is actually readable, so it can never be silently lost to a
	// same-run rollback.
	snap := snapshotGogRegistration(env)
	if snap.state == gogRegUnknown {
		return fmt.Errorf("could not confirm the prior " + config.GWServerName + " registration (sbx mcp ls/get did not resolve cleanly): " +
			"refusing to authorize or overwrite it until this is readable; check the sbx daemon (sbx mcp status), " +
			"then re-run pix gworkspace setup")
	}
	docsSnap := gogRegSnapshot{state: gogRegAbsent}
	if createDocs {
		docsSnap = snapshotMCPRegistration(env, config.GWDocsCreateServerName)
		if docsSnap.state == gogRegUnknown {
			return fmt.Errorf("could not confirm the prior %s registration: check the sbx daemon (sbx mcp status), then re-run pix gworkspace setup", config.GWDocsCreateServerName)
		}
	}

	runInteractive := env.RunInteractive

	profileReady := !createDocs || cfg.GoogleWorkspaceAccess == gwAccessCreateDocs
	if onboard.GogAuthed(env, account) && profileReady {
		fmt.Fprintf(out, "Existing Google authorization found for %s; reusing it.\n", account)
	} else {
		if createDocs {
			fmt.Fprintf(out, "Authorizing %s for Workspace reads and create-new-Docs (mail sending remains blocked)...\n", account)
		} else {
			fmt.Fprintf(out, "Importing your OAuth client + authorizing %s (gog auth route: %s, read-only scopes requested)...\n", account, route.name)
		}
		fmt.Fprintln(out, "This may open a browser for you to sign in.")
		for _, step := range route.steps {
			argv := step.argv(account, credentials)
			fmt.Fprintf(out, "  running: gog %s\n", strings.Join(argv, " "))
			if err := runInteractive("gog", argv...); err != nil {
				return fmt.Errorf("gog %s: %w", strings.Join(argv, " "), err)
			}
		}
	}

	if _, timedOut, err := env.RunTimed("gog", "--account", account, "auth", "doctor", "--check"); timedOut || err != nil {
		fmt.Fprintln(out, "authorization did not verify:")
		fmt.Fprintf(out, "  gog --account %s auth doctor --check\n", account)
		if timedOut {
			return fmt.Errorf("gog auth doctor --check timed out for %s", account)
		}
		return fmt.Errorf("gog auth doctor --check failed for %s: %w", account, err)
	}
	fmt.Fprintf(out, "interactive auth OK for %s\n", account)

	// TOCTOU guard: resolve the registrar snapshot EXACTLY ONCE, right here —
	// op, op-refs, and gogPath are captured into ONE immutable mcp.McpRegistrar and
	// never re-resolved after this point. The exact same reg feeds BOTH the
	// headless probe below (reg.ExecArgv — byte-for-byte the hardened argv,
	// including --gmail-no-send --wrap-untrusted --readonly --allow-tool read
	// and any op wrapper) and the eventual registration (mcp.RegisterGogRegistrar
	// -> reg.AddArgs), so a concurrent PATH or op-refs.env mutation between
	// probe and registration can never cause a DIFFERENT command to be
	// registered than the one that was just proven healthy.
	//
	// Verify headless tools the SAME host-side path doctor uses — and the SAME
	// exact argv/flags/op-wrapper that registration is about to register. This
	// is the documented trap: interactive auth in a logged-in shell proves
	// nothing about the bare env the sbx gateway spawns gog in. When op/op-refs
	// are unavailable this MUST NOT skip verification — reg falls back to the
	// bare hardened invocation (minus the op wrapper), bounded by the same
	// probeListTools machinery. A clean zero-tools result still fails, and an
	// exec/timeout result is unverifiable and must never be reported as
	// success. Nothing here mutates config/registration yet — that only happens
	// after `head` is confirmed healthy, below.
	reg := mcp.BuildGogRegistrar(gogPath, account, mcpCredentials(env))
	if createDocs {
		reg.HostBin = hostPath
	}
	head := probeListTools(env, reg.ExecArgv(config.GWServerName))
	switch head.status {
	case probeToolsOK:
		fmt.Fprintln(out, "headless tools OK (verified the same host-side path the sbx gateway/doctor use)")
	case probeNoTools:
		fmt.Fprintln(out, "interactive auth is healthy, but the headless path returns 0 tools.")
		fmt.Fprintln(out, "  this is the documented trap: the gateway spawns gog in a bare, non-interactive")
		fmt.Fprintln(out, "  env and can't unlock the keyring without help.")
		fmt.Fprintf(out, "  add GOG_KEYRING_BACKEND=file + GOG_KEYRING_PASSWORD + GOG_ACCOUNT + GOG_HOME to %s\n", secret.DefaultOpRefsPath(env))
		return fmt.Errorf("authorization succeeded but the headless tool listing returned 0 tools; nothing was registered (account %s)", account)
	default: // probeTimedOut, probeError, probeDeniedByPolicy — never claimed as success
		fmt.Fprintf(out, "headless verification could not be confirmed (%s): not registering until this is fixed\n", head.detail)
		return fmt.Errorf("headless verification unverifiable for %s (%s): not registering until this is fixed", account, head.detail)
	}

	// sbx presence, config.Load(), and the prior registration snapshot were ALL
	// already confirmed above, before the interactive auth route ran — nothing
	// left to preflight here, just apply the change.
	cfg.SetGogAccount(account)
	if createDocs {
		cfg.SetGoogleWorkspaceAccess(gwAccessCreateDocs)
	}
	// Idempotency: ALWAYS ensure gog is in the configured MCP set, even when
	// the account already matched (a healthy re-run must still leave gog
	// attached/registered deterministically).
	cfg.AddMCP(config.GWServerName)
	if createDocs {
		cfg.AddMCP(config.GWDocsCreateServerName)
	}

	// REGISTER FIRST, save second. mcp.RegisterGogRegistrar runs `sbx mcp add gog
	// ...` using the EXACT reg snapshot resolved above (no re-resolution — the
	// TOCTOU guard); only once that has genuinely succeeded do we persist
	// gog_account/mcp to disk. A registration failure here returns before
	// cfg.Save() is ever called, so the persisted config is left byte-for-byte
	// unchanged — there is no config/registration drift to roll back from.
	if err := mcp.RegisterGogRegistrar(reg, env, out); err != nil {
		return fmt.Errorf("registering %s with the sbx gateway: %w (finish later: pix mcp register %s)", config.GWServerName, err, config.GWServerName)
	}
	if createDocs {
		if err := mcp.RegisterDocsCreateRegistrar(reg, env, out); err != nil {
			docsRollbackErr := restoreMCPRegistration(env, config.GWDocsCreateServerName, docsSnap)
			gogRollbackErr := gogSetupRollbackRegistration(env, snap)
			if docsRollbackErr != nil || gogRollbackErr != nil {
				return fmt.Errorf("registering create-new-Docs with the sbx gateway: %w; rollback failed (ws: %v; docs-create: %v)", err, gogRollbackErr, docsRollbackErr)
			}
			return fmt.Errorf("registering create-new-Docs with the sbx gateway: %w", err)
		}
	}

	if err := cfg.Save(); err != nil {
		// The sbx side is now AHEAD of the (about-to-remain) persisted config:
		// registration just succeeded but we couldn't record it. Roll the sbx
		// side back so the two never drift apart — restore whatever was
		// registered before this run (if anything), else remove the just-added
		// registration entirely. Any rollback failure is folded into the
		// returned error explicitly rather than swallowed.
		var docsErr error
		if createDocs {
			docsErr = restoreMCPRegistration(env, config.GWDocsCreateServerName, docsSnap)
		}
		gogErr := gogSetupRollbackRegistration(env, snap)
		if docsErr != nil || gogErr != nil {
			return fmt.Errorf("saving config: %w; additionally, rollback failed (ws: %v; docs-create: %v)", err, gogErr, docsErr)
		}
		return fmt.Errorf("saving config: %w (gog registration rolled back so config and the gateway stay in sync)", err)
	}

	fmt.Fprintln(out, "")
	fmt.Fprintln(out, config.GWServerName+" is in the configured MCP set: a fresh sandbox creation preloads it (tools in context from the start).")
	fmt.Fprintln(out, "Existing sandbox? attach it live: pix mcp load "+config.GWServerName)
	return nil
}

// gogSetupRollbackRegistration undoes a just-succeeded `sbx mcp add gog ...`
// when the config.Save() that was supposed to follow it fails, so the sbx
// gateway and pix's persisted config never drift apart:
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
func gogSetupRollbackRegistration(env hostenv.Env, snap gogRegSnapshot) error {

	if snap.state == gogRegPresent {
		args := mcp.RawAddArgs(config.GWServerName, snap.argv)
		if _, err := env.Run("sbx", args...); err != nil {
			return fmt.Errorf("could not restore the prior gog registration: %w", err)
		}
		return nil
	}
	if _, err := env.Run("sbx", "mcp", "rm", config.GWServerName); err != nil {
		return fmt.Errorf("could not remove the new gog registration: %w", err)
	}
	return nil
}

// snapshotMCPRegistration is the generic companion to snapshotGogRegistration
// for Pix-owned host MCP servers. It preserves an existing definition so the
// two-server Workspace transaction can roll back without deleting prior state.
func snapshotMCPRegistration(env hostenv.Env, name string) gogRegSnapshot {
	listOut, timedOut, err := env.RunTimed("sbx", "mcp", "ls")
	if err != nil || timedOut {
		return gogRegSnapshot{state: gogRegUnknown}
	}
	if !cli.GrepWord(listOut, name) {
		return gogRegSnapshot{state: gogRegAbsent}
	}
	if argv, ok := mcp.RegisteredCommand(env, name); ok {
		return gogRegSnapshot{state: gogRegPresent, argv: argv}
	}
	return gogRegSnapshot{state: gogRegUnknown}
}

func restoreMCPRegistration(env hostenv.Env, name string, snap gogRegSnapshot) error {

	if snap.state == gogRegPresent {
		if _, err := env.Run("sbx", mcp.RawAddArgs(name, snap.argv)...); err != nil {
			return fmt.Errorf("restore %s: %w", name, err)
		}
		return nil
	}
	if _, err := env.Run("sbx", "mcp", "rm", name); err != nil {
		return fmt.Errorf("remove %s: %w", name, err)
	}
	return nil
}

// gogRegState is the TRI-STATE result of snapshotting whatever gog
// registration exists before gogSetup would overwrite it.
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
// registration BEFORE gogSetup would overwrite it. A binary (nil,false)
// answer would collapse three genuinely different situations: confirmed
// absent, confirmed present but unparseable, and "the sbx probe itself
// failed" — the latter two must never read as "nothing to restore", or a
// registration that was merely unreadable (a quoted command, an unexpected
// shape) or hit a transient sbx hiccup could be silently clobbered by
// rollback's bare `sbx mcp rm gog`.
//
// This distinguishes presence FIRST, via the bounded, PLAIN `sbx mcp ls`
// listing (independent of whether the detailed argv parses — exactly the
// listing doctor's own mcpCheck already uses via grepWord), so "present but
// unreadable" is never mistaken for "confirmed absent":
//   - the listing probe fails or times out               -> gogRegUnknown
//   - the listing succeeds and gog is NOT in it           -> gogRegAbsent
//   - the listing succeeds, gog IS in it, but the detailed
//     command can't be read/parsed (registeredGogCommand's
//     own `sbx mcp inspect google-workspace` + `sbx mcp ls -o json` probes
//     both come up empty, quoted, or malformed)            -> gogRegUnknown
//   - the listing succeeds, gog IS in it, and the detailed
//     command parses cleanly                               -> gogRegPresent(argv)
func snapshotGogRegistration(env hostenv.Env) gogRegSnapshot {

	if _, err := env.LookPath("sbx"); err != nil {
		// Defensive only: gogSetup's own preflight already refuses to run past a
		// missing sbx, so a caller reaching here with sbx absent is unexpected —
		// report unknown rather than guessing absent.
		return gogRegSnapshot{state: gogRegUnknown}
	}
	listOut, timedOut, err := env.RunTimed("sbx", "mcp", "ls")
	if err != nil || timedOut {
		return gogRegSnapshot{state: gogRegUnknown}
	}
	if !cli.GrepWord(listOut, config.GWServerName) {
		return gogRegSnapshot{state: gogRegAbsent}
	}
	if argv, ok := registeredGogCommand(env); ok {
		return gogRegSnapshot{state: gogRegPresent, argv: argv}
	}
	return gogRegSnapshot{state: gogRegUnknown}
}
