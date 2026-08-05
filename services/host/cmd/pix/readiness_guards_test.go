package main

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"pix/host/hostenv"
	"pix/host/readiness"
	"pix/host/readiness/axis"
	"sort"
	"strconv"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/sys/systest"
)

// readiness_guards_test.go holds the fitness functions for Wave 1's "one
// readiness truth": renderer purity (FF2), the frozen axis set (FF3), and the
// evidence/fix walk (the checks a reviewer cannot make by reading).

// errNotFoundFixture is the "this binary is not installed here" error the
// cold-host fixtures below return from lookPath.
var errNotFoundFixture = errors.New("not found")

// readinessRenderers is every file that PRINTS readiness to a user. They may
// not spell a verdict glyph themselves: the mapping lives in exactly one place
// (readiness_render.go) or two commands start disagreeing about the same fact.
// glyphOwner is the ONE file allowed to spell a verdict glyph: the vocabulary
// itself.
const glyphOwner = "readiness/render.go"

// glyphExempt is out-of-scope, not forgiven. setup.go both BUILDS a readiness
// report (so it matches the in-scope test below) and prints its own
// step-progress lines — "  ✗ could not write ref to op-refs.env" — which are
// not verdicts about a requirement and were never what this guard is for. The
// hand-written file list this test used to carry did not include setup.go
// either; deriving the scope newly catches it.
//
// The list may only SHRINK, like drainingPackages in arch_test.go. Routing
// those progress lines through the shared vocabulary would be a real
// improvement; adding a second entry should require arguing for it.
// W5/U10b deleted workflow/setup entirely (the provision loop replaced it),
// so the one entry that was here is gone and the list is now EMPTY. It may
// only shrink, and it has nowhere left to shrink to.
var glyphExempt = map[string]bool{}

// verdictGlyphs is the closed set of markers the vocabulary owns.
var verdictGlyphs = []string{"✓", "✗", "⚠", "⊘"}

// TestRendererPurity (FF2): no readiness renderer contains a verdict glyph in
// a STRING LITERAL. AST-scanned, so a glyph in a comment (explaining the
// mapping) is fine and a glyph in output is not. Ratcheted to zero and pinned
// there: the moment a renderer spells its own glyph, the single vocabulary is
// no longer single.
func TestRendererPurity(t *testing.T) {
	root := filepath.Join("..", "..") // the test runs in cmd/pix

	// A "readiness renderer" is any production file that IMPORTS the readiness
	// package — derived, not listed. The previous version named eleven files by
	// hand and so silently stopped covering onboard.go, status.go and run.go as
	// those moved into packages. Widening it to the whole module was worse: it
	// caught sixty-eight glyphs in setup/reset/secret PROGRESS output, which was
	// never what this guards. The import is the honest definition of in-scope.
	var files []string
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != root && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if rel == glyphOwner || glyphExempt[rel] {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		// In scope = builds or renders the readiness MODEL, identified by
		// mentioning one of its aggregate types. Importing the package is too
		// broad: setup and setup_models import it for verdict CONSTANTS while
		// printing their own step-progress ("  ✗ ollama pull failed"), which is
		// not verdict rendering and never was in this guard's scope.
		src := string(b)
		renders := strings.HasPrefix(rel, "readiness/")
		for _, t := range []string{"readiness.Group", "readiness.Report", "readiness.Snapshot"} {
			if strings.Contains(src, t) {
				renders = true
			}
		}
		if !renders {
			return nil
		}
		files = append(files, rel)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(files) < 8 {
		t.Fatalf("found only %d readiness renderers — the walk is broken, not the code", len(files))
	}
	for _, file := range files {
		node, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, file), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		ast.Inspect(node, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			for _, g := range verdictGlyphs {
				if strings.Contains(value, g) {
					t.Errorf("%s spells the verdict glyph %q in a string literal (%q) — render it through readiness.VerdictGlyph/readiness.Glyph instead", file, g, value)
				}
			}
			return true
		})
	}
}

