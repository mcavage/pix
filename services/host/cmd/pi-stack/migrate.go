// pi-stack migrate — the launcher verb that relocates storage to the standard
// XDG layout. It is a thin front for `pi-stack-host migrate`: the real work (the
// sqlite integrity_check, the index rebuild, the flock-guarded memory move) lives
// in the HOST binary, so the launcher just parses -h and execs it, streaming
// stdio through. Help is printed BEFORE any exec and is config-independent (no
// side effects), mirroring runServe.

package main

import (
	"fmt"
	"os"
	"os/exec"
)

const migrateUsage = `usage: pi-stack migrate

Relocate pi-stack storage to the standard XDG layout, EXPLICITLY and once:
  memory + knowledge bundle + backups  ->  ~/.local/share/pi-stack   (DATA)
  knowledge index + caches + tasks     ->  ~/.local/state/pi-stack   (STATE)
  config.toml + op-refs + broker token  stay in ~/.config/pi-stack   (CONFIG)

Nothing precious is deleted: each artifact is moved (or copied + verified across
filesystems) and the legacy path is left as a symlink or a .pre-xdg-<ts> safety
copy. The knowledge index is REBUILT, not moved. The memory database moves only
while its lock is free (a running 'pi-stack serve' defers it, safely). Re-running
is safe and idempotent. Existing installs keep working in place until you run it.
`

// runMigrate is the `migrate` verb entry point. It prints usage on -h/--help
// (config-independent, no side effects), then execs the sibling pi-stack-host
// migrate, streaming its stdio and preserving its exit code.
func runMigrate(argv []string) {
	if wantsHelp(argv) {
		fmt.Print(migrateUsage)
		return
	}
	bin, err := findHostBinary()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack migrate: %v\n", err)
		os.Exit(1)
	}
	cmd := exec.Command(bin, append([]string{"migrate"}, argv...)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			os.Exit(exit.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "pi-stack migrate: exec %s: %v\n", bin, err)
		os.Exit(1)
	}
}
