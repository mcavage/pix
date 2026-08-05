package main

import (
	"fmt"
	"strings"

	"github.com/alecthomas/kong"

	"pix/host/mcp"
	"pix/host/memory"
	"pix/host/workflow/doctor"
	"pix/host/workflow/pack"
	"pix/host/workflow/provision"
)

// rootNodes returns the kong root's top-level commands, in declaration order.
// What a verb IS, what it does and which tier it lives in are all answered by
// the parser, because a list beside the parser can only be a second answer.
func rootNodes() []*kong.Node {
	parser, err := kong.New(&rootCmd{}, kong.Name("pix"), kong.Exit(func(int) {}))
	if err != nil {
		return nil
	}
	return parser.Model.Children
}

// knownVerbs is every name a user may type as the first token, aliases
// included. It tells a mistyped verb from a would-be `run` DIR.
func knownVerbs() map[string]bool {
	out := map[string]bool{}
	for _, n := range rootNodes() {
		out[n.Name] = true
		for _, a := range n.Aliases {
			out[a] = true
		}
	}
	return out
}

// helpAll renders `pix help --all`: the whole surface, tier by tier, GENERATED
// from the root's `group:`/`help:` tags instead of hand-listed beside them.
func helpAll() string {
	var b strings.Builder
	b.WriteString("pix: a personal, multi-model pi coding agent in a Docker sandbox.\n\nUsage:  pix <command> [args]\n\nNew here?   pix setup     one-time guided setup (a few minutes, resumable)\n")
	group := ""
	for _, n := range rootNodes() {
		if g := n.ClosestGroup(); g != nil && g.Key != group {
			group = g.Key
			fmt.Fprintf(&b, "\n%s\n", group)
		}
		fmt.Fprintf(&b, "  %-9s %s\n", n.Name, n.Help)
	}
	b.WriteString("\nLearn a command:  pix help <command>   ·   pix <command> -h\n")
	return b.String()
}

// suggestVerb returns the closest known verb to input within edit distance 2 —
// the did-you-mean hint on an unknown command. It no longer carries the retired
// names: a retired surface is DISPATCHED (retired.go) and answers with its own
// replacement before this is ever reached, which is strictly more useful than a
// hint on an error path.
func suggestVerb(input string) (string, bool) {
	best, bestD := "", 3
	for v := range knownVerbs() {
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
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// verbUsage maps a LEGACY verb (and its aliases) to the usage constant its own
// seam prints, so `pix help <verb>` routes to it. A migrated verb is absent on
// purpose: it has no constant, and runHelp re-enters the root for it instead.
// ok is false for an unknown or already-migrated verb.
func verbUsage(verb string) (string, bool) {
	switch verb {
	case "run":
		return runUsage, true
	case "status", "st":
		return doctor.StatusUsage, true
	case "doctor":
		return doctor.Usage, true
	case "setup":
		return provision.Usage, true
	case "config":
		return provision.ConfigUsage, true
	case "mcp":
		return mcp.McpUsage, true
	case "pack":
		return pack.Usage, true
	case "memory", "mem":
		return memory.Usage + "\n", true
	case "state":
		return stateUsage, true
	}
	return "", false
}
