// doctorfixture_test.go — copies of doctor's test-only fixtures that cmd/pix
// tests still need. Copies, not exports: scaffolding has no business in a
// package's public API, and a bulk export pass will happily put it there.
package main

import (
	"time"
)

// bareGog / opWrappedGog are the two ways `pix mcp register` actually wires

// gog (see mcp.go serverCmd/addArgs): a BARE `gog … mcp …` command when no

// op-refs.env is present (1Password is optional for gog), or that same command

// behind the `op run --env-file=<refs> -- …` wrapper when op-refs is present.

// Tests use these so the fixtures match a real registration rather than a

// stand-in binary.

func bareGog(acct string) string {
	return "gog --account " + acct +
		" --gmail-no-send --wrap-untrusted --readonly mcp --allow-tool read"
}

func opWrappedGog(refs, acct string) string {
	return "op run --no-masking --env-file=" + refs + " -- " + bareGog(acct)
}

const gogOpRefs = "/fake/config/op-refs.env"

const gogAcct = "you@example.com"

// gogCfgFile / gogOpRefs are the fake $PIX_CONFIG + resolved op-refs path

// the gog fixtures use: setting PIX_CONFIG makes resolveOpRefs return

// gogOpRefs (its dir + op-refs.env), which doctor.GogHeadlessOK then probes with.

const gogCfgFile = "/fake/config/config.toml"

// bareGog / opWrappedGog are the two ways `pix mcp register` actually wires

// gog (see mcp.go serverCmd/addArgs): a BARE `gog … mcp …` command when no

// op-refs.env is present (1Password is optional for gog), or that same command

// behind the `op run --env-file=<refs> -- …` wrapper when op-refs is present.

// Tests use these so the fixtures match a real registration rather than a

// stand-in binary.

var receiptClock = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }

// TestMCPAttachmentFromReceipt covers the attachment axis: ONLY the launcher

// receipt (a record of successful pix actions) may claim attachment.
