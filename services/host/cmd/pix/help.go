package main

import (
	"pix/host/cli"
	"pix/host/mcp"
	"pix/host/memory"
	"pix/host/service"
	"pix/host/workflow/doctor"
	"pix/host/workflow/launch"
	"pix/host/workflow/pack"
	"pix/host/workflow/reset"
	"pix/host/workflow/setup"
)

// cli.ErrHelpRequested is the shared sentinel a parser returns when the argv asks
// knownVerbs is the set of top-level verbs, used to suggest a fix when a bare
// positional (a would-be run DIR) is actually a mistyped verb.
var knownVerbs = map[string]bool{
	"help": true, "serve": true, "doctor": true, "setup": true, "status": true,
	"ls": true, "rm": true,
	"config": true, "mcp": true, "memory": true, "monitor": true,
	"pack": true, "version": true, "run": true, "secret": true,
	"reset": true,
	"state": true,
	"task":  true, "models": true, "agent": true,
}

// suggestVerb returns the closest known verb to input within edit distance 2 —
// the did-you-mean hint on an unknown command. It no longer carries the retired
// names: a retired surface is DISPATCHED (retired.go) and answers with its own
// replacement before this is ever reached, which is strictly more useful than a
// hint on an error path.
func suggestVerb(input string) (string, bool) {
	best, bestD := "", 3
	for v := range knownVerbs {
		if d := levenshtein(input, v); d < bestD {
			best, bestD = v, d
		}
	}
	return best, best != ""
}

// levenshtein is the classic edit distance (insert/delete/substitute), used by
// suggestVerb to rank near-miss verb typos.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	cur := make([]int, len(rb)+1)
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min3(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}

// helpAllText is the full command listing revealed by `pix help --all`. It
// names every verb across all tiers (Core / occasional / rare + expert) so a
// power user can see the whole surface, not just the curated Core view.
const helpAllText = `pix: a personal, multi-model pi coding agent in a Docker sandbox.

Usage:  pix <command> [args]

New here?   pix setup     configure the host, then hand off to an agent for a guided tour

Workflow
  run [DIR]           launch the sandbox in DIR (default: .). This is the main one.
  ls [--json]         list your pix sandboxes (name, state, dir)
  rm <name>...        remove pix sandboxes (--all [--except <name>])
  serve [args...]     start the host services (memory, knowledge); serve stop|status
  status              what is up, what is down, what is next   (also the bare command)

Setup & health
  setup               guided onboarding: host config, then agent handoff for a guided tour
  setup --no-agent    host-side config only (flags/CI); no sandbox, no handoff
  doctor              diagnose host + sandbox health, print the fix commands

Data
  memory <cmd>        recall | remember | forget | learnings | stats   (:11435)
  pack <cmd>          new | add | ls | show | use | rm (git-backed context bundle)

Observability
  monitor [name]       live-follow a sandbox's out-of-sandbox traffic (:11437)

Models & agents (cost/latency/accuracy routing)
  models              which models pix can use, and which are wired up
  models add <prov>   wire another provider key into callable models
  models route        recompile the intent -> model map the sandbox reads
  agent <cmd>         ls | new | edit | rm | reassess (subagents as objects)

Config & context
  config show|path    show the resolved config path and contents
  config get K        print one resolved value (for scripts/make)
  config set|unset    change config without hand-editing the toml

Parallel work
  task <cmd>          new | ls | path | rm: parallel task clones of one repo

Integrations & credentials
  mcp <cmd>           register|ls|load|auth|bundle MCP servers (sbx gateway)
  secret <cmd>        ls|set|rm|readiness.Check the 1Password op-refs (host MCP creds)

State (on-disk lifecycle)
  state <cmd>         reset (grouped alias)
  reset [flags]       move Pix's XDG state aside (reversible) [--keep-memory --sbx --yes]

Meta
  version             print the launcher version
  help [verb]         print this help (or a verb's usage)

run flags:    --dev --skills DIR --kit K --template REF --mcp M --name N --model M -- pi-args...
`

// verbUsage maps a verb (including its aliases) to its usage text, so
// `pix help <verb>` can route to each verb's own help. The returned string
// is newline-terminated and ready to Print. ok is false for an unknown verb.
func verbUsage(verb string) (string, bool) {
	switch verb {
	case "run":
		return runUsage, true
	case "serve":
		return service.Usage, true
	case "status", "st":
		return doctor.StatusUsage, true
	case "ls":
		return launch.LsUsage, true
	case "rm":
		return launch.RmUsage, true
	case "doctor":
		return doctor.Usage, true
	case "setup":
		return setup.Usage, true
	case "config":
		return setup.ConfigUsage, true
	case "mcp":
		return mcp.McpUsage, true
	case "pack":
		return pack.Usage, true
	case "memory", "mem":
		return memory.Usage + "\n", true
	case "monitor":
		return monitorUsage, true
	case "secret":
		return secretUsage(), true
	case "version":
		return versionUsage, true
	case "reset":
		return reset.Usage, true
	case "state":
		return stateUsage, true
	case "task":
		return taskUsage, true
	case "models":
		return modelsUsage(), true
	case "agent":
		return agentUsage(), true
	}
	return "", false
}

// service.Usage moved to service.Usage, with the capability that owns the verb.

// secretHelpBody is the mental model reused verbatim from config so the concept
// reads identically in setup, doctor, the template header, and `secret -h`.
// secretUsage renders the SAME help kong prints, so `pix help secret` cannot
// drift from `pix secret --help`. It was a hand-written block that listed
// argument counts the parser also enforced, separately.
func secretUsage() string { return cli.Usage[SecretCmd]("secret", secretDescription) }

const versionUsage = `usage: pix version

Print the stamped launcher version.
`
