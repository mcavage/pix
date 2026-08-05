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
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// hostModulePath is this module's import path (see go.mod); it is what turns
// an import string like "pix/host/cli" back into a filesystem directory
// under the module root, the same translation scanPackages does in
// arch_test.go.
const hostModulePath = "pix/host"

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
// package-level `var` whose value is a function literal, or resolves — by
// declaration, not by AST shape — to something function-typed, as
// "file:line: name". Resolution follows identifiers to their declaration
// (same package) and selectors to the imported package's declaration
// (stdlib or another package in this module), so `time.Second` or
// `io.Discard` (data) are never confused with `fmt.Sprintln` or a bare
// reference to a sibling func (a function-typed seam) — see isFuncValueDecl.
func funcValuedGlobalHits(t *testing.T, dir string) []string {
	t.Helper()
	root := hostModuleRoot(t)
	idx := loadPkgIndex(t, dir)
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
				if vs.Type != nil && isFuncTypeExpr(idx, vs.Type, map[string]bool{}) {
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
					if !isFuncValueDecl(t, root, dir, file, val, map[string]bool{}) {
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

// pkgIndex is the declaration table for one package directory: enough to
// answer "is this name function-typed" without re-parsing on every lookup.
type pkgIndex struct {
	funcs   map[string]bool      // top-level `func Name(...)` (no receiver — methods can't be a package var's value)
	consts  map[string]bool      // top-level const names — a const can never hold a func value, full stop
	types   map[string]ast.Expr  // top-level `type Name <expr>` underlying type expressions
	varType map[string]ast.Expr  // package var's declared type, if any (`var Name T`)
	varVal  map[string]ast.Expr  // package var's initializer, if any (`var Name = expr`)
	varFile map[string]*ast.File // the file each var was declared in, so its initializer resolves selectors against the RIGHT import list
}

// pkgIndexCache memoizes loadPkgIndex by directory: the same stdlib package
// (time, io, fmt, ...) or sibling module package is consulted repeatedly
// across an architecture run, and it never changes mid-run.
var pkgIndexCache = map[string]*pkgIndex{}

// loadPkgIndex parses every non-test .go file in dir once and returns its
// declaration table. Unlike funcValuedGlobalHits (which only cares about
// package-level vars), this also records funcs, consts and types, because
// resolving what an identifier or selector POINTS TO needs the whole
// declaration surface, not just the vars.
func loadPkgIndex(t *testing.T, dir string) *pkgIndex {
	t.Helper()
	if idx, ok := pkgIndexCache[dir]; ok {
		return idx
	}
	idx := &pkgIndex{
		funcs:   map[string]bool{},
		consts:  map[string]bool{},
		types:   map[string]ast.Expr{},
		varType: map[string]ast.Expr{},
		varVal:  map[string]ast.Expr{},
		varFile: map[string]*ast.File{},
	}
	pkgIndexCache[dir] = idx
	forEachProdFile(t, dir, func(_ *token.FileSet, file *ast.File) {
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil {
					idx.funcs[d.Name.Name] = true
				}
			case *ast.GenDecl:
				switch d.Tok {
				case token.CONST:
					for _, spec := range d.Specs {
						if vs, ok := spec.(*ast.ValueSpec); ok {
							for _, n := range vs.Names {
								idx.consts[n.Name] = true
							}
						}
					}
				case token.TYPE:
					for _, spec := range d.Specs {
						if ts, ok := spec.(*ast.TypeSpec); ok {
							idx.types[ts.Name.Name] = ts.Type
						}
					}
				case token.VAR:
					for _, spec := range d.Specs {
						vs, ok := spec.(*ast.ValueSpec)
						if !ok {
							continue
						}
						for i, name := range vs.Names {
							idx.varFile[name.Name] = file
							if vs.Type != nil {
								idx.varType[name.Name] = vs.Type
							}
							if i < len(vs.Values) {
								idx.varVal[name.Name] = vs.Values[i]
							}
						}
					}
				}
			}
		}
	})
	return idx
}

// isFuncValueDecl reports whether expr's declared VALUE is function-typed,
// resolved by following the declaration rather than guessing from AST shape:
//
//   - a function literal is always function-valued.
//   - a bare identifier is function-valued iff it resolves (in dir's package)
//     to a top-level func, or to a var whose OWN value/type is
//     function-valued (followed recursively).
//   - a selector (pkg.Name) is function-valued iff pkg resolves to an import
//     in file, that import resolves to a real directory (stdlib, under
//     GOROOT, or a sibling package in this module), and Name resolves there
//     the same way a bare identifier would.
//   - anything else (a call, a composite literal, a basic literal, an
//     unresolvable selector into a third-party module we have no source
//     directory for) is not function-valued: a CallExpr is the RESULT of
//     calling something (regexp.MustCompile(...), errors.New(...)), which is
//     ordinary construction, not a seam.
//
// seen guards against a reference cycle (A points at B points at A), which a
// real dependency-substitution seam never legitimately forms.
func isFuncValueDecl(t *testing.T, root, dir string, file *ast.File, expr ast.Expr, seen map[string]bool) bool {
	t.Helper()
	switch e := expr.(type) {
	case *ast.FuncLit:
		return true
	case *ast.Ident:
		if e.Name == "_" {
			return false
		}
		return identFuncValue(t, root, dir, e.Name, seen)
	case *ast.SelectorExpr:
		xIdent, ok := e.X.(*ast.Ident)
		if !ok {
			return false
		}
		impPath, ok := resolveImportAlias(file, xIdent.Name)
		if !ok {
			return false // xIdent is not a known import in this file
		}
		targetDir, ok := resolveImportDir(root, impPath)
		if !ok {
			return false // a third-party module we have no local source for: don't guess
		}
		return identFuncValue(t, root, targetDir, e.Sel.Name, seen)
	default:
		return false
	}
}

// identFuncValue reports whether name, declared in the package at dir, is
// function-typed.
func identFuncValue(t *testing.T, root, dir, name string, seen map[string]bool) bool {
	t.Helper()
	key := dir + "\x00" + name
	if seen[key] {
		return false
	}
	seen[key] = true
	idx := loadPkgIndex(t, dir)
	switch {
	case idx.funcs[name]:
		return true
	case idx.consts[name]:
		return false // a const can never be function-typed
	}
	if ft, ok := idx.varType[name]; ok {
		return isFuncTypeExpr(idx, ft, map[string]bool{})
	}
	if val, ok := idx.varVal[name]; ok {
		return isFuncValueDecl(t, root, dir, idx.varFile[name], val, seen)
	}
	return false // an import name, a builtin, or something else we don't track
}

// isFuncTypeExpr reports whether a TYPE expression is a function type,
// following a named-type alias (`type Handler func(...)`) back to its
// underlying type within the same package. A named type from another package
// is left unresolved (false): rare enough in practice not to be worth a
// second cross-package hop, and unresolved defaults to "not flagged" like
// everywhere else in this file.
func isFuncTypeExpr(idx *pkgIndex, texpr ast.Expr, seen map[string]bool) bool {
	switch e := texpr.(type) {
	case *ast.FuncType:
		return true
	case *ast.Ident:
		if seen[e.Name] {
			return false
		}
		seen[e.Name] = true
		if under, ok := idx.types[e.Name]; ok {
			return isFuncTypeExpr(idx, under, seen)
		}
		return false
	default:
		return false
	}
}

// resolveImportAlias looks up alias (the identifier a selector's X names) in
// file's import list and returns the import path it refers to.
func resolveImportAlias(file *ast.File, alias string) (string, bool) {
	for _, imp := range file.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if importedName(imp, path) == alias {
			return path, true
		}
	}
	return "", false
}

