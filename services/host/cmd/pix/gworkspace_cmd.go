// gworkspace_cmd.go — the argv seam for `pix gworkspace`. Owns os.Exit, builds
// the real env, and supplies the credentials resolver.
package main

import (
	"fmt"
	"os"
	"pix/host/cli"
	"pix/host/config"
	"pix/host/workflow/gworkspace"
)

// runGworkspaceCmd is the `pix gworkspace` verb tree.
//
// The help gate is checked ONLY for the no-subcommand case, exactly as the
// former `gog` tree did: a blanket wantsHelp over the whole argv would catch
// `gworkspace setup -h` and print the noun-level usage instead of the
// subcommand's own.
func runGworkspaceCmd(argv []string) {
	if len(argv) == 0 {
		fmt.Fprint(os.Stderr, gworkspace.Usage)
		os.Exit(2)
	}
	if cli.WantsHelp(argv[:1]) {
		fmt.Print(gworkspace.Usage)
		return
	}
	switch argv[0] {
	case "setup":
		runGworkspaceSetupCmd(argv[1:])
	case "status":
		runGworkspaceStatusCmd(argv[1:])
	case "disable":
		runGworkspaceDisableCmd(argv[1:])
	default:
		fmt.Fprintf(os.Stderr, "pix gworkspace: unknown subcommand %q (want: setup, status, disable)\n", argv[0])
		os.Exit(2)
	}
}

// runGworkspaceSetupCmd parses flags, wires the real hostenv.Env (the
// browser-opening auth steps inherit THIS process's stdio), and runs the
// unchanged transaction.
func runGworkspaceSetupCmd(argv []string) {
	opts, err := gworkspace.ParseGworkspaceSetupArgs(argv)
	if err != nil {
		if err == cli.ErrHelpRequested {
			fmt.Print(gworkspace.SetupUsage)
			return
		}
		fmt.Fprintf(os.Stderr, "pix gworkspace setup: %v\n\n%s", err, gworkspace.SetupUsage)
		os.Exit(2)
	}
	tty := cli.IsTTY(os.Stdin)
	if opts.AssumeYes {
		tty = false // --yes means "never prompt", even on a real terminal
	}
	if err := gworkspace.Setup(defaultShellEnv(), opts, os.Stdin, os.Stdout, tty, mcpCredentials); err != nil {
		fmt.Fprintf(os.Stderr, "pix gworkspace setup: %v\n", err)
		os.Exit(1)
	}
}

func runGworkspaceStatusCmd(argv []string) {
	if cli.WantsHelp(argv) {
		fmt.Print(gworkspace.StatusUsage)
		return
	}
	if len(argv) > 0 {
		fmt.Fprintf(os.Stderr, "pix gworkspace status: unexpected argument %q\n\n%s", argv[0], gworkspace.StatusUsage)
		os.Exit(2)
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix gworkspace status: loading config: %v\n", err)
		os.Exit(1)
	}
	os.Exit(gworkspace.Status(cfg, defaultShellEnv(), os.Stdout))
}

func runGworkspaceDisableCmd(argv []string) {
	if cli.WantsHelp(argv) {
		fmt.Print(gworkspace.DisableUsage)
		return
	}
	if len(argv) > 0 {
		fmt.Fprintf(os.Stderr, "pix gworkspace disable: unexpected argument %q\n\n%s", argv[0], gworkspace.DisableUsage)
		os.Exit(2)
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix gworkspace disable: loading config: %v\n", err)
		os.Exit(1)
	}
	if err := gworkspace.Disable(cfg, defaultShellEnv(), os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "pix gworkspace disable: %v\n", err)
		os.Exit(1)
	}
}
