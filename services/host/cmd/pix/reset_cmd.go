// reset_cmd.go — the `pix reset` command struct. Flags are declared once here
// and parsed by the root; what is moved aside, in what order, and what refuses
// without a confirmation is workflow/reset's.
//
// The one thing this layer OWNS is the sandbox sweep it injects: reset is L3
// and workflow/launch is its L3 sibling, so only the command layer may hold
// both. It passes `pix rm --all` itself rather than a re-implementation, which
// is what makes "reset never force-removes a sandbox" a property of the code
// instead of a promise in a comment.
package main

import (
	"fmt"
	"io"
	"time"

	"pix/host/cli"
	"pix/host/pixhome"
	"pix/host/workflow/launch"
	"pix/host/workflow/reset"
)

func (c *resetCmd) Help() string { return reset.Description }

type resetCmd struct {
	KeepMemory    bool `help:"Keep the captured-memory store (move everything else aside)."`
	KeepSandboxes bool `help:"Leave this host's pix-* sandboxes alone."`
	Yes           bool `short:"y" help:"Assume yes: required to reset a non-interactive terminal."`
	Force         bool `help:"Move the data dir even when serve cannot be confirmed down."`
}

func (c *resetCmd) Run(d *cli.Deps) error {
	// pix reset is the v2 PIX_HOME clean slate (docs/design/
	// pix-v2-architecture.md §12, §16.14): sweep sandboxes, then stop+remove+
	// prove-absent the memory container, then rename PIX_HOME aside. It never
	// reads the (still-live, v1) workspace config — ResetHome does not need
	// to know which MCP registrations to drop; it only tears down what it
	// itself owns.
	if !c.Yes && !d.Interactive {
		return cli.Usagef("refusing to reset a non-interactive terminal without confirmation; re-run with --yes")
	}
	home, err := pixhome.Resolve()
	if err != nil {
		return err
	}
	res, err := reset.ResetHome(reset.HomeDeps{
		Home:   home.Home,
		Sweep:  rmAllSandboxes(d),
		Out:    d.Out,
		ErrOut: d.Err,
		Now:    time.Now,
	})
	if err != nil {
		return err
	}
	if res.BackupPath != "" {
		fmt.Fprintf(d.Out, "pix: PIX_HOME moved aside to %s\n", res.BackupPath)
	} else {
		fmt.Fprintln(d.Out, "pix: nothing to reset (PIX_HOME did not exist).")
	}
	return nil
}

// rmAllSandboxes is `pix rm --all` as a callable: the SAME non-forced,
// zero-reference-proof teardown the verb runs, with Force unset (launch.Rm
// refuses --force with --all anyway, so this cannot drift into a bulk force
// seam) and no keep set, since reset is explicitly asking for all of them.
func rmAllSandboxes(d *cli.Deps) reset.Sweep {
	return func(out, errOut io.Writer) error {
		return sbxAwareFail(d, launch.Rm(defaultShellEnv(), out, errOut, launch.RmOptions{
			All: true, Interactive: d.Interactive,
		}))
	}
}