// importedName is the identifier code in file uses to refer to an import: its
// explicit alias (`foo "some/path"`), or else the conventional last path
// element with a semantic-import-version suffix stripped (`gopkg.in/yaml.v3`
// is referred to as `yaml`, not `yaml.v3`).
func importedName(imp *ast.ImportSpec, path string) string {
	if imp.Name != nil {
		return imp.Name.Name
	}
	base := path
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	if i := strings.LastIndex(base, "."); i >= 0 {
		if n, err := strconv.Atoi(strings.TrimPrefix(base[i+1:], "v")); err == nil && n > 0 {
			base = base[:i]
		}
	}
	return base
}

// resolveImportDir turns an import path into a filesystem directory we can
// parse: a sibling package in this module (under root), or a stdlib package
// (under GOROOT/src). A third-party module import resolves to false — we
// have no guaranteed local source for it, and guessing would reintroduce the
// AST-shape problem this replaces.
func resolveImportDir(root, importPath string) (string, bool) {
	switch {
	case importPath == hostModulePath:
		return root, true
	case strings.HasPrefix(importPath, hostModulePath+"/"):
		return filepath.Join(root, strings.TrimPrefix(importPath, hostModulePath+"/")), true
	}
	dir := filepath.Join(build.Default.GOROOT, "src", importPath)
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return dir, true
	}
	return "", false
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
