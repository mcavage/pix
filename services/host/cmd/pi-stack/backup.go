package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
)

// backup.go is the launcher-side `pi-stack backup` / `pi-stack restore` \u2014 the
// TOP-LEVEL, FULL backup verbs that cover config (profiles) + op-refs + memory.
// They supersede the old `memory backup` / `memory restore` subcommands (a full
// backup is one command, not a memory-scoped one).
//
// The real work needs the sqlite driver, so it lives in pi-stack-host; these
// parse flags then exec `pi-stack-host backup|restore`, streaming output
// through. Help is config-independent (printed before any exec), and because
// backup/restore are the advertised recovery path they never load config here \u2014
// a corrupt config must not block bringing your data back.

const backupUsage = `usage: pi-stack backup [--out PATH] [--keep N]

Take a hot, consistent FULL backup \u2014 safe while 'serve' holds the db open. Packs
a VACUUM INTO snapshot of the memory DB, config.toml (profiles), op-refs.env
(refs only), and a manifest.json (profiles + knowledge-bundle notes) into a
tar.gz. Knowledge bundle CONTENT is not archived (git is its backup) \u2014 the
manifest just records where it lives.

flags:
  --out PATH   archive path (default ~/.pi-stack/backups/pi-stack-backup-<ts>.tar.gz)
  --keep N     keep only the newest N backups in the out dir (default 7)
`

const restoreUsage = `usage: pi-stack restore <archive> [--force]

Restore a FULL backup produced by 'pi-stack backup': memory.db, config.toml
(profiles come back), and op-refs.env. Refuses to run while 'serve' holds the db,
and refuses to overwrite an existing live db unless --force is given (the current
db is moved aside to a .bak-<ts> first, never deleted). config.toml/op-refs.env
are always moved aside to a .bak-<ts> before the archived versions are written.

flags:
  --force, -f   overwrite an existing live db (current db kept as .bak-<ts>)
`

// runBackup is the `pi-stack backup` entry: classify the error into an exit code.
func runBackup(argv []string) {
	if err := runBackupCore(argv, os.Stdout); err != nil {
		exitFromErr("backup", err)
	}
}

// runBackupCore parses --out/--keep and execs `pi-stack-host backup`. Help is
// printed BEFORE any exec (config-independent), matching the other verbs. It
// returns the child's error unmapped so exitFromErr propagates its exit code.
func runBackupCore(argv []string, out io.Writer) error {
	fs := newFlagSet()
	outPath := fs.str("out", "", "o")
	keep := fs.int("keep", 7)
	positional, err := fs.parse(argv)
	if err != nil {
		return err
	}
	if fs.help {
		fmt.Fprint(out, backupUsage)
		return nil
	}
	if len(positional) > 0 {
		return usageErr(backupUsage)
	}
	bin, err := findHostBinary()
	if err != nil {
		return err
	}
	hostArgs := []string{"backup", "--keep", strconv.Itoa(*keep)}
	if *outPath != "" {
		hostArgs = append(hostArgs, "--out", *outPath)
	}
	cmd := exec.Command(bin, hostArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = out
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// runRestore is the `pi-stack restore` entry: classify the error into an exit code.
func runRestore(argv []string) {
	if err := runRestoreCore(argv, os.Stdout); err != nil {
		exitFromErr("restore", err)
	}
}

// runRestoreCore parses <archive>/--force and execs `pi-stack-host restore`.
// Help + missing-archive are handled BEFORE any exec (config-independent).
func runRestoreCore(argv []string, out io.Writer) error {
	fs := newFlagSet()
	force := fs.bool("force", "f")
	positional, err := fs.parse(argv)
	if err != nil {
		return err
	}
	if fs.help {
		fmt.Fprint(out, restoreUsage)
		return nil
	}
	if len(positional) != 1 {
		return usageErr(restoreUsage)
	}
	bin, err := findHostBinary()
	if err != nil {
		return err
	}
	hostArgs := []string{"restore", positional[0]}
	if *force {
		hostArgs = append(hostArgs, "--force")
	}
	cmd := exec.Command(bin, hostArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = out
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
