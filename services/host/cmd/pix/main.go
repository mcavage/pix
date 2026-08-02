// pix — the user-facing launcher for the pix sandbox. Unlike the old
// repo-relative bin/pix shell script, this is a standalone binary a
// consumer installs without cloning the repo: it reads ~/.config/pix config
// and shells out to `sbx run pix`, pinning the git-hosted kit to this
// build's stamped version.
//
// Host convention is Go (one static binary; see services/host/main.go), and this
// launcher shares that binary's config package (pix/host/config) so the two
// agree on config location + the broker token.
//
// Verb tree:
//
//	pix [DIR]                     alias for `run [DIR]`
//	pix run [DIR] [flags] [-- …]  launch the sandbox (full)
//	pix version                   print the stamped version (full)
//	pix config show|path          show config path + contents (full)
//	pix serve [args…]             exec the sibling pix-host serve (full)
//	pix status|doctor|setup|mcp|memory|knowledge|pack   (all implemented)
//	pix reset                     (destructive, reversible: state moved aside)
//	pix help [verb]               print the verb tree (or one verb's usage)
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"pix/host/routing"
)

// version is stamped at build time via -ldflags "-X main.version=0.0.x". An
// unstamped build reports "dev" and tracks the kit's main branch.
var version = "dev"

func main() {
	args := os.Args[1:]

	// A global `--man` may appear anywhere on the command line (before a `--`
	// terminator): render the embedded man page and exit. It is DISTINCT from the
	// -h/--help contract — `--help` prints usage, `--man` opens the full page.
	if rest, ok := extractManFlag(args); ok {
		runMan(rest)
		return
	}

	if len(args) == 0 {
		// Bare `pix` shows STATUS — never launches a sandbox (launching is
		// explicit behind `run`). On a fresh host with no config, offer onboarding.
		if maybeFirstRun() {
			return
		}
		runStatusCmd(nil)
		return
	}

	switch args[0] {
	case "run":
		runVerb(args[1:])
	case "status", "st":
		runStatusCmd(args[1:])
	case "ls":
		runLs(args[1:])
	case "rm":
		runRm(args[1:])
	case "version", "--version", "-v":
		if len(args) > 1 {
			if args[1] == "-h" || args[1] == "--help" {
				fmt.Print(versionUsage)
				return
			}
			fmt.Fprintf(os.Stderr, "pix version: unexpected argument %q\n\n%s", args[1], versionUsage)
			os.Exit(2)
		}
		fmt.Println(version)
	case "config":
		runConfig(args[1:])
	case "serve":
		runServe(args[1:])
	case "doctor":
		runDoctorCmd(args[1:])
	case "setup":
		// Explicit guided onboarding: host phase, then hand off to the in-VM
		// agent. (`pix run` never onboards on its own; `pix setup
		// --no-agent` is the host-only, no-handoff path for CI — the `onboard`
		// verb it replaced is deleted, with no alias: an `onboard` argv takes
		// the standard unknown-verb path, which suggests `setup` and exits 2.)
		runSetupCmd(args[1:])
	case "gworkspace":
		runGworkspaceCmd(args[1:])
	case "slack":
		runSlackCmd(args[1:])
	case "mcp":
		runMcpCmd(args[1:])
	case "pack":
		runPackCmd(args[1:])
	case "secret":
		runSecretCmd(args[1:])
	case "memory", "mem":
		runMemory(args[1:])
	case "monitor":
		runMonitor(args[1:])
	case "backup":
		runBackup(args[1:])
	case "restore":
		runRestore(args[1:])
	case "knowledge", "kb":
		runKnowledge(args[1:])
	case "models":
		runModels(args[1:])
	case "route":
		// Deprecated alias, one release only (docs/design/models-cli.md,
		// Deprecation): stderr-only so --json/piped stdout is unaffected.
		// retiredVerbs["route"] = "models" (help.go) survives after this case is
		// deleted; that is the permanent recovery path.
		//
		// It forwards RAW to the host tree rather than through runModels, so the
		// alias is bug-for-bug the command it replaces. Routing it through the new
		// verb silently broke `pix route models` (the old spelling of the registry
		// list): runModels has no `models` subcommand, so a script piping
		// `pix route models --json` got usage prose on stdout and exit 2 — the
		// exact compatibility this alias exists to promise.
		fmt.Fprintln(os.Stderr, "pix route is now pix models (pix models route compiles the intent map).")
		runRouteAlias(args[1:])
	case "evals":
		// Catch this explicitly (also shadowing the bare-arg-is-a-dir behavior when
		// an evals/ dir is present) so a bare `evals` gets a clear message instead
		// of a confusing "no such directory".
		fmt.Fprintln(os.Stderr, "pix: evals were removed. Model scores are hand-maintained in")
		fmt.Fprintf(os.Stderr, "  %s; run `pix models route`\n", routing.ScorecardPath())
		fmt.Fprintln(os.Stderr, "  after editing.")
		os.Exit(2)
	case "agent":
		runAgent(args[1:])
	case "man":
		runMan(args[1:])
	case "reset":
		runReset(args[1:])
	case "upgrade":
		runUpgrade(args[1:])
	case "state":
		runState(args[1:])
	case "task":
		runTask(args[1:])
	case "host":
		// The unsandboxed escape hatch (expert tier, gated off by default): execs
		// the host-installed pi directly. See hostrun.go + docs/design/host-mode.md.
		runHost(args[1:])
	case "help", "-h", "--help":
		if len(args) > 1 {
			if args[1] == "--all" {
				fmt.Print(helpAllText)
				return
			}
			if u, ok := verbUsage(args[1]); ok {
				fmt.Print(u)
				return
			}
		}
		fmt.Print(helpText)
	default:
		// A bare positional is a run workspace ONLY when it names an existing
		// directory (e.g. `pix ~/dev/foo`). Otherwise classifyBareArg decides:
		// a path-like token is a missing/!dir workspace (no such directory), and a
		// bare word is a probable verb typo (with a did-you-mean hint).
		if a := args[0]; len(a) > 0 && a[0] != '-' {
			msg, launch := classifyBareArg(a)
			if launch {
				runVerb(args)
				return
			}
			fmt.Fprint(os.Stderr, msg)
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "pix: unknown flag %q\n\n", args[0])
		fmt.Print(helpText)
		os.Exit(2)
	}
}

