// slack_verb_test.go — `pix slack` must be in the verb table and its usage
// must list every subcommand. The subject is cmd/pix's dispatch, so it stays
// here while the slack capability lives in pix/host/slack.
package main

import (
	"strings"
	"testing"
)

// TestSlackVerbDiscoverable is a focused sibling of the generic
// TestHelpListsEveryTopLevelVerb/TestManPageDocumentsEveryKnownVerb checks
// (verbcoverage_test.go, man_test.go), which already cover `slack` because
// they read the dispatch switch / knownVerbs live. This pins the four
// subcommands specifically, so a future edit that drops one from Usage
// fails locally in this file too.
func TestSlackVerbDiscoverable(t *testing.T) {
	if !knownVerbs["slack"] {
		t.Error(`"slack" must be in knownVerbs so it is discoverable/documented`)
	}
	usage, ok := verbUsage("slack")
	if !ok {
		t.Fatal(`verbUsage("slack") must resolve`)
	}
	for _, sub := range []string{"setup", "auth", "status", "disable"} {
		if !strings.Contains(usage, sub) {
			t.Errorf("pix slack usage is missing subcommand %q:\n%s", sub, usage)
		}
	}
}
