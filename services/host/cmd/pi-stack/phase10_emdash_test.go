package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestGoStringLiteralsHaveNoEmDash is the Phase-10 UX-copy anti-drift guard:
// it parses each reviewer-named CLI source file with go/parser and fails if
// any STRING literal (the user-visible text a runtime/help/error/status
// message is built from) contains an em dash (U+2014). Comments are
// deliberately not checked here — the finding was about copy a user reads,
// not internal engineering notes — so a comment may still say "foo — bar"
// without tripping this guard.
//
// The check unquotes each literal (via strconv.Unquote for interpreted
// strings, with the raw text as a fallback for raw `...` strings) before
// scanning, so an em dash written as the Go escape `\u2014` is caught exactly
// like a literal UTF-8 em dash byte would be; both produce the same rune at
// runtime, and a reviewer re-adding one either way must fail the same way.
func TestGoStringLiteralsHaveNoEmDash(t *testing.T) {
	root := repoRoot(t)
	files := []string{
		"main.go",
		"help.go",
		"doctor.go",
		"status.go",
		"gog_setup.go",
		"setup.go",
		"run.go",
		"pack.go",
		"memory.go",
		"serve_start.go",
		"reset.go",
	}

	for _, name := range files {
		path := filepath.Join(root, "services", "host", "cmd", "pi-stack", name)
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			val, err := strconv.Unquote(lit.Value)
			if err != nil {
				// Not a normal interpreted/raw literal strconv can unquote
				// (shouldn't happen for token.STRING, but fall back to the
				// raw source text rather than silently skipping the check).
				val = lit.Value
			}
			if strings.ContainsRune(val, '\u2014') {
				pos := fset.Position(lit.Pos())
				t.Errorf("%s:%d: string literal contains an em dash (U+2014): %s",
					name, pos.Line, lit.Value)
			}
			return true
		})
	}
}

// TestProseFilesHaveNoEmDash is the doc half of the same guard: the public
// user-facing prose files named in the Phase-10 findings (plus the embedded
// man page, which ships the identical prose to a terminal `man` reader) must
// never contain an em dash (U+2014). Reports the exact line so a regression
// is a one-line fix, not a re-run of the whole sweep.
func TestProseFilesHaveNoEmDash(t *testing.T) {
	root := repoRoot(t)
	files := []string{
		"README.md",
		"SECURITY.md",
		"skills/onboarding/SKILL.md",
		"skills/gworkspace/SKILL.md",
		"docs/gog-setup.md",
		"services/host/cmd/pi-stack/pi-stack.1",
	}

	for _, rel := range files {
		path := filepath.Join(root, rel)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", rel, err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.ContainsRune(line, '\u2014') {
				t.Errorf("%s:%d: contains an em dash (U+2014): %s", rel, i+1, line)
			}
		}
	}
}
