package main

// memory_cmd.go is `pix memory`: the host-side CLI over the memory daemon (:11435),
// so recall can be inspected and repaired WITHOUT launching a sandbox.
//
// Two behaviours are contracts: the daemon is lazily auto-started before a real
// subcommand (never on a help request, which stays side-effect free), and a down
// daemon exits 3, distinct from usage (2).

import (
	"errors"
	"fmt"
	"strings"

	"pix/host/cli"
	"pix/host/memory"
	"pix/host/rpc"
	"pix/host/service"
	"pix/host/workspace"
)

func (c *memoryCmd) Help() string {
	return `Inspect and repair the agent's recall without launching a sandbox.

Facts live in the memory daemon (:11435), started by 'pix serve': these verbs
auto-start it if it is down (up to 3s), and exit 3 if it cannot be reached.
They never restart an already-running daemon; run 'pix serve start' to move
it to a newer version.

The only unreproducible artifact is memory.db; config.toml is recreated with
"pix config set" and op-refs.env holds op:// references, not secrets, so
neither needs a backup. Snapshot/restore live on the host binary:
  pix-host memory snapshot PATH           hot: safe while the service runs
  pix-host memory restore  PATH [--force] stopped-service: stop the daemon first`
}

type memoryCmd struct {
	Recall    memoryRecallCmd    `cmd:"" help:"Search stored facts."`
	Remember  memoryRememberCmd  `cmd:"" help:"Store a fact. (WRITES)"`
	Forget    memoryForgetCmd    `cmd:"" help:"Delete a fact by id or id prefix. (WRITES)"`
	Learnings memoryLearningsCmd `cmd:"" help:"Recurring learnings, promotable into a skill."`
	Stats     memoryStatsCmd     `cmd:"" help:"Counts by kind and durability."`
}

// withMemory resolves what every subcommand needs (a live-enough daemon, a client,
// the scope profile), runs the call, and maps the one failure the root cannot
// classify: a down daemon is exit 3 with the recovery command. EnsureUp is
// best-effort; on failure the client's own ErrServiceDown lands here.
//
// EnsureMemoryTimeout (not the general EnsureTimeout) bounds the wait: this is
// a READ-SIDE, foreground command a human is staring at, so a cold daemon gets
// a short 3s allowance rather than the general 15s cold-start budget — and,
// per U3-lifecycle, this path never restarts an already-running daemon for a
// version mismatch (that reconciliation lives only on `pix serve start`).
func withMemory(d *cli.Deps, sub string, call func(memory.CLI) error) error {
	service.EnsureUp(d.Err, []string{"memory"}, service.EnsureMemoryTimeout)
	_, profile, err := workspace.LoadResolvedConfig()
	if err == nil {
		err = call(memory.CLI{Client: rpc.MemoryClient(), Out: d.Out, Profile: profile})
	}
	switch {
	case err == nil:
		return nil
	case errors.Is(err, rpc.ErrServiceDown):
		// This must not flatly say "start it with `pix serve`" (architect round
		// 2): EnsureUp, just above, ALREADY tried to auto-start the daemon —
		// telling the user to start ANOTHER one when the real cause is a cold
		// start that outran the 3s EnsureMemoryTimeout budget (sqlite init under
		// an advisory flock is not always instant) is actively wrong advice, not
		// merely unhelpful. It is honest about BOTH real causes instead: a slow
		// autostart that just needs a retry, or a genuinely down/opted-out
		// daemon that does need starting.
		fmt.Fprintf(d.Err, "pix memory %s: service unreachable: if it just started, wait a moment and retry — otherwise start it with `pix serve`\n", sub)
		return cli.SilentError{Code: rpc.ExitServiceDown}
	default:
		return fmt.Errorf("memory %s: %w", sub, err)
	}
}

// A free-text tail is ONE value, not N arguments: `remember a new fact` always meant
// one fact.
type memoryRecallCmd struct {
	Query   []string `arg:"" help:"What to search for (free text)."`
	Limit   int      `default:"8" help:"Maximum hits to return." placeholder:"N"`
	Project string   `help:"Restrict to one project." placeholder:"P"`
	JSON    bool     `help:"Emit machine-readable JSON."`
}

func (c *memoryRecallCmd) Run(d *cli.Deps) error {
	return withMemory(d, "recall", func(m memory.CLI) error {
		return m.Recall(strings.Join(c.Query, " "), c.Limit, c.Project, c.JSON)
	})
}

type memoryRememberCmd struct {
	Text []string `arg:"" help:"The fact to store (free text)."`
	JSON bool     `help:"Emit machine-readable JSON."`
}

func (c *memoryRememberCmd) Run(d *cli.Deps) error {
	return withMemory(d, "remember", func(m memory.CLI) error {
		return m.Remember(strings.Join(c.Text, " "), c.JSON)
	})
}

type memoryForgetCmd struct {
	ID   string `arg:"" help:"Fact id, or a unique prefix of one."`
	JSON bool   `help:"Emit machine-readable JSON."`
}

func (c *memoryForgetCmd) Run(d *cli.Deps) error {
	return withMemory(d, "forget", func(m memory.CLI) error { return m.Forget(c.ID, c.JSON) })
}

type memoryLearningsCmd struct {
	Min  int  `default:"3" help:"Only lessons seen at least N times." placeholder:"N"`
	JSON bool `help:"Emit machine-readable JSON."`
}

func (c *memoryLearningsCmd) Run(d *cli.Deps) error {
	return withMemory(d, "learnings", func(m memory.CLI) error { return m.Learnings(c.Min, c.JSON) })
}

type memoryStatsCmd struct {
	JSON bool `help:"Emit machine-readable JSON."`
}

func (c *memoryStatsCmd) Run(d *cli.Deps) error {
	return withMemory(d, "stats", func(m memory.CLI) error { return m.Stats(c.JSON) })
}
