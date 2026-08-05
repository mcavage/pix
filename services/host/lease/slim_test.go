//go:build unix

// slim_test.go — the guard on this package's surface. lease is a foundation
// package: every export is a primitive some caller must be unable to get wrong,
// so an export with no caller is not a spare part, it is a second way to do a
// thing that already has one right way.
package lease

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// retiredSurface is what U04f deleted, and why:
//
//	Lease, Open            — pre-reshard compatibility aliases for RefLease /
//	                         OpenRefLease. Nothing spelled it the old way.
//	AttachRef              — AttachRefUnderLifecycle(ctx, dir, nil). One
//	                         ordering, one entry point.
//	WithLifecycle          — an exported "hold the lifecycle lock and run fn"
//	                         with zero production callers. The two real
//	                         compositions (attach, reap proof) are their own
//	                         functions.
//	ClearKeep              — no caller: a proven teardown clears keep.json
//	                         through ClearState, and nothing else releases a
//	                         keep.
//	fcntlGetFD             — a test helper that lived in production and carried
//	                         a comment claiming production could use it.
var retiredSurface = []string{"Lease", "Open", "AttachRef", "WithLifecycle", "ClearKeep", "fcntlGetFD"}

func TestLease_RetiredSurfaceStaysGone(t *testing.T) {
	banned := map[string]bool{}
	for _, n := range retiredSurface {
		banned[n] = true
	}
	for file, f := range parseProductionFiles(t) {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil && banned[d.Name.Name] {
					t.Errorf("%s: %s is back; see retiredSurface for why it went", file, d.Name.Name)
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if banned[s.Name.Name] {
							t.Errorf("%s: type %s is back; see retiredSurface", file, s.Name.Name)
						}
					case *ast.ValueSpec:
						for _, id := range s.Names {
							if banned[id.Name] {
								t.Errorf("%s: %s is back; see retiredSurface", file, id.Name)
							}
						}
					}
				}
			}
		}
	}
}

// TestLease_RefLeaseHasNoBlockingExclusive is the one method removal worth its
// own assertion. A blocking EXCLUSIVE acquire on refs.lock waits behind live
// shared holders, which is never what a reaper wants and is exactly the misuse
// TryExclusive/TryReapProof exist to prevent; the deadline-bounded blocking
// acquire that IS wanted lives on LifecycleLock, where the lock is EX-only.
func TestLease_RefLeaseHasNoBlockingExclusive(t *testing.T) {
	for file, f := range parseProductionFiles(t) {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Name.Name != "AcquireExclusive" {
				continue
			}
			if recv := receiverTypeName(fn); recv == "RefLease" {
				t.Errorf("%s: RefLease.AcquireExclusive is back — a blocking EX on refs.lock waits behind live references", file)
			}
		}
	}
}

func receiverTypeName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	t := fn.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

func parseProductionFiles(t *testing.T) map[string]*ast.File {
	t.Helper()
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, name := range matches {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		files[name] = f
	}
	if len(files) == 0 {
		t.Fatal("no production files parsed — the guard would pass vacuously")
	}
	return files
}
