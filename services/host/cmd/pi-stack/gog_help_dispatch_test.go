package main

import (
	"strings"
	"testing"
)

// TestGogSetupHelp_ReachesDetailedUsage is the DX-1 P0 regression: `pi-stack
// gog setup -h` was intercepted by runGogCmd's blanket wantsHelp(argv) gate
// (wantsHelp scans the WHOLE remaining argv for -h/--help, so it fired on
// ["setup", "-h"] before the "setup" subcommand ever got dispatched) and
// printed the noun-level gogUsage instead of the detailed gogSetupUsage
// (flags, numbered steps). The gate must only catch `gog -h`/`gog --help`
// with NO subcommand — once a subcommand token is present, dispatch to it and
// let it own its own -h/--help handling.
func TestGogSetupHelp_ReachesDetailedUsage(t *testing.T) {
	for _, help := range []string{"-h", "--help"} {
		out := captureStdout(t, func() { runGogCmd([]string{"setup", help}) })
		if out != gogSetupUsage {
			t.Fatalf("runGogCmd([\"setup\", %q\"]) = %q, want the detailed gogSetupUsage %q", help, out, gogSetupUsage)
		}
		// Exact-output guard also asserts the flags doctor callers need are
		// actually present in what got printed.
		for _, want := range []string{"--account <email>", "--credentials <path>", "--yes"} {
			if !strings.Contains(out, want) {
				t.Fatalf("gog setup %s output missing flag %q:\n%s", help, want, out)
			}
		}
	}
}

// TestGogTopLevelHelp_StillNounUsage guards the OTHER half of the contract:
// `pi-stack gog -h` / `--help` with no subcommand must still print the
// top-level noun usage (gogUsage), not fall through to "unknown subcommand".
func TestGogTopLevelHelp_StillNounUsage(t *testing.T) {
	for _, help := range []string{"-h", "--help"} {
		out := captureStdout(t, func() { runGogCmd([]string{help}) })
		if out != gogUsage {
			t.Fatalf("runGogCmd([%q]) = %q, want the noun-level gogUsage %q", help, out, gogUsage)
		}
	}
}
