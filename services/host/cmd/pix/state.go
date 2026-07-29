package main

import (
	"fmt"
	"os"
)

const stateUsage = `usage: pix state <backup|restore|reset> [args]

Group for the stack's on-disk lifecycle. Each subcommand is identical to its
top-level alias (which keeps working):

  backup [--out P] [--keep N]   hot FULL backup (memory + config + op-refs) -> tar.gz
  restore <archive> [--force]   restore a FULL backup (safe swap)
  reset [flags]                 move stack state aside (reversible)
Run ` + "`pix help <subcommand>`" + ` for full flags.
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
	switch argv[0] {
	case "backup":
		runBackup(argv[1:])
	case "restore":
		runRestore(argv[1:])
	case "reset":
		runReset(argv[1:])
	default:
		fmt.Fprintf(os.Stderr, "pix state: unknown subcommand %q\n\n%s", argv[0], stateUsage)
		os.Exit(2)
	}
}
