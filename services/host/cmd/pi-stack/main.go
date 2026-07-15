// pi-stack — the user-facing launcher for the pi-stack sandbox. Unlike the old
// repo-relative bin/pi-stack shell script, this is a standalone binary a
// consumer installs without cloning the repo: it reads ~/.config/pi-stack config
// and shells out to `sbx run pi-stack`, pinning the git-hosted kit to this
// build's stamped version.
//
// Host convention is Go (one static binary; see services/host/main.go), and this
// launcher shares that binary's config package (pi-stack/host/config) so the two
// agree on config location + the broker token.
//
// Verb tree:
//
//	pi-stack [DIR]                     alias for `run [DIR]`
//	pi-stack run [DIR] [flags] [-- …]  launch the sandbox (full)
//	pi-stack version                   print the stamped version (full)
//	pi-stack config show|path          show config path + contents (full)
//	pi-stack serve [args…]             exec the sibling pi-stack-host serve (full)
//	pi-stack status|doctor|setup|mcp|memory|knowledge|profile   (all implemented)
//	pi-stack models|upgrade|uninstall  (stubbed — later units)
//	pi-stack help [verb]               print the verb tree (or one verb's usage)
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// version is stamped at build time via -ldflags "-X main.version=0.0.x". An
// unstamped build reports "dev" and tracks the kit's main branch.
var version = "dev"

func main() {
	// A global `--profile <name>` may appear before the subcommand; pull it out
	// first so both `pi-stack --profile work run` and `pi-stack run --profile work`
	// work. flagProfile is consumed by loadResolvedConfig / run.
	args, perr := extractProfileFlag(os.Args[1:])
	if perr != nil {
		fmt.Fprintf(os.Stderr, "pi-stack: %v\n", perr)
		os.Exit(2)
	}

	if len(args) == 0 {
		// Bare `pi-stack` shows STATUS — never launches a sandbox (launching is
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
	case "version", "--version", "-v":
		if len(args) > 1 && (args[1] == "-h" || args[1] == "--help") {
			fmt.Print(versionUsage)
			return
		}
		fmt.Println(version)
	case "config":
		runConfig(args[1:])
	case "serve":
		runServe(args[1:])
	case "doctor":
		runDoctorCmd(args[1:])
	case "setup":
		runSetupCmd(args[1:])
	case "mcp":
		runMcpCmd(args[1:])
	case "secret":
		runSecretCmd(args[1:])
	case "memory", "mem":
		runMemory(args[1:])
	case "knowledge", "kb":
		runKnowledge(args[1:])
	case "profile":
		runProfile(args[1:])
	case "models", "upgrade", "uninstall":
		stub(args[0])
	case "help", "-h", "--help":
		if len(args) > 1 {
			if u, ok := verbUsage(args[1]); ok {
				fmt.Print(u)
				return
			}
		}
		fmt.Print(helpText)
	default:
		// A bare positional is a run workspace ONLY when it names an existing
		// directory (e.g. `pi-stack ~/dev/foo`). A non-directory word is a typo
		// (e.g. `pi-stack memoyr`) — do NOT silently launch a sandbox for it.
		if a := args[0]; len(a) > 0 && a[0] != '-' {
			if fi, err := os.Stat(a); err == nil && fi.IsDir() {
				runVerb(args)
				return
			}
			fmt.Fprintf(os.Stderr, "pi-stack: unknown command %q (and no such directory)\n\n", a)
			fmt.Print(helpText)
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "pi-stack: unknown flag %q\n\n", args[0])
		fmt.Print(helpText)
		os.Exit(2)
	}
}

// firstRunHook is the onboarding entry point, indirected through a package var
// so the help-before-onboarding ordering is unit-testable: a test swaps it for a
// spy and asserts a help short-circuit never reaches it.
var firstRunHook = maybeFirstRun

