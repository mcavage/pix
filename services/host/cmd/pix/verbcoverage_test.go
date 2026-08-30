package main

import (
	"strings"
	"testing"
)

// verbcoverage_test.go: the two tests that would have caught a verb nobody can
// find. They read the ROOT TREE itself (root.go's rootCmd, via rootVerbs()),
// not a hand-maintained list, so a verb added to the tree and forgotten in
// help fails the build instead of shipping undiscoverable.

// hiddenVerbs is the ONLY escape hatch: verbs deliberately absent from the
// help tree. Every entry needs a reason on its line — an entry without one is
// a verb someone hid to make this test pass.
var hiddenVerbs = map[string]string{
	"-h":        "alias of help, not a verb",
	"--help":    "alias of help, not a verb",
	"-v":        "alias of version, not a verb",
	"--version": "alias of version, not a verb",
	"ls":        "documented abbreviation, listed under its long form",
}

// dispatchVerbs is the actual, live set of things a user may type: the
// children of the kong root. It was main.go's `switch args[0]`, then rootVerbs
// derived off kong's Model.Children; either way the list is derived from the
// parser itself and cannot describe a verb that is not dispatchable.
func dispatchVerbs(t *testing.T) []string {
	t.Helper()
	verbs := rootVerbs()
	if len(verbs) < 8 {
		t.Fatalf("found only %d root verbs (%v) — the root tree moved and this test stopped testing anything", len(verbs), verbs)
	}
	return verbs
}

// TestHelpListsEveryTopLevelVerb: everything the root dispatches is
// discoverable in `pix help --all`, or explicitly listed as hidden with a
// reason. This is the test that would have caught a verb being dispatchable
// but unlisted.
func TestHelpListsEveryTopLevelVerb(t *testing.T) {
	for _, verb := range dispatchVerbs(t) {
		if _, hidden := hiddenVerbs[verb]; hidden {
			continue
		}
		if !strings.Contains(helpAll(), verb) {
			t.Errorf("verb %q is dispatched but absent from `pix help --all` (add it, or add it to hiddenVerbs with a reason)", verb)
		}
	}
}

// TestEveryDispatchedSubcommandAppearsInItsUsage: a multi-subcommand verb
// must name every subcommand its own dispatch accepts. Each verb's generated
// usage (`pix help <verb>`) is parsed for the subcommand token — this is the
// test that would have caught `task path` being implemented and unlisted.
// The v1 verbs this used to cover (config, mcp, models, secret sync) are
// gone with the router/pack/mcp-registry surfaces they belonged to; only the
// v2 multi-subcommand verbs remain.
func TestEveryDispatchedSubcommandAppearsInItsUsage(t *testing.T) {
	for verb, subs := range map[string][]string{
		"task":   {"new", "ls", "path", "rm"},
		"env":    {"list", "show", "default", "trust"},
		"secret": {"ls", "set", "rm", "check"},
	} {
		d, out, _ := rootDeps()
		dispatch([]string{"help", verb}, d)
		usage := out.String()
		if strings.TrimSpace(usage) == "" {
			t.Errorf("verb %q has no usage text", verb)
			continue
		}
		for _, sub := range subs {
			if !strings.Contains(usage, sub) {
				t.Errorf("`pix %s %s` is dispatched but missing from its usage text:\n%s", verb, sub, usage)
			}
		}
	}
}
