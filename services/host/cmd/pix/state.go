package main

import (
	"fmt"
	"os"
)

const stateUsage = `usage: pix state reset [args]

Group for the stack's on-disk lifecycle.

  reset [flags]                 move stack state aside (reversible)

Run ` + "`pix help reset`" + ` for full flags. Archiving state is no longer a
launcher verb: use ` + "`pix-host backup`" + ` / ` + "`pix-host restore`" + `.
`

// runState is a thin verbatim dispatcher for the `state` grouping noun. It does
// NOT reimplement any behavior; it forwards to the existing flat entry points
// (which keep working as top-level aliases). A bare noun and -h/--help print the
// group usage and exit 0; an unknown subcommand is a usage error (exit 2).
func runState(argv []string) {
	if len(argv) == 0 || argv[0] == "-h" || argv[0] == "--help" {
		fmt.Print(stateUsage)
		return
	}
	retiredIfRetired("state", argv[0])
	switch argv[0] {
	case "reset":
		// Re-enter the ROOT rather than reimplementing the verb: `state reset`
		// is an alias, and an alias that parses its own flags is how the two
		// spellings drift.
		if code := dispatch(append([]string{"reset"}, argv[1:]...), newRootDeps()); code != 0 {
			os.Exit(code)
		}
	default:
		fmt.Fprintf(os.Stderr, "pix state: unknown subcommand %q\n\n%s", argv[0], stateUsage)
		os.Exit(2)
	}
}