// TestGlyphExemptIsShrinking guards the exemption the way
// TestArchitecture_DrainingListIsShrinking guards its own: a list of "allowed
// to break the rule" stays honest only while growing it is deliberate.
func TestGlyphExemptIsShrinking(t *testing.T) {
	if len(glyphExempt) > 0 {
		t.Errorf("glyphExempt has %d entries; it may only shrink, and it is empty", len(glyphExempt))
	}
}

// TestRendererPuritySelfTest proves the scanner above can actually FAIL: a
// guard nobody has seen fail is a guard nobody should trust.
func TestRendererPuritySelfTest(t *testing.T) {
	const src = `package main
func render() string { return "✓ ready" }`
	node, err := parser.ParseFile(token.NewFileSet(), "fake.go", src, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	found := false
	ast.Inspect(node, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if v, err := strconv.Unquote(lit.Value); err == nil && strings.Contains(v, "✓") {
			found = true
		}
		return true
	})
	if !found {
		t.Fatal("the purity scan failed to flag a literal glyph — it is not testing anything")
	}
}

// TestAxisSetFrozen (FF3): the axis set is a product decision recorded in the
// PRD, not a commit. Adding one fails here on purpose; the fix is to update
// the PRD and this list together.
func TestAxisSetFrozen(t *testing.T) {
	want := []string{
		"gworkspace",
		"model.bridge",
		"model.embed",
		"model.watcher",
		"ollama.host",
		"ollama.sandbox",
		"pack",
		"providers",
		"sbx",
		"secrets",
		"service.knowledge",
		"service.memory",
	}
	got := readiness.AxisNames(readiness.AllAxes)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("frozen axis set changed:\n got %v\nwant %v", got, want)
	}
	// The one parameterized family, and nothing else, is accepted beyond the list.
	if !readiness.MCPAxis("slack").Known() {
		t.Error("mcp:<server> must be a known axis")
	}
	if readiness.Axis("mcp:").Known() || readiness.Axis("invented").Known() {
		t.Error("an unknown axis must never be known")
	}
}

// TestUnrequestedAxisIsAbsent: an axis nobody asked for is ABSENT, never
// rendered ready and never rendered as a verdict. This is what keeps the fast
// surfaces honest about what they did not check.
func TestUnrequestedAxisIsAbsent(t *testing.T) {
	s := readiness.Build(readiness.Request{Axes: []readiness.Axis{readiness.AxisProviders}},
		map[readiness.Axis]readiness.AxisBuilder{
			readiness.AxisProviders: func() []readiness.Check {
				return []readiness.Check{{Label: "k", Requirement: readiness.RequirementCore, Verdict: readiness.VerdictReady, Evidence: "e"}}
			},
			readiness.AxisPack: func() []readiness.Check { t.Fatal("an unrequested axis builder must never run"); return nil },
		},
	)
	if s.Has(readiness.AxisPack) {
		t.Error("an unrequested axis must be absent from the snapshot")
	}
	if _, _, ok := s.AxisVerdict(readiness.AxisPack); ok {
		t.Error("an absent axis must not report a readiness.Verdict")
	}
	if axis.AxisReady(s, readiness.AxisPack) {
		t.Error("an absent axis must never read as ready")
	}
}

// fixFirstTokens is the closed set of commands a fix line may start with. A
// fix beginning with anything else is prose, not a command.
var fixFirstTokens = map[string]bool{
	"pix": true, "pix-host": true, "brew": true, "op": true, "sbx": true,
	"gh": true, "ollama": true, "docker": true, "gcloud": true, "git": true,
	"curl": true, "export": true, "launchctl": true, "systemctl": true,
}

