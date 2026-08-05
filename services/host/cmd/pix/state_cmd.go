// state_cmd.go — `pix state`, the grouping noun for the stack's on-disk
// lifecycle. It is an ALIAS group and nothing else: `state reset` IS `reset`,
// the same typed command struct, so the two spellings cannot drift apart the
// way a group that parses its own flags always does.
package main

import "pix/host/cli"

const stateDescription = `Group for the stack's on-disk lifecycle.

Archiving state is no longer a launcher verb: use ` + "`pix-host memory snapshot`" +
	` / ` + "`pix-host memory restore`" + `.`

// stateCmd carries the SAME resetCmd the top-level verb does, so `pix state
// reset --help` and `pix reset --help` are generated from one declaration.
func (c *stateCmd) Help() string { return stateDescription }

type stateCmd struct {
	Reset resetCmd `cmd:"" help:"Move Pix's state aside (reversible). (WRITES)"`
}

// Run is unreachable in practice (kong requires a subcommand), and exists so a
// bare `pix state` is a usage error rather than a nil-command panic.
func (c *stateCmd) Run(*cli.Deps) error {
	return cli.Usagef("state needs a subcommand (see `pix state --help`)")
}
