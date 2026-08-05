// retired.go — the host binary's retired-subcommand table. pix-host is
// machine-facing (launchd plists, `sbx mcp add` registrations, scripts), so a
// retired subcommand answers like the launcher's: a greppable PIX_RETIRED line
// naming the replacement, exit 2, nothing else. Approval + reasoning per
// retirement: cmd/pix/corpus/retirement.jsonl.
package main

import (
	"fmt"
	"strings"
)

// retiredHostSubcommands maps a retired subcommand to its replacement.
// `serve knowledge` needs no entry (serve's alias table dropped the name).
// The top-level `backup`/`restore` verbs were multi-component archive tooling
// (memory + config + op-refs in a versioned tar.gz); the artifact worth keeping
// is the memory db, so they collapsed into `memory snapshot`/`memory restore`.
func retiredHostSubcommands() map[string]string {
	return map[string]string{
		// The bridge that replacement named is gone too (U11j: it served zero
		// built-in servers), so the pointer moves to where a Slack server
		// actually comes from now rather than to a second dead path.
		"slack":   "sbx mcp add slack --local --url <manifest> (a container the gateway runs; docs/design/slack-setup.md)",
		"backup":  "pix-host memory snapshot PATH",
		"restore": "pix-host memory restore PATH",
	}
}

func retiredHostMessage(name string) string {

	return fmt.Sprintf("PIX_RETIRED: `pix-host %s` was retired. Use `%s` instead.\n", name, retiredHostSubcommands()[name])
}

// retiredHostNotice returns the notice for a retired argv, and whether it
// matched. The CALLER owns the exit: this hangs off the existing
// unknown-subcommand branch, already exiting 2.
func retiredHostNotice(argv []string) (string, bool) {
	for n := len(argv); n > 0; n-- {
		if name := strings.Join(argv[:n], " "); retiredHostSubcommands()[name] != "" {
			return retiredHostMessage(name), true
		}
	}
	return "", false
}
