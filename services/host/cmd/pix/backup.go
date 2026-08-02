package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"pix/host/cli"
	"strconv"
)

// backup.go is the launcher-side `pix backup` / `pix restore` \u2014 the
// TOP-LEVEL, FULL backup verbs that cover config (profiles) + op-refs + memory.
// They supersede the old `memory backup` / `memory restore` subcommands (a full
// backup is one command, not a memory-scoped one).
//
// The real work needs the sqlite driver, so it lives in pix-host; these
// parse flags then exec `pix-host backup|restore`, streaming output
// through. Help is config-independent (printed before any exec), and because
// backup/restore are the advertised recovery path they never load config here \u2014
// a corrupt config must not block bringing your data back.

const backupUsage = `usage: pix backup [--out PATH] [--keep N]

Take a hot, consistent FULL backup \u2014 safe while 'serve' holds the db open. Packs
a VACUUM INTO snapshot of the memory DB, config.toml (profiles), op-refs.env
(refs only), and a manifest.json (profiles + knowledge-bundle notes) into a
tar.gz. Knowledge bundle CONTENT is not archived (git is its backup) \u2014 the
manifest just records where it lives.

flags:
  --out PATH   archive path (default ~/.local/share/pix/backups/pix-backup-<ts>.tar.gz)
  --keep N     keep only the newest N backups in the out dir (default 7)
`

const restoreUsage = `usage: pix restore <archive> [--force]

Restore a FULL backup produced by 'pix backup': memory.db, config.toml
(profiles come back), and op-refs.env. Refuses to run while 'serve' holds the db,
and refuses to overwrite an existing live db unless --force is given (the current
db is moved aside to a .bak-<ts> first, never deleted). config.toml/op-refs.env
are always moved aside to a .bak-<ts> before the archived versions are written.

flags:
  --force, -f   overwrite an existing live db (current db kept as .bak-<ts>)
`

// runBackup is the `pix backup` entry: classify the error into an exit code.
func runBackup(argv []string) {
	if err := runBackupCore(argv, os.Stdout); err != nil {
		cli.ExitFromErr("backup", err)
	}
}

// runBackupCore parses --out/--keep and execs `pix-host backup`. Help is
// printed BEFORE any exec (config-independent), matching the other verbs. It
// returns the child's error unmapped so cli.ExitFromErr propagates its exit code.
func runBackupCore(argv []string, out io.Writer) error {
	fs := cli.NewFlagSet()
	outPath := fs.Str("out", "", "o")
	keep := fs.Int("keep", 7)
	positional, err := fs.Parse(argv)
	if err != nil {
		return err
	}
	if fs.Help {
		fmt.Fprint(out, backupUsage)
		return nil
	}
	if len(positional) > 0 {
		return cli.UsageErr(backupUsage)
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

// runRestore is the `pix restore` entry: classify the error into an exit code.
func runRestore(argv []string) {
	if err := runRestoreCore(argv, os.Stdout); err != nil {
		cli.ExitFromErr("restore", err)
	}
}

// runRestoreCore parses <archive>/--force and execs `pix-host restore`.
// Help + missing-archive are handled BEFORE any exec (config-independent).
func runRestoreCore(argv []string, out io.Writer) error {
	fs := cli.NewFlagSet()
	force := fs.Bool("force", "f")
	positional, err := fs.Parse(argv)
	if err != nil {
		return err
	}
	if fs.Help {
		fmt.Fprint(out, restoreUsage)
		return nil
	}
	if len(positional) != 1 {
		return cli.UsageErr(restoreUsage)
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
