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
// The verb tree is root.go's rootCmd — the one parser, the one dispatcher, and
// (via `pix help --all`) the one listing. main owns only what comes BEFORE a
// parse: the retired table, the bare-`pix` status screen, and the exit code.
package main

import (
	"fmt"
	"os"
	"strings"

	"pix/host/cli"
	"pix/host/launcher"
	"pix/host/workflow/provision"
)

// version is stamped at build time via -ldflags "-X main.version=0.0.x". An
// unstamped build reports "dev" and tracks the kit's main branch.
//
// It must stay a plain string with a CONSTANT initializer. `-X` silently does
// nothing to a variable initialised from a non-constant expression, so writing
// `var version = launcher.Version` here would leave every release reporting
// "dev" with no error anywhere — and the Makefile stamps this same symbol in
// pix-host, which is a different package, so the stamp cannot simply move.
// Instead main OWNS the stamp and pushes it down.
var version = "dev"

func init() { launcher.Version = version }

func main() {
	args := os.Args[1:]

	// The global `--man` flag is retired along with the `man` verb: the embedded
	// page was a third rendering of the verb table, and `pix help --all` is the
	// one that stays. Checked first, before any dispatch, so `pix run --man`
	// answers with the notice instead of launching.
	if hasGlobalManFlag(args) {
		retiredExit(retiredKey("pix", "--man"))
	}

	if len(args) == 0 {
		// Bare `pix` shows STATUS — never launches a sandbox (launching is
		// explicit behind `run`). On a fresh host with no config, offer onboarding.
		if provision.MaybeFirstRun() {
			return
		}
		runStatusCmd(nil)
		return
	}

	// A retired surface answers before anything else can happen: no config read,
	// no probe, no side effect. Both granularities are checked here, because a
	// retired SUBCOMMAND (`task gc`, `state backup`) must answer before its
	// group's parser rejects the name it no longer knows (see retired.go).
	retiredIfRetired(args[0], "")
	if len(args) > 1 {
		retiredIfRetired(args[0], args[1])
	}

	// Everything else is the root's: one parser, one dispatch, one exit map.
	if code := dispatch(args, newRootDeps()); code != 0 {
		os.Exit(code)
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
	if cli.WantsHelp(argv) {
		fmt.Print(runUsage)
		return
	}
	runRun(argv)
}

// hostBinaryResolver locates pix-host. It is indirected through a package
// var (like firstRunHook) so tests can inject a fake `pix-host mcp --list`
// responder when exercising the local-vs-remote MCP partition in setup.
var hostBinaryResolver = launcher.FindHostBinary

// "which pix-host am I paired with" is an identity question, so the answer
// lives in the launcher package; only the test indirection stays here.

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
  setup            guided setup: keys, memory, pack (integrations optional)
  doctor           diagnose problems and print the exact fix commands
  monitor [name]   live-follow a sandbox's out-of-sandbox traffic (:11437)

Data & models
  memory           recall | remember | forget | learnings | stats
  models           which models pix can use, and which are wired up
  agent            manage subagents: ls | new | edit | rm | reassess

More             config, mcp, task, state, version   (see ` + "`pix help --all`" + `)

Learn a command:  pix help run     ·     pix <command> -h
`
