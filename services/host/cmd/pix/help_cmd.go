// help_cmd.go — the `help` verb. It stays PASSTHROUGH on purpose: `pix help
// <anything>` is a question, never a mistake, so it must answer with the
// tiered screen instead of a parse error (a user who types `pix help --nope`
// wants help, and gets it, exit 0).
package main

import (
	"fmt"

	"pix/host/cli"
)

// helpCmd is the tiered screen, `--all`, or one verb's usage.
type helpCmd struct {
	Args []string `arg:"" optional:"" passthrough:"" help:"A verb to explain, or --all for the whole surface."`
}

// Run: every verb is typed, so there is no usage constant left to print —
// `pix help ls` re-enters the root as `pix ls --help`, and the usage a user
// reads is generated from the same tags that parse it.
func (c *helpCmd) Run(d *cli.Deps) error {
	if legacyIntercepted("help", c.Args) {
		return nil
	}
	if len(c.Args) > 0 {
		if c.Args[0] == "--all" {
			fmt.Fprint(d.Out, helpAll())
			return nil
		}
		if v := c.Args[0]; v != "help" && knownVerbs()[v] {
			dispatch([]string{v, "--help"}, d)
			return nil
		}
	}
	fmt.Fprint(d.Out, helpText)
	return nil
}
