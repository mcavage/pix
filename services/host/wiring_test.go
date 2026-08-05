package main

// wiring_test.go is the third half of the layering contract: arch_test.go
// proves imports point down, processboundary_test.go proves the process
// boundary points down, this proves WIRING itself points down — composition
// (init-time side effects, and a package reaching for a real dependency
// through its own package-level var rather than taking one as a parameter)
// belongs to L4, the one layer whose job is argv in, dependency construction,
// dispatch out.
//
//	L4        cmd/pix, "."   may init() and may hold a wiring seam
//	L0/L1/L2/L3               take dependencies as parameters; init() is empty
//
// Both claims are zero-debt today, derived from pkgLayer rather than
// hand-listed, and NOT ratchets: unlike l3Debt (a known debt being paid down),
// there is no known instance of either violation below L4, so an entry here
// would be new debt, not an existing one being tracked.
//
// A narrower rule than "no package-level var" is deliberate: L1 capabilities
// legitimately declare package-level regexes, error sentinels and static
// lookup tables (sys/probeclass.go's deniedPatterns, secret/sync.go's
// providerKeyRefs) — those are DATA, not a substitutable dependency, and
// policing them would just make the guard noisy without catching anything
// real. What this catches is the OTHER shape: a var whose value is a function
// (a literal, or a bare reference to one in another package) — the pattern
// L3/L4 use on purpose to let a test point a real effect (service.Install,
// launcher.FindHostBinary) at a fake, which is a legitimate composition-root
// technique but is not a leaf capability's or a foundation package's to use:
// it takes what it needs as a parameter instead (see workflow/pack/trust.go's
// PackLocalMCP and workflow/provision/setup.go's installLaunchd for what the
// L3-shaped version of this looks like, and why it belongs there, not below).

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

// TestNoWiringInitBelowL4 confirms composition happens in L4 only: no
// foundation, capability, readiness or workflow package runs code at package
// load time. A package that wires something in init() cannot be constructed
// with a fake in its place — the real thing is already live before a test's
// first line runs.
func TestNoWiringInitBelowL4(t *testing.T) {
	root := hostModuleRoot(t)
	var bad []string
	for pkg, layer := range pkgLayer {
		if layer < 0 || layer >= layerCommand {
			continue // L4 (and the unplaced/exempt, of which there are none) owns init
		}
		for _, hit := range initFuncHits(t, filepath.Join(root, pkg)) {
			bad = append(bad, pkg+": "+hit)
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Errorf("package(s) below L4 declare func init() — composition belongs to cmd/pix:\n  %s", strings.Join(bad, "\n  "))
	}
}

// TestNoDependencySeamGlobalsBelowWorkflow confirms L0 (foundation) and L1
// (capability) never hold a package-level, function-valued global: the
// pattern that lets a test substitute a real effect for a fake by reassigning
// a package var. That technique is reserved for L3/L4 (see the file comment);
// a leaf capability or a foundation package takes the function as a
// parameter, so its zero-value is a compile error, not a silently-live real
// dependency.
func TestNoDependencySeamGlobalsBelowWorkflow(t *testing.T) {
	root := hostModuleRoot(t)
	var bad []string
	for pkg, layer := range pkgLayer {
		if layer != layerFoundation && layer != layerCapability {
			continue
		}
		for _, hit := range funcValuedGlobalHits(t, filepath.Join(root, pkg)) {
			bad = append(bad, pkg+": "+hit)
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Errorf("foundation/capability package(s) hold a function-valued package var — thread it "+
			"through a parameter instead; that seam is an L3/L4 technique, not a leaf's:\n  %s", strings.Join(bad, "\n  "))
	}
}

// initFuncHits parses every non-test .go file in dir and reports each
// package-level `func init()` as "file:line".
func initFuncHits(t *testing.T, dir string) []string {
	t.Helper()
	var hits []string
	forEachProdFile(t, dir, func(fset *token.FileSet, file *ast.File) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name.Name != "init" {
				continue
			}
			pos := fset.Position(fn.Pos())
			hits = append(hits, fmt.Sprintf("%s:%d", filepath.Base(pos.Filename), pos.Line))
		}
	})
	return hits
}

// funcValuedGlobalHits parses every non-test .go file in dir and reports each
// package-level `var` whose value is a function literal or a bare reference to
// one (no call) as "file:line: name".
func funcValuedGlobalHits(t *testing.T, dir string) []string {
	t.Helper()
	var hits []string
	forEachProdFile(t, dir, func(fset *token.FileSet, file *ast.File) {
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				if _, isFuncType := vs.Type.(*ast.FuncType); isFuncType {
					for _, name := range vs.Names {
						pos := fset.Position(name.Pos())
						hits = append(hits, fmt.Sprintf("%s:%d: %s", filepath.Base(pos.Filename), pos.Line, name.Name))
					}
					continue
				}
				for i, val := range vs.Values {
					if i >= len(vs.Names) {
						break
					}
					if !isFuncValue(val) {
						continue
					}
					pos := fset.Position(vs.Names[i].Pos())
					hits = append(hits, fmt.Sprintf("%s:%d: %s", filepath.Base(pos.Filename), pos.Line, vs.Names[i].Name))
				}
			}
		}
	})
	return hits
}

// isFuncValue reports whether expr is a function literal, or a bare (uncalled)
// reference to another package's exported function — the two shapes a
// dependency-substitution seam takes. A CallExpr (the value is the RESULT of
// calling something) is deliberately not flagged: that is ordinary
// construction, e.g. regexp.MustCompile(...) or a map/slice literal.
func isFuncValue(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.FuncLit:
		return true
	case *ast.SelectorExpr:
		_, ok := e.X.(*ast.Ident)
		return ok
	default:
		return false
	}
}

// forEachProdFile parses every non-test .go file in dir (skipping a dir with
// none) and invokes fn once per file.
func forEachProdFile(t *testing.T, dir string, fn func(*token.FileSet, *ast.File)) {
	t.Helper()
	if _, err := os.Stat(dir); err != nil {
		return // an unplaced-package failure in TestArchitecture_ImportsPointDown covers this
	}
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", dir, err)
	}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			fn(fset, file)
		}
	}
}
