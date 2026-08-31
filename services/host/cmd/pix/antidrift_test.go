package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// scanEmDashSource is the pure form of the em-dash guard: parse src (Go
// source text; filename is only used for parse-error/position reporting)
// and return one description per string literal that contains an em dash.
// It never calls into *testing.T, so a planted-violation self-test
// (TestScanEmDashSource_SelfTest) can assert "found exactly one finding"
// without a failing subtest's Fail() propagating up to a meta-test that
// must itself pass — the same split env_copy_lint_test.go's
// scanCopyLintSource and errors_test.go's familyCopyViolations already use
// for their own AST/rendered-text scanners.
func scanEmDashSource(t *testing.T, filename string, src []byte) []string {
	t.Helper()
	node, err := parser.ParseFile(token.NewFileSet(), filename, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}
	var findings []string
	ast.Inspect(node, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(lit.Value)
		if err == nil && strings.Contains(value, "—") {
			findings = append(findings, fmt.Sprintf("%s contains an em dash in a user-facing string: %q", filename, value))
		}
		return true
	})
	return findings
}

// TestScanEmDashSource_SelfTest is this guard's own planted-violation proof
// (finding C13's "expand ... self-test where appropriate"): a scanner
// nobody has seen catch anything is not a scanner. Mirrors
// env_copy_lint_test.go's TestEnvCopyLint_SelfTest and errors_test.go's
// TestAssertFamilyCopy_SelfTest for this package's third AST scanner.
func TestScanEmDashSource_SelfTest(t *testing.T) {
	planted := "package planted\n\nconst s = \"pix: bad \u2014 thing\"\n"
	if got := scanEmDashSource(t, "planted.go", []byte(planted)); len(got) != 1 {
		t.Errorf("scanEmDashSource found %d finding(s) for a planted em dash, want exactly 1: %v", len(got), got)
	}
	clean := "package planted\n\nconst s = \"pix: no problem here\"\n"
	if got := scanEmDashSource(t, "planted.go", []byte(clean)); len(got) != 0 {
		t.Errorf("scanEmDashSource on clean source found %v, want none", got)
	}
}

// TestPrimaryHelpAndStatusAvoidEmDashes keeps the highest-traffic user-facing
// surfaces in the project's direct house style. It intentionally does not make
// a repo-wide claim about older diagnostics outside this change.
func TestPrimaryHelpAndStatusAvoidEmDashes(t *testing.T) {
	// The files by path, not by bare name: a hand-written file list in this
	// repo's guards has rotted before as packages were extracted; see
	// TestRendererPurity and assertOnlyCalledFrom for the same fix.
	// workflow/doctor/status.go (the `pix status` verb's own short-form
	// renderers) was deleted outright as unreachable dead code (AC-16), not
	// merely moved, so it has no replacement entry here.
	for _, file := range []string{
		"help.go",
		filepath.Join("..", "..", "health", "render.go"),
		// `pix setup` is a first-run, high-traffic surface exactly like the
		// three above (finding C13): it was never in this list, so its two em
		// dashes went unguarded until a direct read caught them.
		"setup_cmd.go",
	} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, f := range scanEmDashSource(t, file, src) {
			t.Error(f)
		}
	}
}