// looksLikePath reports whether a non-flag token is meant as a filesystem path
// (so a failure to resolve it is a missing/!dir workspace, never a verb typo).
// A path either contains a separator or begins with a path-ish prefix.
func looksLikePath(a string) bool {
	return strings.ContainsRune(a, '/') ||
		strings.HasPrefix(a, ".") ||
		strings.HasPrefix(a, "~")
}

// classifyBareArg decides what a bare (non-flag) positional means when it is not
// a matched verb. It returns the stderr message to print and whether the arg
// should launch `run` (an existing directory). A path-like token, or a token
// that exists but is not a directory, is reported as a missing/!dir workspace
// ("no such directory"). Only a plausible bare-word verb gets the did-you-mean
// suggester. Both non-launch branches map to exit code 2 at the call site.
func classifyBareArg(a string) (msg string, launch bool) {
	fi, statErr := os.Stat(a)
	if statErr == nil && fi.IsDir() {
		return "", true
	}
	if looksLikePath(a) || statErr == nil {
		return fmt.Sprintf("pix: %q: no such directory\n", a), false
	}
	msg = fmt.Sprintf("pix: no command named %q.\n", a)
	if s, ok := suggestVerb(a); ok {
		msg += fmt.Sprintf("Did you mean %q?\n", s)
	}
	msg += "Run `pix help` to see all commands.\n"
	return msg, false
}

// runVerb handles the `run` verb and the bare-DIR alias. It short-circuits to
// run usage on a -h/--help request, then launches. It deliberately NEVER runs
// onboarding (owner constraint: `pix run` just gives the agent to the
// user); it auto-provisions a model key only if none exists (else it can't
// launch), and otherwise stays out of the way. The guided onboarding is the
// explicit `pix setup`, which does the host phase then launches a normal
// `run` handing the agent a short kickoff message to begin the walkthrough.
func runVerb(argv []string) {
	if wantsHelp(argv) {
		fmt.Print(runUsage)
		return
	}
	runRun(argv)
}

// runServe execs the sibling pix-host binary's `serve` subcommand, found
// next to this binary or on PATH, passing along any args.
func runServe(argv []string) {
	// A leading -h/--help prints serve usage instead of execing the host binary.
	if len(argv) > 0 && (argv[0] == "-h" || argv[0] == "--help") {
		fmt.Print(serveUsage)
		return
	}
	// `serve stop` / `serve status` are launcher-side control verbs (pidfile-based)
	// handled HERE — they are NOT passed through to `pix-host serve`.
	if len(argv) > 0 {
		switch argv[0] {
		case "stop":
			runServeStop(argv[1:])
			return
		case "status":
			runServeStatus(argv[1:])
			return
		case "start", "install":
			// `start` is an alias for `install`: it registers + (re)starts the
			// managed service, picking up a freshly-rebuilt binary — the natural
			// partner to `serve stop`.
			runServeInstall(argv[1:])
			return
		case "uninstall":
			runServeUninstall(argv[1:])
			return
		}
	}
	bin, err := findHostBinary()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix serve: %v\n", err)
		os.Exit(1)
	}
	cmd := exec.Command(bin, append([]string{"serve"}, argv...)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			os.Exit(exit.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "pix serve: exec %s: %v\n", bin, err)
		os.Exit(1)
	}
}

// hostBinaryResolver locates pix-host. It is indirected through a package
// var (like firstRunHook) so tests can inject a fake `pix-host mcp --list`
// responder when exercising the local-vs-remote MCP partition in setup.
var hostBinaryResolver = findHostBinary

