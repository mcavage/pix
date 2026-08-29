package main

// agent_cmd.go is `pix agent`: one subcommand, ls, and no os.Exit. new / edit /
// rm / reassess were retired (see retired.go) — an agent is a hand-edited
// agents/<name>.md file now, not a CLI mutation surface.

import (
	"pix/host/cli"
)

const agentDescription = `List the subagent roster: AGENT, MODEL, SOURCE.

An agent's model comes from its own frontmatter 'model:' (explicit, wins
outright), else the machine's selected environment roster (pix.toml
[agents].<name>, else [models].main). No environment roster and no explicit
model: means "(inherit parent)" -- this command never picks a model for
you, and nothing else does either. The roster is $PIX_AGENTS_DIR if set, else a directory found
beside the pix binary, else a pix repo checkout's agents/ (found by climbing
from the current directory -- works from any subdirectory, not only the repo
root). A packaged install with none of those prints exactly what to set.

new/edit/rm/reassess are retired: edit agents/*.md directly (add/change/remove
the file), or the selected environment's pix.toml [agents]/[models] table to
change what it resolves to.`

// AgentCmd is the verb tree. `list` is a kong alias, so it appears in
// generated help instead of hiding in a dispatcher.
func (c *AgentCmd) Help() string { return agentDescription }

type AgentCmd struct {
	Ls AgentLsCmd `cmd:"" aliases:"list" help:"List the roster: AGENT, MODEL, SOURCE."`
}

type AgentLsCmd struct {
	JSON bool `help:"Emit the roster as JSON."`
}

func (c *AgentLsCmd) Run(d *cli.Deps) error { return agentLs(d, c.JSON) }
