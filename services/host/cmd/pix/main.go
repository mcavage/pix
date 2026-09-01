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
	deps := newRootDeps()
	exitCode := 0

	if len(args) == 0 {
		interactive := cli.IsTTY(os.Stdin)
		var stop bool
		args, exitCode, stop = planBareInvocation(interactive, provision.FirstRunNeeded(), func() int {
			fmt.Fprintln(os.Stdout, "pix: first run; setting up this PIX_HOME before launch")
			return dispatch([]string{"setup"}, deps)
		})
		if !stop {
			exitCode = dispatch(args, deps)
		}
	} else {
		exitCode = dispatch(args, deps)
	}

	if exitCode != 0 {
		os.Exit(exitCode)
	}
}

// planBareInvocation preserves the implicit-launch safety boundary while making
// the interactive first run real: setup must succeed before the ordinary run.
// Explicit `pix run` never passes through here and remains the setup opt-out.
func planBareInvocation(interactive, firstRun bool, setup func() int) (args []string, code int, stop bool) {
	if !interactive {
		return []string{"ls"}, 0, false
	}
	if firstRun {
		if code := setup(); code != 0 {
			return nil, code, true
		}
	}
	return []string{"run"}, 0, false
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
