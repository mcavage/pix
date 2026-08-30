// pix — the only host binary and the only user-facing CLI (there is no
// separate pix-host binary in v2). A consumer installs it without cloning
// the repo; it resolves PIX_HOME (default ~/.pix, no XDG split) and shells
// out to `sbx run` against the pinned pix-agent image and kit
// (pi-kit/spec.yaml), stamped to this build's version.
//
// The verb tree is root.go's rootCmd — the one parser, the one dispatcher, and (via
// `pix help --all`) the one listing. main owns only what comes BEFORE a parse: what
// bare `pix` means, and the exit code.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"pix/host/cli"
	"pix/host/launcher"
	"pix/host/workflow/provision"
)

// version is stamped at build time via -ldflags "-X main.version=0.0.x"; an
// unstamped build reports "dev" and tracks the kit's main branch. It MUST stay a
// plain string with a CONSTANT initializer: `-X` silently does nothing to a
// variable initialised from an expression, so `var version = launcher.Version`
// would leave every release reporting "dev" with no error anywhere.
var version = "dev"

func init() { launcher.Version = version }

func main() {
	args := os.Args[1:]

	if len(args) == 0 {
		// On a fresh host with no config, onboarding comes before anything else.
		if provision.MaybeFirstRun(os.Stdout) {
			return
		}
		args = bareArgs(cli.IsTTY(os.Stdin))
	}

	// Everything else is the root's: one parser, one dispatch, one exit map.
	if code := dispatch(args, newRootDeps()); code != 0 {
		os.Exit(code)
	}
}

// bareArgs decides what plain `pix` means. At a terminal it is the thing you
// almost always wanted: `run` here, which attaches to this directory's sandbox if
// one is already up and creates it otherwise. Typing the tool's name to be told
// what is up, and then having to type a second command to actually work, is a
// toll on every single session.
//
// It stays the read-only `ls` when stdin is not a terminal, and that half is
// load-bearing: an implicit launch is only ever safe when a human is sitting
// there to have meant it. `pix` in a script, a pipe, a CI step, or an editor
// task must never create or attach a sandbox as a side effect of someone
// asking what is up, so the non-interactive answer stays read-only. This
// mirrors the identical rule on a bare positional (`pix DIR`, see dispatch):
// bare launches need a TTY, the explicit `pix run` never does.
func bareArgs(interactive bool) []string {
	if interactive {
		return []string{"run"}
	}
	return []string{"ls"}
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
// did-you-mean suggester.
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

const helpText = `pix: a personal, multi-model pi coding agent in a Docker sandbox.

Usage:  pix <command> [args]

New here?   pix setup      one-time guided setup (a few minutes, resumable)

Workflow & parallel work
  run [DIR]        launch or re-attach DIR's sandbox (default: .) — plain "pix"
  ls               list your pix sandboxes;  rm <name>  removes one
  task             parallel task checkouts: new | ls | path | rm

Setup & health
  setup            guided setup: PIX_HOME, images, the memory container
  doctor           diagnose problems and print the exact fix commands
  reset            clean slate: remove sandboxes + memory container, back up ~/.pix

Environments & credentials
  env              named environments under ~/.pix/envs: list | show | default | trust
  secret           1Password references: list | set | rm | check

Learn a command:  pix help run     ·     pix <command> -h     ·     pix help --all
`
