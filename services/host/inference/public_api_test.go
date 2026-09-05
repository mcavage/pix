package inference

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestPublicAPINeverExposesUnexported is the regression pin for the E3.1
// pre-merge BLOCK: BuildRoster was exported but its second parameter
// ([]runtimeModel) and return type (*runtimeRoster) named types this
// package never exports, so an external caller could not construct an
// argument for it — the function was exported in name only. The sanctioned
// contract is RosterInput flowing through CompileInferenceRuntime /
// SynthesizeInferenceKit (see RosterInput's doc in roster.go); buildRoster
// is now unexported.
//
// This test proves that INVARIANT by parsing every non-test .go file in
// this package (go/ast, no external tooling) and failing if any exported,
// top-level function's PARAMETER list — at any pointer/slice/array/map/
// channel/variadic nesting — names an unexported type declared in this
// same package.
//
// Scope is deliberately parameters, not results. A parameter of an
// unexported package type is a real defect: an outside package can never
// construct an argument for it (RosterInput's own resolved shape is the
// one exception this package intentionally hands across the boundary, and
// it is exported). A RESULT of an unexported type is a different, already
// load-bearing, and unrelated pattern this package uses elsewhere
// (CompileInferenceRuntime returns the unexported runtimeInferenceManifest,
// and every real caller — cmd/pix, this test file's own external-boundary
// sibling — receives it via `:=` and reads only its exported fields,
// exactly like a typed opaque token). Widening this test to flag that too
// would fail the build on a pattern nobody asked to change and this unit
// does not touch; see TestExternalPackage_CompileInferenceRuntimeAndSynthesizeInferenceKit
// in external_boundary_test.go for proof that pattern actually works from
// outside the package.
func TestPublicAPINeverExposesUnexported(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	unexportedTypes := map[string]bool{}
	var parsed []*ast.File
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		parsed = append(parsed, file)
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				if !ts.Name.IsExported() {
					unexportedTypes[ts.Name.Name] = true
				}
			}
		}
	}
	if len(parsed) == 0 {
		t.Fatal("no non-test .go files found in package inference; the glob is broken, not the invariant")
	}

	// referencesUnexported walks an expr (a param field's type) and reports
	// whether it names one of unexportedTypes anywhere inside pointer,
	// slice, array, map key/value, channel elem, or variadic (Ellipsis)
	// nesting. A dotted SelectorExpr (another package's exported type) is
	// never a hit: this package cannot make another package's types
	// unexported.
	var referencesUnexported func(ast.Expr) (string, bool)
	referencesUnexported = func(e ast.Expr) (string, bool) {
		switch t := e.(type) {
		case *ast.Ident:
			if unexportedTypes[t.Name] {
				return t.Name, true
			}
		case *ast.StarExpr:
			return referencesUnexported(t.X)
		case *ast.ArrayType:
			return referencesUnexported(t.Elt)
		case *ast.Ellipsis:
			return referencesUnexported(t.Elt)
		case *ast.MapType:
			if name, hit := referencesUnexported(t.Key); hit {
				return name, true
			}
			return referencesUnexported(t.Value)
		case *ast.ChanType:
			return referencesUnexported(t.Value)
		}
		return "", false
	}

	for _, file := range parsed {
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || !fd.Name.IsExported() {
				continue
			}
			if fd.Type.Params == nil {
				continue
			}
			for _, field := range fd.Type.Params.List {
				if name, hit := referencesUnexported(field.Type); hit {
					t.Errorf("exported func %s takes an unexported-type parameter %q — an external caller cannot construct an argument for it; unexport the function or export a coherent DTO instead", fd.Name.Name, name)
				}
			}
		}
	}
}
