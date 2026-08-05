package main

import (
	"pix/host/cli"
	"pix/host/readiness"
	"strings"
	"testing"
)

// verbcoverage_test.go: the two tests that would have caught a verb nobody can
// find. They read the DISPATCH SWITCH itself (main.go's `switch args[0]`), not
// a hand-maintained list, so a verb added to the switch and forgotten in help
// fails the build instead of shipping undiscoverable.

// hiddenVerbs is the ONLY escape hatch: verbs deliberately absent from the
// help tree. Every entry needs a reason on its line — an entry without one is
// a verb someone hid to make this test pass.
var hiddenVerbs = map[string]string{
	"-h":        "alias of help, not a verb",
	"--help":    "alias of help, not a verb",
	"-v":        "alias of version, not a verb",
	"--version": "alias of version, not a verb",
	"st":        "documented abbreviation of status",
	"ls":        "documented abbreviation, listed under its long form",
	"mem":       "documented abbreviation of memory",
}

// dispatchVerbs is the actual, live set of things a user may type: the
// children of the kong root. It was main.go's `switch args[0]`, parsed out of
// the source; the root is now the only dispatcher, so the list is derived from
// the parser itself and cannot describe a verb that is not dispatchable.
func dispatchVerbs(t *testing.T) []string {
	t.Helper()
	verbs := cli.RootVerbs[rootCmd]()
	if len(verbs) < 10 {
		t.Fatalf("found only %d root verbs (%v) — the root tree moved and this test stopped testing anything", len(verbs), verbs)
	}
	return verbs
}

// TestHelpListsEveryTopLevelVerb: everything the dispatch switch accepts is
// discoverable in `pix help --all`, or explicitly listed as hidden with a
// reason. This is the test that would have caught `gworkspace` being
// dispatchable but unlisted.
func TestHelpListsEveryTopLevelVerb(t *testing.T) {
	for _, verb := range dispatchVerbs(t) {
		if _, hidden := hiddenVerbs[verb]; hidden {
			continue
		}
		if !strings.Contains(helpAllText, verb) {
			t.Errorf("verb %q is dispatched but absent from `pix help --all` (add it, or add it to hiddenVerbs with a reason)", verb)
		}
	}
}

// TestEveryDispatchedSubcommandAppearsInItsUsage: a verb with its own usage
// text must name every subcommand its own dispatch accepts. The measured pairs
// below are the multi-subcommand verbs; each one's usage string is parsed for
// the subcommand token. This is the test that would have caught `task path`
// being implemented and unlisted.
func TestEveryDispatchedSubcommandAppearsInItsUsage(t *testing.T) {
	for verb, subs := range map[string][]string{
		"config": {"show", "path", "get", "set", "unset"},
		"mcp":    {"register", "ls", "load", "auth", "bundle"},
		"secret": {"ls", "set", "rm", "check", "sync"},
		// "add" and "setup" join this list once the reconcile seam lands
		// (docs/design/models-cli.md); this rename-only change only wires
		// ls/show/pick/route.
		"models": {"ls", "show", "pick", "route"},
	} {
		usage, ok := verbUsage(verb)
		if !ok {
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

// TestExitMatrix pins the ONE exit contract, including the two commands that
// suppress the 3 arm. Precedence is 2 > 1 > 3 > 0 (a verified failure outranks
// an unverifiable); usage errors are produced by argument parsing before any
// probe, so they can never be derived from a snapshot and are not in this
// table.
func TestExitMatrix(t *testing.T) {
	core := func(v readiness.Verdict) readiness.Check {
		return readiness.Check{Label: "core", Requirement: readiness.RequirementCore, Verdict: v, Evidence: "fixture", Todo: "pix setup"}
	}
	opt := func(v readiness.Verdict) readiness.Check {
		return readiness.Check{Label: "opt", Requirement: readiness.RequirementOptional, Verdict: v, Evidence: "fixture", Todo: "pix setup"}
	}
	for name, tc := range map[string]struct {
		checks         []readiness.Check
		full, suppress int
	}{
		"all ready":                     {[]readiness.Check{core(readiness.VerdictReady), opt(readiness.VerdictReady)}, readiness.ExitReady, readiness.ExitReady},
		"core todo":                     {[]readiness.Check{core(readiness.VerdictTodo)}, readiness.ExitNotReady, readiness.ExitNotReady},
		"core denied":                   {[]readiness.Check{core(readiness.VerdictDenied)}, readiness.ExitNotReady, readiness.ExitNotReady},
		"core unverifiable":             {[]readiness.Check{core(readiness.VerdictUnverifiable)}, readiness.ExitUnverifiable, readiness.ExitReady},
		"verified failure outranks unv": {[]readiness.Check{core(readiness.VerdictUnverifiable), core(readiness.VerdictTodo)}, readiness.ExitNotReady, readiness.ExitNotReady},
		"optional todo never blocks":    {[]readiness.Check{core(readiness.VerdictReady), opt(readiness.VerdictTodo)}, readiness.ExitReady, readiness.ExitReady},
		"note never blocks":             {[]readiness.Check{{Label: "n", Requirement: readiness.RequirementCore, Verdict: readiness.VerdictTodo, Note: true}}, readiness.ExitReady, readiness.ExitReady},
	} {
		t.Run(name, func(t *testing.T) {
			s := readiness.Build(
				readiness.Request{Axes: []readiness.Axis{readiness.AxisProviders}},
				map[readiness.Axis]readiness.AxisBuilder{readiness.AxisProviders: func() []readiness.Check { return tc.checks }},
			)
			if got := s.ExitCode(); got != tc.full {
				t.Errorf("ExitCode() = %d, want %d", got, tc.full)
			}
			if got := s.ExitCodeSuppressingUnverifiable(); got != tc.suppress {
				t.Errorf("ExitCodeSuppressingUnverifiable() = %d, want %d", got, tc.suppress)
			}
		})
	}
}

// TestRequestedPromotionBlocks: an OPTIONAL axis the user explicitly asked for
// on this invocation blocks like core, and promotion happens in the snapshot
// type rather than in any command's flag handling.
func TestRequestedPromotionBlocks(t *testing.T) {
	builders := map[readiness.Axis]readiness.AxisBuilder{
		readiness.AxisGworkspace: func() []readiness.Check {
			return []readiness.Check{{Label: "gworkspace", Requirement: readiness.RequirementOptional, Verdict: readiness.VerdictTodo, Evidence: "not authed", Todo: "pix mcp register"}}
		},
	}
	unrequested := readiness.Build(readiness.Request{Axes: []readiness.Axis{readiness.AxisGworkspace}}, builders)
	if got := unrequested.ExitCode(); got != readiness.ExitReady {
		t.Errorf("an unrequested optional failure must not block: exit = %d", got)
	}
	requested := readiness.Build(readiness.Request{Axes: []readiness.Axis{readiness.AxisGworkspace}, Requested: []readiness.Axis{readiness.AxisGworkspace}}, builders)
	if got := requested.ExitCode(); got != readiness.ExitNotReady {
		t.Errorf("a REQUESTED optional failure must block: exit = %d", got)
	}
}
