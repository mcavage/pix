package main

// agent_cmd.go is `pix agent`: one subcommand, ls, and no os.Exit. new / edit /
// rm / reassess were retired (see retired.go) — an agent is a hand-edited
// agents/<name>.md file plus scorecard.json now, not a CLI mutation surface.

import (
	"pix/host/cli"
)

const agentDescription = `List the subagent roster.

An agent stores an INTENT, not a pinned model; the router derives its default
model from a hand-maintained scorecard. The roster is $PIX_AGENTS_DIR if set,
else a directory found beside the pix binary, else a pix repo checkout's
agents/ (found by climbing from the current directory — works from any
subdirectory, not only the repo root). A packaged install with none of those
prints exactly what to set.

new/edit/rm/reassess are retired: edit agents/*.md directly (add/change/remove
the file), hand-score a new intent's models in scorecard.json if needed, then
run 'pix models route' to recompile and relaunch the sandbox to pick it up.`

// AgentCmd is the verb tree. `list` is a kong alias, so it appears in
// generated help instead of hiding in a dispatcher.
func (c *AgentCmd) Help() string { return agentDescription }

type AgentCmd struct {
	Ls AgentLsCmd `cmd:"" aliases:"list" help:"List the roster with each agent's resolved model and why."`
}

type AgentLsCmd struct {
	JSON bool `help:"Emit the roster as JSON."`
}

func (c *AgentLsCmd) Run(d *cli.Deps) error { return agentLs(d, c.JSON) }
