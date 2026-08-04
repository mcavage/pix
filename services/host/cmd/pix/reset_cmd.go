// reset_cmd.go — the argv seam for `pix reset` / `pix uninstall`.
package main

import (
	"errors"
	"fmt"
	"os"
	"pix/host/cli"
	"pix/host/workflow/reset"
	"pix/host/workspace"
	"time"
)

// runReset is the `reset` verb entry point.
func runReset(argv []string) {
	opts, err := reset.ParseArgs(argv, true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix reset: %v\n\n%s", err, reset.Usage)
		os.Exit(2)
	}
	if opts.Help {
		fmt.Print(reset.Usage)
		return
	}
	cfg, _, err := workspace.LoadResolvedConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix reset: %v\n", err)
		os.Exit(1)
	}
	env := defaultShellEnv()
	rio := cli.IO{In: os.Stdin, Out: os.Stdout, IsTTY: cli.IsTTY(os.Stdin)}
	if err := reset.RunCore(cfg, reset.ResolveResetPaths(env), opts, reset.DefaultResetFS(), env, rio, time.Now); err != nil {
		if errors.Is(err, reset.ErrResetNeedsYes) {
			fmt.Fprintln(os.Stderr, "pix reset: refusing to reset a non-interactive terminal without confirmation")
			fmt.Fprintln(os.Stderr, "re-run with --yes to reset non-interactively")
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "pix reset: %v\n", err)
		os.Exit(1)
	}
}
