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
	"errors"
	"io"
	"time"

	"pix/host/cli"
	"pix/host/workflow/launch"
	"pix/host/workflow/reset"
	"pix/host/workspace"
)

func (c *resetCmd) Help() string { return reset.Description }

type resetCmd struct {
	KeepMemory    bool `help:"Keep the captured-memory store (move everything else aside)."`
	KeepSandboxes bool `help:"Leave this host's pix-* sandboxes alone."`
	Yes           bool `short:"y" help:"Assume yes: required to reset a non-interactive terminal."`
	Force         bool `help:"Move the data dir even when serve cannot be confirmed down."`
}

func (c *resetCmd) Run(d *cli.Deps) error {
	// A reset that could not READ the config would also not know which MCP
	// registrations to drop, so it fails here rather than half-resetting.
	cfg, _, err := workspace.LoadResolvedConfig()
	if err != nil {
		return err
	}
	env := defaultShellEnv()
	err = reset.Run(cfg, reset.ResolvePaths(env), reset.NewOpts(c.KeepMemory, c.KeepSandboxes, c.Yes, c.Force), reset.Runtime{
		FS:    reset.DefaultResetFS(),
		Env:   d.Sys,
		IO:    cli.IO{In: d.In, Out: d.Out, IsTTY: d.Interactive},
		ErrW:  d.Err,
		Sweep: rmAllSandboxes(d),
		Now:   time.Now,
	})
	if errors.Is(err, reset.ErrNeedsYes) {
		return cli.Usagef("refusing to reset a non-interactive terminal without confirmation; re-run with --yes")
	}
	return err
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