// walkEvidenceAndFix asserts the two properties a reader depends on: every
// non-ready check STATES AN OBSERVATION, and every verified failure carries an
// exact, runnable repair.
func walkEvidenceAndFix(t *testing.T, where string, checks []readiness.Check) {
	t.Helper()
	for _, c := range checks {
		if c.Result() == readiness.VerdictReady {
			continue
		}
		if strings.TrimSpace(c.EvidenceString()) == "" {
			t.Errorf("%s: check %q is %s with no evidence", where, c.Label, c.Result())
		}
		if strings.Contains(c.EvidenceString(), "...") {
			t.Errorf("%s: check %q elides its evidence with \"...\": %q", where, c.Label, c.EvidenceString())
		}
		if c.Note {
			continue // a note asserts nothing, so it owes no repair
		}
		if v := c.Result(); v != readiness.VerdictTodo && v != readiness.VerdictDenied {
			continue
		}
		if strings.TrimSpace(c.Todo) == "" {
			t.Errorf("%s: verified failure %q carries no fix command", where, c.Label)
			continue
		}
		first := strings.Fields(c.Todo)[0]
		if !fixFirstTokens[first] {
			t.Errorf("%s: fix for %q starts with %q, which is not a command: %q", where, c.Label, first, c.Todo)
		}
	}
}

// TestEvidenceAndFixWalk_Fast walks the shared fast snapshot the daily
// surfaces render, on the same cold host.
func TestEvidenceAndFixWalk_Fast(t *testing.T) {
	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(name string) (string, error) { return "/usr/bin/" + name, nil }, RunFn: func(string, ...string) (string, error) { return "", nil }, DialLocalFn: func(int) bool { return false }}}
	cfg := &config.Config{Services: []string{"memory", "knowledge"}}
	walkEvidenceAndFix(t, "fast", axis.FastReadinessSnapshot(cfg, env, axis.ProbeSbxKeyEvidence(env)).All())
}

// TestRunWarningsAreCappedAndNeverBlock (AC-P0-224): `run` prints AT MOST
// three readiness rows plus a count, and rendering them is not a gate.
func TestRunWarningsAreCappedAndNeverBlock(t *testing.T) {
	var rows []readiness.Check
	for _, label := range []string{"a", "b", "c", "d", "e"} {
		rows = append(rows, readiness.Check{Label: label, Requirement: readiness.RequirementCore, Verdict: readiness.VerdictTodo,
			Evidence: "nothing listening", Todo: "pix serve"})
	}
	s := readiness.Build(readiness.Request{Axes: []readiness.Axis{readiness.AxisProviders}}, map[readiness.Axis]readiness.AxisBuilder{
		readiness.AxisProviders: func() []readiness.Check { return rows },
	})
	var out bytes.Buffer
	if total := axis.RenderReadinessWarnings(&out, s, axis.LaunchWarningLimit); total != len(rows) {
		t.Errorf("reported %d warnings, want %d", total, len(rows))
	}
	printed := 0
	for _, ln := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "fix:") || strings.Contains(ln, "more: run") {
			continue
		}
		printed++
	}
	if printed != axis.LaunchWarningLimit {
		t.Errorf("printed %d warning rows, want at most %d:\n%s", printed, axis.LaunchWarningLimit, out.String())
	}
	if !strings.Contains(out.String(), "2 more: run `pix doctor`") {
		t.Errorf("the remainder must be a single count pointing at doctor:\n%s", out.String())
	}
}

