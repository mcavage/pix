// pix — the user-facing launcher for the pix sandbox: a standalone binary a
// consumer installs without cloning the repo. It reads ~/.config/pix config and
// shells out to `sbx run pix`, pinning the git-hosted kit to this build's stamped
// version, and shares pix-host's config package so the two agree on config
// location.
//
// The verb tree is root.go's rootCmd — the one parser, the one dispatcher, and (via
// `pix help --all`) the one listing. main owns only what comes BEFORE a parse: the
// retired table, the bare-`pix` status screen, and the exit code.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"pix/host/launcher"
	"pix/host/workflow/provision"
)

// version is stamped at build time via -ldflags "-X main.version=0.0.x"; an
// unstamped build reports "dev" and tracks the kit's main branch.
//
// It MUST stay a plain string with a CONSTANT initializer: `-X` silently does
// nothing to a variable initialised from an expression, so `var version =
// launcher.Version` would leave every release reporting "dev" with no error
// anywhere. main owns the stamp and pushes it down.
var version = "dev"

func init() { launcher.Version = version }

func main() {
	args := os.Args[1:]

	// The retired global `--man` is checked before any dispatch, so `pix run
	// --man` answers with the notice instead of launching.
	if hasGlobalManFlag(args) {
		retiredExit(retiredKey("pix", "--man"))
	}

	if len(args) == 0 {
		// Bare `pix` shows STATUS — it never launches a sandbox (launching is explicit,
		// behind `run`). On a fresh host with no config, offer onboarding.
		if provision.MaybeFirstRun() {
			return
		}
		args = []string{"status"}
	}

	// A retired surface answers before anything else: no config read, no probe, no
	// side effect. Both granularities are checked, because a retired SUBCOMMAND
	// (`task gc`) must answer before its group's parser rejects the name.
	retiredIfRetired(args[0], "")
	if len(args) > 1 {
		retiredIfRetired(args[0], args[1])
	}

	// Everything else is the root's: one parser, one dispatch, one exit map.
	if code := dispatch(args, newRootDeps()); code != 0 {
		os.Exit(code)
	}
}

// looksLikePath reports whether a non-flag token is meant as a filesystem path (so
// failing to resolve it is a missing/!dir workspace, never a verb typo).
func looksLikePath(a string) bool {
	return strings.ContainsRune(a, '/') ||
		strings.HasPrefix(a, ".") ||
		strings.HasPrefix(a, "~")
}

// bareNonTTYRefusalFmt is the bare-non-interactive refusal message. It names the
// RESOLVED absolute path (never the raw, possibly-relative arg the user typed), so
// a script reading it from another cwd still gets a copy-pasteable next step.
const bareNonTTYRefusalFmt = "pix: refusing to launch %[1]q on a non-interactive terminal.\n" +
	"Run it explicitly instead:  pix run %[1]s\n"

// resolvedBareArgPath resolves a to its absolute form for the refusal message
// above, falling back to a rather than ever printing an empty path.
func resolvedBareArgPath(a string) string {
	if abs, err := filepath.Abs(a); err == nil {
		return abs
	}
	return a
}

// classifyBareArg decides what a bare (non-flag) positional means when it is not a
// matched verb: the stderr message to print, and whether it should launch `run` (an
// existing directory). A path-like token, or one that exists but is not a
// directory, is a missing/!dir workspace; only a plausible bare-word verb gets the
// did-you-mean suggester. Both non-launch branches exit 2.
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

// hostBinaryResolver locates pix-host. "Which pix-host am I paired with" is an
// identity question the launcher package answers; the var exists so a test can
// inject a fake `pix-host mcp --list` responder for setup's MCP partition.
var hostBinaryResolver = launcher.FindHostBinary

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
