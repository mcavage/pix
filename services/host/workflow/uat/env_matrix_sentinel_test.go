package uat

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"
)

// TestCandidateSmokeNeverCallsLinkedEnvMatrix is the structural sentinel for
// the candidate-owned environment-matrix seam
// (docs/design/self-development-uat.md; host run
// run-20260823-201941-8f7b648b proved the worker still executed its own
// statically linked pre-candidate uatenvmatrix.RunForCandidateSmoke instead
// of the SUBMITTED candidate's own uat-env-matrix binary, even after commit
// 74f74103 fixed fixture materialization). candidate_smoke's env-matrix step
// must reach uatenvmatrix code ONLY by execing the candidate's own
// `<OutDir>/pix-host uat-env-matrix` child process (env_matrix.go); this
// package must never call uatenvmatrix.Run directly again.
// uatenvmatrix.CheckNames() stays legitimate: it is a pure, static name list
// Runner.capabilities reports, never execution.
func TestCandidateSmokeNeverCallsLinkedEnvMatrix(t *testing.T) {
	for _, file := range []string{"execute.go", "env_matrix.go", "runner.go"} {
		for _, v := range linkedEnvMatrixCallSites(t, file) {
			t.Errorf("%s calls uatenvmatrix.%s directly; candidate_smoke must run the SUBMITTED candidate's own uat-env-matrix child process, never the worker-linked matrix (docs/design/self-development-uat.md)", file, v)
		}
	}
}

// TestCandidateSmokeSentinelDetectsAPlantedViolation proves the check above
// actually fires — the same discipline uat_mcp_test.go's identical
// plausibility test applies: a guard only ever seen passing has never been
// proven to catch anything.
func TestCandidateSmokeSentinelDetectsAPlantedViolation(t *testing.T) {
	dir := t.TempDir()
	planted := dir + "/planted.go"
	src := "package uat\n\n" +
		"import (\n\t\"context\"\n\n\t\"pix/host/uatenvmatrix\"\n)\n\n" +
		"func bad(ctx context.Context) error {\n" +
		"\treturn uatenvmatrix.Run(ctx, uatenvmatrix.Inputs{})\n" +
		"}\n"
	if err := os.WriteFile(planted, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	violations := linkedEnvMatrixCallSites(t, planted)
	if len(violations) != 1 || violations[0] != "Run" {
		t.Fatalf("expected exactly one planted Run violation, got %v", violations)
	}
}

// TestCandidateSmokeSentinelAllowsCheckNames proves the sentinel is scoped to
// EXECUTION (uatenvmatrix.Run), not the package generally: runner.go's own
// legitimate uatenvmatrix.CheckNames() call for capabilities reporting must
// never trip this guard.
func TestCandidateSmokeSentinelAllowsCheckNames(t *testing.T) {
	dir := t.TempDir()
	planted := dir + "/planted_checknames.go"
	src := "package uat\n\nimport \"pix/host/uatenvmatrix\"\n\nvar names = uatenvmatrix.CheckNames()\n"
	if err := os.WriteFile(planted, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if violations := linkedEnvMatrixCallSites(t, planted); len(violations) != 0 {
		t.Fatalf("expected CheckNames() to be allowed, got violations: %v", violations)
	}
}

func linkedEnvMatrixCallSites(t *testing.T, file string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	var found []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkgIdent, ok := sel.X.(*ast.Ident)
		if !ok || pkgIdent.Name != "uatenvmatrix" {
			return true
		}
		if sel.Sel.Name == "Run" {
			found = append(found, sel.Sel.Name)
		}
		return true
	})
	return found
}
