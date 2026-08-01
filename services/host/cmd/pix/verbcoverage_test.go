package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
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
	"kb":        "documented abbreviation of knowledge",
	"evals":     "experimental, deliberately unlisted",
	// `route` is a one-release deprecation alias of `models` (docs/design/
	// models-cli.md). It is NOT forced into this map by
	// TestHelpListsEveryTopLevelVerb below — that test does naive
	// strings.Contains matching, and the `models route` help line already
	// supplies the token `route`, so the alias would pass with or without this
	// entry. It is added anyway as hygiene: hiddenVerbs' contract is
	// "deliberately absent from the help tree", and `route` genuinely is
	// absent as a verb. Delete this line along with `case "route"` in main.go
	// when the alias is removed.
	"route": "deprecated alias of models; removed after one release",
}

// dispatchVerbs extracts the case values of main.go's top-level `switch
// args[0]` — the actual, live set of things a user may type.
func dispatchVerbs(t *testing.T) []string {
	t.Helper()
	node, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	var verbs []string
	ast.Inspect(node, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok || sw.Tag == nil {
			return true
		}
		// The dispatch switch is the one switching on args[0].
		idx, ok := sw.Tag.(*ast.IndexExpr)
		if !ok {
			return true
		}
		id, ok := idx.X.(*ast.Ident)
		if !ok || id.Name != "args" {
			return true
		}
		for _, stmt := range sw.Body.List {
			cc, ok := stmt.(*ast.CaseClause)
			if !ok {
				continue
			}
			for _, expr := range cc.List {
				lit, ok := expr.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				if v, err := strconv.Unquote(lit.Value); err == nil {
					verbs = append(verbs, v)
				}
			}
		}
		return true
	})
	if len(verbs) < 10 {
		t.Fatalf("found only %d dispatch verbs (%v) — the switch shape moved and this test stopped testing anything", len(verbs), verbs)
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
		"config":    {"show", "path", "get", "set", "unset"},
		"mcp":       {"register", "ls", "load", "auth", "bundle"},
		"slack":     {"setup", "status", "disable"},
		"knowledge": {"init", "use", "ls", "query", "sync", "remote"},
		"secret":    {"ls", "set", "rm", "check", "sync"},
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
	core := func(v verdict) check {
		return check{label: "core", requirement: requirementCore, verdict: v, evidence: "fixture", todo: "pix setup"}
	}
	opt := func(v verdict) check {
		return check{label: "opt", requirement: requirementOptional, verdict: v, evidence: "fixture", todo: "pix setup"}
	}
	for name, tc := range map[string]struct {
		checks         []check
		full, suppress int
	}{
		"all ready":                     {[]check{core(verdictReady), opt(verdictReady)}, exitReady, exitReady},
		"core todo":                     {[]check{core(verdictTodo)}, exitNotReady, exitNotReady},
		"core denied":                   {[]check{core(verdictDenied)}, exitNotReady, exitNotReady},
		"core unverifiable":             {[]check{core(verdictUnverifiable)}, exitUnverifiable, exitReady},
		"verified failure outranks unv": {[]check{core(verdictUnverifiable), core(verdictTodo)}, exitNotReady, exitNotReady},
		"optional todo never blocks":    {[]check{core(verdictReady), opt(verdictTodo)}, exitReady, exitReady},
		"note never blocks":             {[]check{{label: "n", requirement: requirementCore, verdict: verdictTodo, note: true}}, exitReady, exitReady},
	} {
		t.Run(name, func(t *testing.T) {
			s := buildSnapshot(
				Request{Axes: []Axis{axisProviders}},
				map[Axis]axisBuilder{axisProviders: func() []check { return tc.checks }},
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
	builders := map[Axis]axisBuilder{
		axisGworkspace: func() []check {
			return []check{{label: "gworkspace", requirement: requirementOptional, verdict: verdictTodo, evidence: "not authed", todo: "pix gworkspace setup"}}
		},
	}
	unrequested := buildSnapshot(Request{Axes: []Axis{axisGworkspace}}, builders)
	if got := unrequested.ExitCode(); got != exitReady {
		t.Errorf("an unrequested optional failure must not block: exit = %d", got)
	}
	requested := buildSnapshot(Request{Axes: []Axis{axisGworkspace}, Requested: []Axis{axisGworkspace}}, builders)
	if got := requested.ExitCode(); got != exitNotReady {
		t.Errorf("a REQUESTED optional failure must block: exit = %d", got)
	}
}
