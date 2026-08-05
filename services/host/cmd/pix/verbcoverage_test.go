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

