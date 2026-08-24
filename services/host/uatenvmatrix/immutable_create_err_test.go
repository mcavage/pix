// immutable_create_err_test.go is a source-level (AST) guard against the
// exact proven bug class fresh UAT run run-20260824-092338-d4c384f5 found in
// checkEnvironmentUsesLocalCandidateImage: the check reused a bare `err`
// variable for a post-create host-command call, and since
// cleanupCreatedFixture is invoked from a `defer func() { ... }()` closure
// that captures its createErr argument BY REFERENCE, a later call's own
// outcome silently overwrote the create's own error by the time the
// deferred cleanup actually ran — misclassifying a successful create as
// receiptless and leaking the fixture.
//
// check_create_exec.go's own createErr comment already names the required
// discipline ("createErr is deliberately its own named variable, never
// reused for the exec call below"); this test enforces it MECHANICALLY,
// across every check in this package that calls cleanupCreatedFixture, so a
// future edit cannot silently reintroduce the same bug under a different
// check's name: whatever identifier a check passes as cleanupCreatedFixture's
// createErr argument, it must never be reassigned anywhere AFTER the
// `defer func() { ... cleanupCreatedFixture(...) ... }()` statement itself —
// reassigning it earlier (e.g. reusing a bare `err` for an EARLIER,
// unrelated call like writeAuthoredFixture, whose result the create call's
// own assignment then immediately overwrites) is not the bug class this
// guards against and is left alone; it is specifically a LATER call's
// outcome silently clobbering the create's own receipt that this test
// exists to make impossible to reintroduce.
//
// This is a source scan, not a full data-flow analysis: it does not descend
// into the defer's own closure body (a reassignment there cannot leak
// outward through the closure boundary the way the proven bug did), and it
// does not account for a nested closure elsewhere that shadows the SAME
// identifier name in its own new scope (none of this package's checks do
// that today). It is intentionally narrow and maintainable rather than a
// general-purpose reassignment checker.
package uatenvmatrix

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// checkFilesCallingCleanupCreatedFixture is every source file in this
// package (excluding tests) whose named check creates a real fixture and
// therefore calls cleanupCreatedFixture from a deferred closure. A file
// added here with a new such check must also be added to this list, or this
// guard silently stops covering it.
var checkFilesCallingCleanupCreatedFixture = []string{
	"check_create_exec.go",
	"check_create_exec_interpolation.go",
	"check_local_image.go",
	"check_custom_agent_ollama.go",
	"check_recreate_boundary.go",
}

// findCleanupDeferAndErrArg locates the `defer func() { ... }()` statement
// inside fn whose body calls cleanupCreatedFixture, and returns that
// DeferStmt plus the identifier name passed as cleanupCreatedFixture's final
// (createErr) argument. It returns (nil, "") if fn never calls it.
func findCleanupDeferAndErrArg(fn *ast.FuncDecl) (*ast.DeferStmt, string) {
	var deferStmt *ast.DeferStmt
	var errIdent string
	ast.Inspect(fn, func(n ast.Node) bool {
		if deferStmt != nil {
			return false
		}
		d, ok := n.(*ast.DeferStmt)
		if !ok {
			return true
		}
		var ident string
		ast.Inspect(d, func(m ast.Node) bool {
			call, ok := m.(*ast.CallExpr)
			if !ok {
				return true
			}
			fnIdent, ok := call.Fun.(*ast.Ident)
			if !ok || fnIdent.Name != "cleanupCreatedFixture" || len(call.Args) == 0 {
				return true
			}
			last := call.Args[len(call.Args)-1]
			if id, ok := last.(*ast.Ident); ok {
				ident = id.Name
			}
			return false
		})
		if ident != "" {
			deferStmt = d
			errIdent = ident
			return false
		}
		return true
	})
	return deferStmt, errIdent
}

// countReassignmentsAfterDefer counts every AssignStmt (both `:=` and `=`)
// in fn's body, positioned strictly AFTER deferStmt, whose left-hand side
// names ident — the exact place a later call's own outcome could silently
// clobber the create call's own captured error before the function
// actually returns and the deferred closure reads it.
//
// This is a deliberately narrow, hand-rolled statement walker rather than a
// blanket ast.Inspect over every AssignStmt, because Go's scoping rules mean
// NOT every same-named AssignStmt is the same variable: `if err := f();
// err != nil { ... }` declares err in a scope implicit to that if statement
// (spec: "each clause ... is considered to be in its own implicit block"),
// so it can never be the outer captured variable even though it reuses the
// name — check_recreate_boundary.go's own drifted-create branch does exactly
// this, legitimately. This walker therefore skips an IfStmt's own Init
// clause (while still recursing into its Body/Else, where a real hazard
// could still hide), and never descends into deferStmt's own closure body: a
// reassignment made there cannot leak outward through the closure the way
// the proven bug did. It does not (yet) need to handle for/switch/select,
// since none of checkFilesCallingCleanupCreatedFixture's functions use one
// between their create call and return; add a case here if one ever does.
func countReassignmentsAfterDefer(fn *ast.FuncDecl, deferStmt *ast.DeferStmt, ident string) int {
	count := 0
	var walkStmt func(ast.Stmt)
	walkStmt = func(s ast.Stmt) {
		switch st := s.(type) {
		case nil:
			return
		case *ast.BlockStmt:
			for _, stmt := range st.List {
				walkStmt(stmt)
			}
		case *ast.AssignStmt:
			if st.Pos() > deferStmt.Pos() {
				for _, lhs := range st.Lhs {
					if id, ok := lhs.(*ast.Ident); ok && id.Name == ident {
						count++
					}
				}
			}
		case *ast.IfStmt:
			// st.Init is intentionally NOT walked: see the func doc above.
			walkStmt(st.Body)
			walkStmt(st.Else)
		case *ast.LabeledStmt:
			walkStmt(st.Stmt)
		case *ast.DeferStmt:
			// Never descend into ANY defer's own closure body (not just the
			// anchor deferStmt): a reassignment made there cannot leak
			// outward through the closure boundary.
			return
		default:
			// ExprStmt, ReturnStmt, DeclStmt, IncDecStmt, and every other
			// statement kind these functions use carry no nested statement
			// list to recurse into.
		}
	}
	walkStmt(fn.Body)
	return count
}

func TestCleanupCreatedFixtureErrArgIsNeverReassignedAfterDefer(t *testing.T) {
	for _, filename := range checkFilesCallingCleanupCreatedFixture {
		t.Run(filename, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, filename, nil, parser.AllErrors)
			if err != nil {
				t.Fatalf("parse %s: %v", filename, err)
			}
			sawCall := false
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				deferStmt, errIdent := findCleanupDeferAndErrArg(fn)
				if deferStmt == nil {
					continue
				}
				sawCall = true
				count := countReassignmentsAfterDefer(fn, deferStmt, errIdent)
				if count != 0 {
					t.Errorf(
						"%s: func %s passes %q to cleanupCreatedFixture, but it is reassigned %d time(s) AFTER the defer statement that captures it by reference \u2014 this is the exact bug class run-20260824-092338-d4c384f5 found: a later call's own outcome silently overwrote the create call's own error by the time deferred cleanup ran, misclassifying a successful create as receiptless",
						filename, fn.Name.Name, errIdent, count,
					)
				}
			}
			if !sawCall {
				t.Fatalf("%s: expected at least one function calling cleanupCreatedFixture; update checkFilesCallingCleanupCreatedFixture if this check no longer creates a real fixture", filename)
			}
		})
	}
}
