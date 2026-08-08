package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestPrimaryHelpAndStatusAvoidEmDashes keeps the highest-traffic user-facing
// surfaces in the project's direct house style. It intentionally does not make
// a repo-wide claim about older diagnostics outside this change.
func TestPrimaryHelpAndStatusAvoidEmDashes(t *testing.T) {
	// The two files by path, not by bare name: status.go moved into
	// workflow/doctor and this guard silently stopped checking it. Every
	// hand-written file list in this repo's guards has rotted the same way as
	// packages were extracted; see TestRendererPurity and
	// assertOnlyCalledFrom for the same fix.
	for _, file := range []string{
		"help.go",
		filepath.Join("..", "..", "workflow", "doctor", "status.go"),
		// The words status PRINTS now live in the health renderer; checking
		// only the verb's file would leave the landing screen unguarded
		// again, which is the exact rot the comment above describes.
		filepath.Join("..", "..", "health", "render.go"),
	} {
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
