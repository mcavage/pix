package main

// agent_cmd.go is `pix agent` under the cli command contract. It is the second
// verb migrated, and it is the one that shows the contract holds for a verb
// with a real flag surface: five subcommands, nine flags, and — before this —
// twenty-seven os.Exit calls reached through a hand-written `fatalLauncher`.
//
// Those exits are gone. Every handler returns an error, so `pix agent edit`
// failing to parse frontmatter is now a value a test can inspect rather than a
// process death it has to fork to observe.

import (
	"pix/host/cli"
)

const agentDescription = `Manage subagents as first-class objects.

An agent stores an INTENT, not a pinned model; the router derives its default
model from a hand-maintained scorecard. Agents live in ./agents (or
$PIX_AGENTS_DIR); run from the repo root.`

// AgentCmd is the verb tree. `list` and `remove` are kong aliases rather than
// extra switch arms, so the alias appears in generated help instead of being a
// fact you could only learn by reading the dispatcher.
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

// AgentNewCmd scaffolds an agent.
//
// The defaults live in the tags, which is the point: `--intent` defaulting to
// "code" used to be a string literal buried in the handler AND a sentence in a
// hand-written usage block, and only one of them was authoritative.
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

// AgentRmCmd removes an agent. `--yes` is required rather than prompted,
// because the command is scriptable and a prompt that a script cannot answer is
// worse than an explicit flag.
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
