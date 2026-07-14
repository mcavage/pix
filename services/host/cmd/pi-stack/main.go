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
//	pi-stack doctor|setup|mcp|models|upgrade|uninstall   (stubbed — later units)
//	pi-stack help                      print this verb tree
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
	if len(os.Args) < 2 {
		// Bare `pi-stack` == `run` in the current directory.
		runRun(nil)
		return
	}

	switch os.Args[1] {
	case "run":
		runRun(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println(version)
	case "config":
		runConfig(os.Args[2:])
	case "serve":
		runServe(os.Args[2:])
	case "doctor":
		runDoctorCmd(os.Args[2:])
	case "setup":
		runSetupCmd(os.Args[2:])
	case "mcp":
		runMcpCmd(os.Args[2:])
	case "models", "upgrade", "uninstall":
		stub(os.Args[1])
	case "help", "-h", "--help":
		fmt.Print(helpText)
	default:
		// A bare positional (e.g. `pi-stack ~/dev/foo`) is a run workspace.
		if len(os.Args[1]) > 0 && os.Args[1][0] != '-' {
			runRun(os.Args[1:])
			return
		}
		fmt.Fprintf(os.Stderr, "pi-stack: unknown subcommand %q\n\n", os.Args[1])
		fmt.Print(helpText)
		os.Exit(2)
	}
}

// runServe execs the sibling pi-stack-host binary's `serve` subcommand, found
// next to this binary or on PATH, passing along any args.
func runServe(argv []string) {
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
  --dev            Mode B: load skills live from a repo checkout (needs a checkout)
  --skills DIR     mount an extra skill tree and load it live (repeatable)
  --kit K          stack an extra kit (repeatable)
  --mcp M          attach an MCP server at creation (repeatable)
  --name N         sandbox name
  --model M        active pi model (passed through to pi)

DIR defaults to the current directory. Everything after -- is passed to pi.
Set PI_STACK_DEBUG=1 to print the composed sbx command.
`

const helpText = `pi-stack — launch the pi-stack sandbox

usage: pi-stack <command> [args]

commands:
  run [DIR] [flags]   launch the sandbox (also the default with no command)
  version             print the launcher version
  config show|path    show the resolved config path and contents
  config set|unset    change config without hand-editing the toml
  serve [args...]     run the host services (execs pi-stack-host serve)
  doctor              diagnose host + sandbox health
  setup               guided first-run setup (writes config + registers MCP)
  mcp register|ls     register local stdio MCP servers with the sbx gateway
  models              manage local Ollama models                (coming in a later unit)
  upgrade             update the launcher + kit                 (coming in a later unit)
  uninstall           remove pi-stack from this host            (coming in a later unit)
  help                print this help

run flags: --dev --skills DIR --kit K --mcp M --name N --model M -- pi-args...
`
