package main

// agent_cmd.go is `pix agent`: five subcommands, nine flags, and no os.Exit. Every
// handler returns an error, so `pix agent edit` failing to parse frontmatter is a
// value a test inspects rather than a process death it has to fork to observe.

import (
	"pix/host/cli"
)

const agentDescription = `Manage subagents as first-class objects.

An agent stores an INTENT, not a pinned model; the router derives its default
model from a hand-maintained scorecard. Agents live in ./agents (or
$PIX_AGENTS_DIR); run from the repo root.`

// AgentCmd is the verb tree. `list`/`remove` are kong aliases, so they appear in
// generated help instead of hiding in a dispatcher.
func (c *AgentCmd) Help() string { return agentDescription }

type AgentCmd struct {
	Ls       AgentLsCmd       `cmd:"" aliases:"list" help:"List the roster with each agent's resolved model and why."`
	New      AgentNewCmd      `cmd:"" help:"Scaffold an agent."`
	Edit     AgentEditCmd     `cmd:"" help:"Change fields without hand-editing frontmatter."`
	Rm       AgentRmCmd       `cmd:"" aliases:"remove" help:"Remove an agent."`
	Reassess AgentReassessCmd `cmd:"" help:"Re-resolve the roster under the current policy/scorecard, and recompile."`
}

type AgentLsCmd struct {
	JSON bool `help:"Emit the roster as JSON."`
}

func (c *AgentLsCmd) Run(d *cli.Deps) error { return agentLs(d, c.JSON) }

// AgentNewCmd scaffolds an agent. The defaults live in the tags, so the value a
// user reads in help is the value that parses.
type AgentNewCmd struct {
	Name        string  `arg:"" help:"Agent name (lowercase a-z0-9 and dashes)."`
	Intent      string  `default:"code" help:"Routing intent (see 'pix models show')."`
	Description string  `help:"One line describing what this agent is for."`
	Tools       string  `help:"Comma-separated tool allowlist."`
	Budget      float64 `help:"Advisory per-task budget in USD."`
	Interactive bool    `help:"Author the prompt conversationally instead of scaffolding."`
}

func (c *AgentNewCmd) Run(d *cli.Deps) error { return agentNew(d, c) }

type AgentEditCmd struct {
	Name        string  `arg:"" help:"Agent to edit."`
	Intent      string  `help:"Routing intent."`
	Description string  `help:"One line describing what this agent is for."`
	Tools       string  `help:"Comma-separated tool allowlist."`
	Budget      float64 `help:"Advisory per-task budget in USD."`
	Model       string  `help:"Pin a model, overriding the intent (discouraged)."`
}

func (c *AgentEditCmd) Run(d *cli.Deps) error { return agentEdit(d, c) }

// AgentRmCmd removes an agent. `--yes` is required rather than prompted: the
// command is scriptable, and a prompt a script cannot answer is worse than a flag.
type AgentRmCmd struct {
	Name string `arg:"" help:"Agent to remove."`
	Yes  bool   `short:"y" help:"Confirm removal."`
}

func (c *AgentRmCmd) Run(d *cli.Deps) error { return agentRm(d, c) }

// AgentReassessCmd re-resolves the roster. It spends nothing: scores are
// hand-maintained, so --model points at the scorecard rather than measuring.
type AgentReassessCmd struct {
	Model string `help:"Name a model to hand-score, instead of re-resolving the roster."`
}

func (c *AgentReassessCmd) Run(d *cli.Deps) error { return agentReassess(d, c) }
