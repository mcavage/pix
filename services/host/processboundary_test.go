package main

// processboundary_test.go enforces the second half of the layering contract
// arch_test.go started. arch_test.go proves imports point DOWN; this proves the
// PROCESS BOUNDARY points down too: only L4 (cmd/pix) may end the process or
// reach for a global stream.
//
//	L4  cmd/pix    owns os.Exit, os.Stdout, os.Stderr
//	L3  workflow/* RETURNS a typed error and writes to an INJECTED writer
//
// The rule is not aesthetic. A workflow that calls os.Exit cannot be tested
// without re-execing the test binary (pack alone had eight such subprocess
// tests, each hiding its assertion behind an exit status), and a workflow that
// writes to os.Stdout pollutes a --json answer from a layer that cannot know
// one was requested.
//
// The allowlist is a RATCHET, not an exemption list: an entry states the exact
// number of writes a package still owes, so the guard fails BOTH when a package
// grows a new one and when a package pays one off without updating the entry.
// The second half is what stops the list from outliving the debt.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// processExits are the os identifiers that END or BYPASS the caller's control
// of the process. os.Stdin is deliberately absent: it is read through the
// prompt seams the interactive flows already thread (in io.Reader, tty bool),
// and pack's remaining two reads are a signature change (its trust gate takes
// the reader from its caller), tracked separately from this guard.
var processExits = map[string]bool{"Exit": true, "Stdout": true, "Stderr": true}

// l3Debt is the remaining, KNOWN process-boundary debt in L3, by package and
// count. Every entry is a promise to pay, with the reason it has not been.
var l3Debt = map[string]int{
	// provision's first-run flow is the ONE composition-root-shaped workflow:
	// FirstRunFlow itself takes (in, out, tty), and this is the wrapper that
	// supplies the process's own three.
	"workflow/provision": 1,
}

func TestL3NeverTouchesTheProcessBoundary(t *testing.T) {
	root := hostModuleRoot(t)
	found := map[string]int{}
	var detail []string
	for _, pkg := range workflowPackages(t, root) {
		for _, hit := range processBoundaryHits(t, filepath.Join(root, pkg)) {
			found[pkg]++
			detail = append(detail, pkg+": "+hit)
		}
	}
	sort.Strings(detail)
	for pkg, n := range found {
		want, ok := l3Debt[pkg]
		if !ok {
			t.Errorf("%s reaches the process boundary %d time(s) — an L3 workflow returns a typed error and writes to an injected writer; only cmd/pix owns os.Exit/os.Stdout/os.Stderr", pkg, n)
			continue
		}
		if n != want {
			t.Errorf("%s: %d process-boundary use(s), l3Debt says %d — %s", pkg, n, want,
				map[bool]string{true: "new debt: thread the writer instead", false: "debt paid: lower the l3Debt entry (or delete it)"}[n > want])
		}
	}
	for pkg := range l3Debt {
		if found[pkg] == 0 {
			t.Errorf("l3Debt lists %s, which is now clean — delete the entry so the list keeps meaning what it says", pkg)
		}
	}
	if t.Failed() {
		t.Logf("process-boundary uses found:\n  %s", strings.Join(detail, "\n  "))
	}
}

// TestPackOwnsNoProcessBoundary is the U08d regression, stated as its own
// named claim rather than as an absence from l3Debt: `pix pack` is the verb
// tree that used to exit the process from L3, and it must stay clean.
func TestPackOwnsNoProcessBoundary(t *testing.T) {
	if _, listed := l3Debt["workflow/pack"]; listed {
		t.Fatal("workflow/pack must never be granted process-boundary debt: its verbs return errors and cmd/pix maps them to exit codes")
	}
	if hits := processBoundaryHits(t, filepath.Join(hostModuleRoot(t), "workflow", "pack")); len(hits) != 0 {
		t.Errorf("workflow/pack reached the process boundary again:\n  %s", strings.Join(hits, "\n  "))
	}
}

// hostModuleRoot resolves the services/host module root. The test binary runs
// in the package directory, which IS the module root for package main.
func hostModuleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("%s does not look like the services/host module root: %v", root, err)
	}
	return root
}

// workflowPackages lists every L3 package (workflow/<name>), so a workflow
// added tomorrow is covered without editing this test.
func workflowPackages(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "workflow"))
	if err != nil {
		t.Fatalf("reading workflow/: %v", err)
	}
	var pkgs []string
	for _, e := range entries {
		if e.IsDir() {
			pkgs = append(pkgs, "workflow/"+e.Name())
		}
	}
	return pkgs
}

// processBoundaryHits parses every non-test .go file in dir and reports each
// os.Exit / os.Stdout / os.Stderr reference as "file:line: os.X". An AST walk
// rather than a grep: a grep counts the word in a comment (this file's own
// prose would trip it) and misses nothing a compiler would see.
func processBoundaryHits(t *testing.T, dir string) []string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", dir, err)
	}
	var hits []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			if !importsOS(file) {
				continue // a local identifier named os cannot be the stdlib package
			}
			ast.Inspect(file, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				ident, ok := sel.X.(*ast.Ident)
				if !ok || ident.Name != "os" || ident.Obj != nil || !processExits[sel.Sel.Name] {
					return true
				}
				pos := fset.Position(sel.Pos())
				hits = append(hits, fmt.Sprintf("%s:%d: os.%s", filepath.Base(pos.Filename), pos.Line, sel.Sel.Name))
				return true
			})
		}
	}
	sort.Strings(hits)
	return hits
}

// importsOS reports whether the file imports "os" under its own name, which is
// what makes `os.Exit` in it the stdlib call rather than a local variable's
// field.
func importsOS(file *ast.File) bool {
	for _, imp := range file.Imports {
		if imp.Path.Value == `"os"` && imp.Name == nil {
			return true
		}
	}
	return false
}
