package main

// forcerm_guard_test.go is the AST guard for U-r5's rule: sandbox.PlanRemove
// and sandbox.PlanForceRemove (services/host/sandbox/remove.go) are the ONLY
// place in this module that may compose a forced `sbx rm` argv. Every other
// call site must go THROUGH one of those two planners — never hand-roll
// `"rm", "-f"` (or `"--force"`) as literal arguments to a subprocess call,
// because a hand-rolled argv shares neither the pix-* scope check nor the
// name-safety check the planners share (see remove.go's validateScopedName).
// A caller that skips the planner is a caller that could, on a typo or a
// future edit, be handed ANY name at all.
//
// This is a build-time property (a literal string pair appearing together in
// a call's arguments), so an AST walk over CallExpr.Args is the right tool:
// a plain grep for "-f" hits doc comments and unrelated flags (git's
// `worktree remove --force`, `memory restore --force`, …) constantly, and a
// grep for "rm" hits even more. The AST guard narrows to exactly the shape
// that matters: a call whose literal string arguments include "sbx", "rm",
// and either "-f" or "--force" together.
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

// forceRmPlannerFile is the ONE production file allowed to compose the
// literal shape this guard looks for — it IS the planner.
const forceRmPlannerFile = "sandbox/remove.go"

// forceRmFinding is one offending call site, named for the failure message.
type forceRmFinding struct {
	file string
	line int
	argv []string
}

// scanForAdHocForcedSbxRm walks every non-test .go file under root and
// reports each CallExpr whose literal string arguments contain "sbx", "rm",
// and a force flag together — regardless of which function is being called,
// since the point is that NO call anywhere composes this exact argv shape
// outside the planner, not that only env.Run/RunWithin/RunTimed are policed.
func scanForAdHocForcedSbxRm(t *testing.T, root string) []forceRmFinding {
	t.Helper()
	var findings []forceRmFinding
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "testdata" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		if rel == forceRmPlannerFile {
			return nil // the planner itself: this IS the one allowed shape.
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var lits []string
			for _, a := range call.Args {
				lit, ok := a.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, uerr := strconv.Unquote(lit.Value)
				if uerr == nil {
					lits = append(lits, v)
				}
			}
			if hasAdHocForcedRm(lits) {
				pos := fset.Position(call.Pos())
				findings = append(findings, forceRmFinding{file: rel, line: pos.Line, argv: lits})
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return findings
}

func hasAdHocForcedRm(lits []string) bool {
	var sawSbx, sawRm, sawForce bool
	for _, l := range lits {
		switch l {
		case "sbx":
			sawSbx = true
		case "rm":
			sawRm = true
		case "-f", "--force":
			sawForce = true
		}
	}
	return sawSbx && sawRm && sawForce
}

// TestNoAdHocForcedSbxRmOutsidePlanner is the guard itself: it must find
// ZERO occurrences of the sbx+rm+force literal shape anywhere in the module
// except the planner file it deliberately skips.
func TestNoAdHocForcedSbxRmOutsidePlanner(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	findings := scanForAdHocForcedSbxRm(t, root)
	if len(findings) == 0 {
		return
	}
	var b strings.Builder
	b.WriteString("ad-hoc forced `sbx rm` argv found outside sandbox.PlanForceRemove:\n")
	for _, f := range findings {
		b.WriteString("  " + f.file + ":" + strconv.Itoa(f.line) + ": " + strings.Join(f.argv, " ") + "\n")
	}
	b.WriteString("fix: compose the argv with sandbox.PlanForceRemove(name) instead of hand-rolling \"rm\", \"-f\"/\"--force\"\n")
	t.Error(b.String())
}

// TestNoAdHocForcedSbxRmOutsidePlanner_SelfTest proves the guard can actually
// fail: a hand-rolled forced-rm argv planted in a scratch tree must be found.
// A guard nobody has seen fail is not a guard.
func TestNoAdHocForcedSbxRmOutsidePlanner_SelfTest(t *testing.T) {
	tmp := t.TempDir()
	planted := `package planted

func removeIt(run func(string, ...string) (string, error), name string) {
	run("sbx", "rm", "-f", name)
}
`
	if err := os.WriteFile(filepath.Join(tmp, "planted.go"), []byte(planted), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := scanForAdHocForcedSbxRm(t, tmp); len(got) != 1 {
		t.Fatalf("self-test: scanner found %d findings in a planted offender, want 1: %v", len(got), got)
	}
}
