// reset_cmd.go — the `pix reset` command struct. Flags are declared once here
// and parsed by the root; the domain (what is moved aside, in what order, and
// what refuses without a confirmation) is workflow/reset's.
package main

import (
	"errors"
	"time"

	"pix/host/cli"
	"pix/host/workflow/reset"
	"pix/host/workspace"
)

const resetDescription = `Reset the stack to a clean slate (REVERSIBLE). Nothing is hard-deleted:
state is moved aside to a timestamped <path>.bak-<unixts> sibling you can
rename back. Run 'pix help reset' for what each flag keeps.`

func (c *resetCmd) Help() string { return resetDescription }

type resetCmd struct {
	KeepMemory bool `help:"Keep the memory database (move everything else aside)."`
	Sbx        bool `help:"Also remove this host's pix-* sandboxes."`
	Yes        bool `short:"y" help:"Assume yes: required to reset a non-interactive terminal."`
	Force      bool `help:"Proceed past a refusal that is safe to override."`
}

func (c *resetCmd) Run(d *cli.Deps) error {
	cfg, _, err := workspace.LoadResolvedConfig()
	if err != nil {
		return err
	}
	env := defaultShellEnv()
	rio := cli.IO{In: d.In, Out: d.Out, IsTTY: d.Interactive}
	opts := reset.NewOpts(c.KeepMemory, c.Sbx, c.Yes, c.Force)
	err = reset.RunCore(cfg, reset.ResolveResetPaths(env), opts, reset.DefaultResetFS(), env, rio, time.Now)
	if errors.Is(err, reset.ErrResetNeedsYes) {
		return cli.Usagef("refusing to reset a non-interactive terminal without confirmation; re-run with --yes")
	}
	return err
}
