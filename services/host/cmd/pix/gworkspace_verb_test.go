// gworkspace_verb_test.go — `pix gworkspace` must appear in the verb usage
// table, which is a cmd/pix fact.
package main

import (
	"pix/host/cli"
	"testing"
)

func TestRunGogCmd_HelpAndUnknownSubcommand(t *testing.T) {
	if !cli.WantsHelp([]string{"-h"}) {
		t.Fatal("sanity")
	}
	if _, ok := verbUsage("gworkspace"); !ok {
		t.Error("verbUsage(gog) should be known")
	}
	if !knownVerbs["gworkspace"] {
		t.Error(`knownVerbs["gworkspace"] should be true`)
	}
}
