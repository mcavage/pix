// pack_cmd.go — the argv seam for `pix pack`, plus the composition wiring the
// pack capability deliberately does not do for itself: building the real env,
// supplying the MCP register function, and pinning the local-MCP classifier.
package main

import (
	"fmt"
	"os"
	"pix/host/cli"
	"pix/host/workflow/pack"
)

// init supplies the real classifier PackLocalMCP defaults to a safe no-op for.
// Only the composition root can build a real env, so only it can answer "is
// this MCP server local"; pack asks the question and cmd/pix owns the answer.
func init() {
	pack.PackLocalMCP = func() func(string) bool {
		env := defaultShellEnv()
		return pack.LocalMCPClassifier(env, env.HostBinary)
	}
}

func runPackCmd(argv []string) {
	if cli.WantsHelp(argv) {
		fmt.Print(pack.Usage)
		return
	}
	sub := "ls"
	var rest []string
	if len(argv) > 0 {
		sub, rest = argv[0], argv[1:]
	}
	env := defaultShellEnv()
	switch sub {
	case "new":
		pack.RunPackNew(env, os.Stdout, rest)
	case "add":
		pack.RunPackAdd(env, os.Stdout, rest, registerServers)
	case "ls":
		pack.RunPackLs(os.Stdout)
	case "show":
		pack.RunPackShow(defaultShellEnv(), os.Stdout, rest)
	case "use":
		pack.RunPackUse(env, os.Stdout, rest, registerServers)
	case "rm":
		pack.RunPackRm(os.Stdout, rest)
	default:
		fmt.Fprintf(os.Stderr, "pix pack: unknown subcommand %q (want: new, add, ls, show, use, rm)\n", sub)
		os.Exit(2)
	}
}
