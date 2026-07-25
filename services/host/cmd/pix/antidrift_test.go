package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// TestPrimaryHelpAndStatusAvoidEmDashes keeps the highest-traffic user-facing
// surfaces in the project's direct house style. It intentionally does not make
// a repo-wide claim about older diagnostics outside this change.
func TestPrimaryHelpAndStatusAvoidEmDashes(t *testing.T) {
	for _, file := range []string{"help.go", "status.go"} {
		node, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		ast.Inspect(node, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(lit.Value)
			if err == nil && strings.Contains(value, "—") {
				t.Errorf("%s contains an em dash in a user-facing string: %q", file, value)
			}
			return true
		})
	}
}