// TestFastSurfacesShareTheVocabulary (FF1, lite): one fabricated snapshot,
// rendered by the fast renderer, uses the same glyph and word the vocabulary
// defines for each (requirement, verdict) pair.
func TestFastSurfacesShareTheVocabulary(t *testing.T) {
	for _, tc := range []struct {
		req         readiness.Requirement
		v           readiness.Verdict
		glyph, word string
	}{
		{readiness.RequirementCore, readiness.VerdictTodo, "✗", "needs setup"},
		{readiness.RequirementOptional, readiness.VerdictTodo, "⚠", "needs setup"},
		{readiness.RequirementCore, readiness.VerdictUnverifiable, "?", "can't check from here"},
		{readiness.RequirementCore, readiness.VerdictDenied, "⊘", "blocked"},
	} {
		c := readiness.Check{Label: "axis", Requirement: tc.req, Verdict: tc.v, Evidence: "observed", Todo: "pix doctor"}
		s := readiness.Build(readiness.Request{Axes: []readiness.Axis{readiness.AxisProviders}}, map[readiness.Axis]readiness.AxisBuilder{
			readiness.AxisProviders: func() []readiness.Check { return []readiness.Check{c} },
		})
		var out bytes.Buffer
		axis.RenderReadinessWarnings(&out, s, axis.LaunchWarningLimit)
		if !strings.Contains(out.String(), tc.glyph+" axis: "+tc.word) {
			t.Errorf("(%s, %s) rendered %q, want glyph %q + word %q", tc.req, tc.v, out.String(), tc.glyph, tc.word)
		}
	}
}

// readinessConsumers is every package outside readiness/ that still imports
// it. The readiness model is COMPATIBILITY-ONLY (see the package doc): health
// replaced it for `status`/`doctor`, and this list is the honest, enforced
// record of what is left rather than a claim that the migration finished.
//
// It may only SHRINK, exactly like glyphExempt and drainingPackages. Adding a
// consumer means writing new code against a retired model; the fix is to use
// pix/host/health. When the list is empty, delete the package.
var readinessConsumers = map[string]bool{
	"cmd/pix":         true, // ResolveSessionModel + the sbx key-evidence probe
	"workflow/doctor": true, // the leaf helpers the three below call
	"workflow/launch": true,
	"workflow/models": true,
}

// TestReadinessConsumersOnlyShrink walks the module for PRODUCTION files that
// import the readiness model and asserts the set of owning packages is
// exactly readinessConsumers — no more (a new consumer is a regression) and
// no stale entry (a package that finished migrating must be struck off, or
// the list stops describing anything).
func TestReadinessConsumersOnlyShrink(t *testing.T) {
	root := filepath.Join("..", "..")
	found := map[string]bool{}
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != root && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "readiness/") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		src := string(b)
		if strings.Contains(src, `"pix/host/readiness"`) || strings.Contains(src, `"pix/host/readiness/axis"`) {
			found[filepath.ToSlash(filepath.Dir(rel))] = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for pkg := range found {
		if !readinessConsumers[pkg] {
			t.Errorf("%s imports the COMPATIBILITY-ONLY readiness model; new code must use pix/host/health", pkg)
		}
	}
	for pkg := range readinessConsumers {
		if !found[pkg] {
			t.Errorf("%s no longer imports readiness — strike it off readinessConsumers (the list must stay true)", pkg)
		}
	}
}

// TestCoreSurfacesAreOffReadiness is the positive half of the U10c claim: the
// files that BUILD and RENDER `pix status` and `pix doctor` import health and
// nothing from the retired model. The leaf helpers in the same package (gog,
// ollama, providers) still do, which is why the package-level list above
// still names workflow/doctor — and why this test names files, not packages.
func TestCoreSurfacesAreOffReadiness(t *testing.T) {
	for _, rel := range []string{
		"workflow/doctor/doctor.go", "workflow/doctor/status.go", "workflow/doctor/probes.go",
		"workflow/doctor/json.go", "workflow/doctor/render.go", "workflow/doctor/mcp.go",
		"health/health.go", "health/probes.go", "health/mcp.go", "health/render.go",
		"cmd/pix/doctor_cmd.go",
	} {
		b, err := os.ReadFile(filepath.Join("..", "..", rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if strings.Contains(string(b), `"pix/host/readiness`) {
			t.Errorf("%s still imports the retired readiness model", rel)
		}
	}
}
