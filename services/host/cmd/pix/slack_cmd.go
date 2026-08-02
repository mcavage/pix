// slack_cmd.go — the argv seams for `pix slack`. These own os.Exit, build the
// real env, and supply the register function; the slack capability itself
// decides nothing about composition.
package main

import (
	"fmt"
	"os"
	"pix/host/cli"
	"pix/host/config"
	"pix/host/workflow/slack"
	"time"
)

// runSlackCmd is the `pix slack` verb tree.
func runSlackCmd(argv []string) {
	if len(argv) == 0 {
		fmt.Fprint(os.Stderr, slack.Usage)
		os.Exit(2)
	}
	if cli.WantsHelp(argv[:1]) {
		fmt.Print(slack.Usage)
		return
	}
	switch argv[0] {
	case "setup":
		runSlackSetupCmd(argv[1:])
	case "auth":
		runSlackAuthCmd(argv[1:])
	case "status":
		runSlackStatusCmd(argv[1:])
	case "disable":
		runSlackDisableCmd(argv[1:])
	default:
		fmt.Fprintf(os.Stderr, "pix slack: unknown subcommand %q (want: setup, auth, status, disable)\n", argv[0])
		os.Exit(2)
	}
}

func runSlackAuthCmd(argv []string) {
	opts, err := slack.ParseSlackAuthArgs(argv)
	if err != nil {
		if err == cli.ErrHelpRequested {
			fmt.Print(slack.AuthUsage)
			return
		}
		fmt.Fprintf(os.Stderr, "pix slack auth: %v\n\n%s", err, slack.AuthUsage)
		os.Exit(2)
	}
	tty := cli.IsTTY(os.Stdin)
	if opts.AssumeYes {
		tty = false
	}
	if err := slack.Setup(defaultShellEnv(), opts, os.Stdin, os.Stdout, tty, hostBinaryResolver, registerNoContainers); err != nil {
		fmt.Fprintf(os.Stderr, "pix slack auth: %v\n", err)
		os.Exit(1)
	}
}

func runSlackSetupCmd(argv []string) {
	opts, err := slack.ParseSlackSetupArgs(argv)
	if err != nil {
		if err == cli.ErrHelpRequested {
			fmt.Print(slack.SetupUsage)
			return
		}
		fmt.Fprintf(os.Stderr, "pix slack setup: %v\n\n%s", err, slack.SetupUsage)
		os.Exit(2)
	}
	tty := cli.IsTTY(os.Stdin)
	if opts.AssumeYes {
		tty = false // --yes means "never prompt", even on a real terminal
	}
	if err := slack.Setup(defaultShellEnv(), opts, os.Stdin, os.Stdout, tty, hostBinaryResolver, registerNoContainers); err != nil {
		fmt.Fprintf(os.Stderr, "pix slack setup: %v\n", err)
		os.Exit(1)
	}
}

func runSlackStatusCmd(argv []string) {
	if cli.WantsHelp(argv) {
		fmt.Print(slack.StatusUsage)
		return
	}
	if len(argv) > 0 {
		fmt.Fprintf(os.Stderr, "pix slack status: unexpected argument %q\n\n%s", argv[0], slack.StatusUsage)
		os.Exit(2)
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix slack status: loading config: %v\n", err)
		os.Exit(1)
	}
	os.Exit(slack.Status(cfg, defaultShellEnv(), os.Stdout, time.Now()))
}

func runSlackDisableCmd(argv []string) {
	if cli.WantsHelp(argv) {
		fmt.Print(slack.DisableUsage)
		return
	}
	if len(argv) > 0 {
		fmt.Fprintf(os.Stderr, "pix slack disable: unexpected argument %q\n\n%s", argv[0], slack.DisableUsage)
		os.Exit(2)
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix slack disable: loading config: %v\n", err)
		os.Exit(1)
	}
	if err := slack.Disable(cfg, defaultShellEnv(), os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "pix slack disable: %v\n", err)
		os.Exit(1)
	}
}