// findHostBinary locates pix-host next to argv[0] first (the common install
// layout), then falls back to PATH. A located binary is usable only when its
// stamped version exactly matches the launcher, preventing silent mixed-release
// RPC and flag behavior.
func findHostBinary() (string, error) {
	verify := func(path string) (string, error) {
		out, err := exec.Command(path, "version").CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("pix-host at %s cannot report its version: %v", path, err)
		}
		hostVersion := strings.TrimSpace(string(out))
		if hostVersion != version {
			return "", fmt.Errorf("pix-host version %q at %s does not match pix version %q; reinstall both binaries together", hostVersion, path, version)
		}
		return path, nil
	}
	if self, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(self), "pix-host")
		if fi, err := os.Stat(sibling); err == nil && !fi.IsDir() {
			return verify(sibling)
		}
	}
	if p, err := exec.LookPath("pix-host"); err == nil {
		return verify(p)
	}
	return "", fmt.Errorf("pix-host not found next to this binary or on PATH")
}

const runUsage = `usage: pix run [DIR] [flags] [-- pi-args...]

flags:
  --dev            Mode B: use the local checkout kit + load skills live from it
                   (needs a checkout; resolves $PIX_DEV_ROOT, the cwd, or
                   the launcher's own location)
  --skills DIR     mount an extra skill tree and load it live (repeatable)
  --kit K          override the KIT — the whole sandbox spec (image + entrypoint +
                   creds + egress + skills): replaces the auto git/local pin, so you
                   can work around an unresolvable release tag (repeatable; path or
                   git+URL). To just swap the IMAGE, use --template instead.
  --kit-ref REF    pin the auto kit to a specific git ref (e.g. v0.1.0, main)
                   instead of the latest stable release. Steers the automatic
                   pin rather than replacing it, so the image still comes from
                   that ref's kit. Persist it with version_pin in config.toml.
  --template REF   override only the IMAGE sbx boots (the ref 'make load' prints,
                   e.g. docker.io/mcavage/pix:local-1234567890). Works from ANY
                   directory — no checkout needed — so you can point at one worktree's
                   build while sitting in another. Orthogonal to --kit.
  --mcp M          attach an MCP server at creation (repeatable)
  --pack P         active pack for this run (path or git-url); mounts its skills +
                   knowledge, overriding the configured active pack
  --name N         sandbox name
  --model M        active pi model (passed through to pi)
  --intent NAME    resolve the session model via the router (cost/latency/accuracy);
                   --model overrides it. Intents: pix models show
  --replace        recreate the sandbox (sbx rm -f, then create) instead of
                   re-attaching to an existing one; picks up changed --kit/--mcp/
                   create-only flags

lifecycle (matches sbx's own re-attach model):
  no sandbox named N          -> create it (the flags above apply).
  a sandbox named N exists    -> RE-ATTACH to it as-is (running or stopped);
                                 sbx reads the agent from its own spec, so
                                 --kit/--mcp/--template, --dev, and the
                                 create-only skill flags are NOT re-sent (--dev
                                 is create/replace-only and is ignored, with a
                                 note, on a plain re-attach). Use --replace to
                                 recreate with the current flags instead.
                                 --model/--intent are NOT create-only: they are
                                 pi runtime args, so they still reach the pi
                                 session on a re-attach too.

released vs local:
  A RELEASED launcher (clean version like 0.0.16) tracks the LATEST STABLE
  release: it resolves the newest published tag (cached 24h) and pins that, so a
  launcher you installed months ago still boots today's kit + image. The tag is
  always concrete, never a moving ':latest'. If the lookup cannot run — offline,
  GitHub unreachable — it silently falls back to this build's own version, which
  is the old lockstep behaviour; a run is never blocked or failed by it.
  Precedence: --kit-ref, then version_pin in config.toml, then latest stable,
  then this build's version.

  An UNRELEASED/local build (version with +local, a dev build, or non-semver)
  never pins a nonexistent v<version> tag and never auto-tracks a release: it
  uses your local checkout kit when one is resolvable (also pinning the locally
  loaded image via --template), otherwise it falls back to #ref=main with a
  warning.

DIR defaults to the current directory. Everything after -- is passed to pi.
Set PIX_DEBUG=1 to print the composed sbx command.
`

const helpText = `pix — a personal, multi-model pi coding agent in a Docker sandbox.

Usage:  pix <command> [args]

New here?   pix setup      one-time guided setup (a few minutes, resumable)

Workflow
  run [DIR]        launch the sandbox in DIR (default: .). This is the main one.
  ls               list your pix sandboxes;  rm <name>  removes one
  serve            start the host services (memory, knowledge); ` + "`serve stop|status`" + `
  status           what is up, what is down, what is next   (also the bare command)

Setup & health
  setup            guided setup: keys, memory, pack (knowledge/integrations optional)
  doctor           diagnose problems and print the exact fix commands
  upgrade          update the pix binaries (` + "`--check`" + ` just reports; run tracks the
                   latest kit/image on its own)
  monitor [name]   live-follow a sandbox's out-of-sandbox traffic (:11437)

Data
  memory           recall | remember | forget | learnings | stats
  knowledge        init | use | ls | query | sync | remote

Models & agents
  models           which models pix can use, and which are wired up
  agent            manage subagents: ls | new | edit | rm | reassess

More
  config, mcp, state, version, man     (see ` + "`pix help --all`" + `)

Learn a command:  pix help run     ·     pix <command> -h
`