// runVerb handles the `run` verb and the bare-DIR alias. It short-circuits to
// run usage on a -h/--help request BEFORE any side effect (notably first-run
// onboarding), so `pi-stack run --help` prints help even on a config-less host
// instead of dropping into the setup prompt. Otherwise it runs onboarding (which
// may fully handle a fresh host) then launches the sandbox.
func runVerb(argv []string) {
	if wantsHelp(argv) {
		fmt.Print(runUsage)
		return
	}
	if firstRunHook() {
		return
	}
	runRun(argv)
}

// runServe execs the sibling pi-stack-host binary's `serve` subcommand, found
// next to this binary or on PATH, passing along any args.
func runServe(argv []string) {
	// A leading -h/--help prints serve usage instead of execing the host binary.
	if len(argv) > 0 && (argv[0] == "-h" || argv[0] == "--help") {
		fmt.Print(serveUsage)
		return
	}
	bin, err := findHostBinary()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack serve: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "pi-stack serve: exec %s: %v\n", bin, err)
		os.Exit(1)
	}
}

// findHostBinary locates pi-stack-host next to argv[0] first (the common
// install layout), then falls back to PATH.
func findHostBinary() (string, error) {
	if self, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(self), "pi-stack-host")
		if fi, err := os.Stat(sibling); err == nil && !fi.IsDir() {
			return sibling, nil
		}
	}
	if p, err := exec.LookPath("pi-stack-host"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("pi-stack-host not found next to this binary or on PATH")
}

// stub prints a clearly-labeled "not yet implemented" line so the verb surface
// exists and is testable ahead of the units that fill it in.
func stub(name string) {
	fmt.Fprintf(os.Stderr, "pi-stack %s: not yet implemented — coming in a later unit\n", name)
	os.Exit(2)
}

const runUsage = `usage: pi-stack run [DIR] [flags] [-- pi-args...]

flags:
  --dev            Mode B: use the local checkout kit + load skills live from it
                   (needs a checkout; resolves $PI_STACK_DEV_ROOT, the cwd, or
                   the launcher's own location)
  --skills DIR     mount an extra skill tree and load it live (repeatable)
  --kit K          override the kit (escape hatch): replaces the auto git/local
                   pin, so you can work around an unresolvable release tag
                   (repeatable; a path or git+URL)
  --mcp M          attach an MCP server at creation (repeatable)
  --name N         sandbox name
  --model M        active pi model (passed through to pi)

released vs local:
  A RELEASED launcher (clean version like 0.0.16) pins the matching kit tag
  (git+...#ref=v0.0.16). An UNRELEASED/local build (version with +local, a dev
  build, or non-semver) never pins a nonexistent v<version> tag: it uses your
  local checkout kit when one is resolvable (also pinning the locally loaded
  image via --template), otherwise it falls back to #ref=main with a warning.

DIR defaults to the current directory. Everything after -- is passed to pi.
Set PI_STACK_DEBUG=1 to print the composed sbx command.
`

const helpText = `pi-stack — the pi-stack sandbox + host services

usage: pi-stack [--profile NAME] <command> [args]

new here?  pi-stack setup     (guided one-time setup)

commands:
  (none)              show status (this does NOT launch a sandbox)
  status              host + services + sandboxes at a glance   [--json]
  setup               guided first-run setup (writes config + registers MCP)
  doctor              diagnose host + sandbox health
  run [DIR] [flags]   launch the sandbox (launching is explicit)
  serve [args...]     run the host services (execs pi-stack-host serve)

  memory <cmd>        recall|remember|forget|learnings|stats   (:11435)
  knowledge <cmd>     init|use|ls|query|sync|remote            (:11436)
  mcp register|ls     register local stdio MCP servers with the sbx gateway
  secret <cmd>        status|edit|check the 1Password op-refs (host MCP creds)
  profile ls|use      switch between contexts (work / personal / default)

  config show|path    show the resolved config path and contents
  config set|unset    change config without hand-editing the toml
  version             print the launcher version
  help [run]          print this help (or run-flag help)

global: --profile NAME   run/read a named profile (work, personal, ...)
run flags: --dev --skills DIR --kit K --mcp M --name N --model M -- pi-args...
`
