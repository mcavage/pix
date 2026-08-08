package main

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"pix/host/config"
	"pix/host/health"
	"pix/host/workflow/launch"
)

// health_guards_test.go holds the fitness functions for "one readiness truth",
// now that there is only one: renderer purity, the retirement of the old
// model, and the two launch-path properties (a gate that refuses only a
// POSITIVE no-key answer, and warnings that are capped and never block).
//
// It replaces readiness_guards_test.go, whose subject — the Requirement ×
// Verdict model, its lazy axis registry and its second renderer — no longer
// exists.

// glyphOwner is the ONE file allowed to spell a status glyph: the vocabulary
// itself.
const glyphOwner = "health/render.go"

// glyphExempt is out-of-scope, not forgiven, and it may only SHRINK (see
// drainingPackages in arch_test.go for the same mechanism). It is empty, and
// it has nowhere left to shrink to.
var glyphExempt = map[string]bool{}

// statusGlyphs is the closed set of markers the vocabulary owns.
var statusGlyphs = []string{"✓", "✗", "⚠", "⊘"}

// TestRendererPurity: no file that renders health spells a status glyph in a
// STRING LITERAL. AST-scanned, so a glyph in a comment (explaining the
// mapping) is fine and a glyph in output is not. The moment a renderer spells
// its own glyph, two surfaces can disagree about the same fact.
func TestRendererPurity(t *testing.T) {
	root := filepath.Join("..", "..") // the test runs in cmd/pix
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
		rel = filepath.ToSlash(rel)
		if rel == glyphOwner || glyphExempt[rel] {
			return nil
		}
		// In scope = builds or renders the health MODEL, identified by
		// mentioning one of its aggregate types. Merely importing the package
		// is too broad: setup and secret import it for status CONSTANTS while
		// printing their own step-progress ("  ✗ ollama pull failed"), which is
		// not verdict rendering and never was in this guard's scope.
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		src := string(b)
		renders := strings.HasPrefix(rel, "health/")
		for _, ty := range []string{"health.Snapshot", "health.Result", "health.Probe"} {
			if strings.Contains(src, ty) {
				renders = true
			}
		}
		if renders {
			files = append(files, rel)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(files) < 8 {
		t.Fatalf("found only %d health renderers — the walk is broken, not the code", len(files))
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
			for _, g := range statusGlyphs {
				if strings.Contains(value, g) {
					t.Errorf("%s spells the status glyph %q in a string literal (%q) — render it through health.Glyph instead", file, g, value)
				}
			}
			return true
		})
	}
}

// TestGlyphExemptIsShrinking guards the exemption the way
// TestArchitecture_DrainingListIsShrinking guards its own.
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

// TestReadinessModelIsRetired is the end state of the shrink-only consumer
// ratchet readiness_guards_test.go used to carry: the list reached zero, so
// the package went. Nothing in the module — production or test — may import
// it again, and the directory itself must be gone.
func TestReadinessModelIsRetired(t *testing.T) {
	root := filepath.Join("..", "..")
	if _, err := os.Stat(filepath.Join(root, "readiness")); !os.IsNotExist(err) {
		t.Errorf("services/host/readiness still exists (%v); the retired model must not come back", err)
	}
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != root && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if rel == "cmd/pix/health_guards_test.go" {
			return nil // this guard names the import it forbids
		}
		if strings.Contains(string(b), `"pix/host/readiness`) {
			t.Errorf("%s imports the retired readiness model; readiness lives in pix/host/health now", rel)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// keyStore writes a REAL executable that behaves like `sbx secret ls`: it is
// the process the probe actually runs, so the classification under test is the
// production one. No probe seam is faked.
func keyStore(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLaunchGateRefusesOnlyAPositiveNoKey is safety invariant 6: `run` refuses
// to launch ONLY on a POSITIVE "no model key" answer. A key store that is
// missing, broken, hung or refusing is UNKNOWN, and unknown proceeds — a false
// refusal is worse than a failed launch.
//
// This table drives launch.ProbeModelKeysBudget directly, with a generous
// TEST-only budget (testKeyProbeBudget), rather than launch.ProbeModelKeys
// (which always pays the production health.StatusBudget, 2s). Every fixture
// here exits in milliseconds, so the budget value itself proves nothing about
// these cases — but pinning them to the production constant made this
// correctness table hostage to that constant's real-world margin on a loaded
// or slower runner (macOS CI in particular): a subprocess spawn that is merely
// slow, not hung, could read as a timeout and turn an expected ready/absent
// verdict into a flaky unknown. A generous, test-owned budget removes that
// coupling; TestLaunchGateProbeModelKeysUsesTheProductionBudget below is the
// separate, dedicated test that still pins ProbeModelKeys to health.StatusBudget.
func TestLaunchGateRefusesOnlyAPositiveNoKey(t *testing.T) {
	// testKeyProbeBudget is generous on purpose: these fixtures all exit
	// almost instantly, so this only needs to be far larger than any real
	// exec + pipe-read overhead, never a value this table is trying to prove.
	const testKeyProbeBudget = 10 * time.Second
	// testKeyProbeParentDeadline is the test's OWN parent context deadline,
	// deliberately generous and independent of testKeyProbeBudget above: a
	// tight 3s parent used to sit UNDER a 10s budget and silently cap every
	// case's effective timeout to 3s regardless of the constant this table is
	// meant to exercise, so a slow (not hung) subprocess spawn on a loaded
	// runner could still read as a timeout. Sized well above the budget so
	// the budget itself, not this deadline, governs the fixtures below —
	// which all exit in milliseconds anyway.
	const testKeyProbeParentDeadline = 30 * time.Second
	for _, tc := range []struct {
		name       string
		body       string
		wantRefuse bool
		wantStatus health.Status
	}{
		{"listing names a key", "#!/bin/sh\necho anthropic\n", false, health.StatusReady},
		{"listing answers with no model key", "#!/bin/sh\necho github\n", true, health.StatusAbsent},
		{"store fails", "#!/bin/sh\nexit 3\n", false, health.StatusUnknown},
		{"store refuses", "#!/bin/sh\necho 'permission denied' >&2\nexit 1\n", false, health.StatusUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), testKeyProbeParentDeadline)
			defer cancel()
			r := launch.ProbeModelKeysBudget(ctx, testKeyProbeBudget, keyStore(t, "keystore", tc.body), "secret", "ls")
			if got := r.Effective(); got != tc.wantStatus {
				t.Errorf("status = %s, want %s (%+v)", got, tc.wantStatus, r)
			}
			if got := launch.RefusesLaunch(r); got != tc.wantRefuse {
				t.Errorf("RefusesLaunch = %v, want %v (%+v)", got, tc.wantRefuse, r)
			}
		})
	}
}

// TestLaunchGateTimeoutIsUnknownAndNeverRefuses is the hung-store arm of the
// tri-state, split out of the table above on purpose: it is the one case that
// needs a probe to actually time out, and the table's other four cases must
// never be slowed down (or made flaky under load/-race) by sharing a run with
// a real subprocess racing a production-sized deadline. It proves the
// contract with a budget the TEST controls (workflow/launch.ProbeModelKeysBudget),
// not the production 2s (health.StatusBudget) — so this test is fast and
// deterministic on a loaded CI box, while a SEPARATE self-test below still
// proves ProbeModelKeys itself is wired to the real production budget.
func TestLaunchGateTimeoutIsUnknownAndNeverRefuses(t *testing.T) {
	// `exec` so the killed process IS the sleep: a shell that forks keeps the
	// output pipe open past the deadline, which is a fixture bug, not a probe
	// one. The sleep only needs to outlast the tiny test budget below, not any
	// production timing.
	bin := keyStore(t, "keystore", "#!/bin/sh\nexec sleep 5\n")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r := launch.ProbeModelKeysBudget(ctx, 50*time.Millisecond, bin, "secret", "ls")
	if got := r.Effective(); got != health.StatusUnknown {
		t.Errorf("status = %s, want %s (%+v)", got, health.StatusUnknown, r)
	}
	if got := launch.RefusesLaunch(r); got {
		t.Errorf("RefusesLaunch = %v, want false: a timed-out store must never refuse a launch (%+v)", got, r)
	}
}

// TestLaunchGateProbeModelKeysUsesTheProductionBudget pins the ONE thing the
// budget seam above must never drift from: ProbeModelKeys (what `run` actually
// calls) hands health.StatusBudget to the shared implementation, not some
// other constant a future edit could quietly relax. It never has to wait for a
// timeout to prove this — a fast store settles long before either budget
// expires, so the two calls' results are compared directly instead.
func TestLaunchGateProbeModelKeysUsesTheProductionBudget(t *testing.T) {
	bin := keyStore(t, "keystore", "#!/bin/sh\necho anthropic\n")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// Warm the fixture first, and throw the result away. The FIRST exec of a
	// just-written executable pays a one-time cost the second does not (on
	// macOS, Gatekeeper/XProtect inspection plus a cold page-in); under load
	// that cost has exceeded the 2s production budget. Without this, `want`
	// pays it and `got` does not, so the two disagree on a cold-vs-warm
	// artifact rather than on the wiring this test exists to pin -- observed
	// as want=unknown/"probe timed out" (2.001s) vs got=ready (86ms).
	_ = launch.ProbeModelKeysBudget(ctx, health.StatusBudget, bin, "secret", "ls")
	want := launch.ProbeModelKeysBudget(ctx, health.StatusBudget, bin, "secret", "ls")
	got := launch.ProbeModelKeys(ctx, bin, "secret", "ls")
	if got.Effective() != want.Effective() || got.Detail != want.Detail {
		t.Errorf("ProbeModelKeys = %+v, want the same as an explicit health.StatusBudget call = %+v", got, want)
	}
}

// TestLaunchGateEvidenceCarriesARunnableFix: every warning row a launch prints
// states an observation, and every VERIFIED gap carries an exact command.
// Anything unknown carries none — run never guesses a repair for something it
// could not check.
func TestLaunchGateEvidenceCarriesARunnableFix(t *testing.T) {
	fixFirstTokens := map[string]bool{"pix": true, "pix-host": true, "brew": true, "op": true, "sbx": true,
		"gh": true, "ollama": true, "docker": true, "git": true, "launchctl": true, "systemctl": true}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	keys := launch.ProbeModelKeys(ctx, keyStore(t, "keystore", "#!/bin/sh\necho github\n"), "secret", "ls")
	// A memory-enabled config with nothing listening: the second row is a real
	// probe of a real (closed) port, not a fabricated result.
	snap := launch.FastSnapshot(ctx, &config.Config{Services: []string{"memory"}}, keys)
	if len(snap.Results) < 2 {
		t.Fatalf("the fast snapshot must carry the key evidence and the memory probe: %+v", snap.Results)
	}
	for _, r := range snap.Results {
		if r.OK() {
			continue
		}
		if strings.TrimSpace(r.Evidence) == "" {
			t.Errorf("%s is %s with no evidence", r.Name, r.Effective())
		}
		if !r.Missing() {
			if r.Fix != "" {
				t.Errorf("%s could not be checked yet offers the fix %q", r.Name, r.Fix)
			}
			continue
		}
		if strings.TrimSpace(r.Fix) == "" {
			t.Errorf("verified gap %q carries no fix command", r.Name)
			continue
		}
		if first := strings.Fields(r.Fix)[0]; !fixFirstTokens[first] {
			t.Errorf("fix for %q starts with %q, which is not a command: %q", r.Name, first, r.Fix)
		}
	}
}

// TestRunWarningsAreCappedAndNeverBlock (AC-P0-224): `run` prints AT MOST
// launch.WarningLimit rows plus a count, worst first, and rendering them is
// not a gate.
func TestRunWarningsAreCappedAndNeverBlock(t *testing.T) {
	var snap health.Snapshot
	for _, name := range []string{"a", "b", "c", "d"} {
		snap.Results = append(snap.Results, health.Result{Name: name, Status: health.StatusAbsent, Required: true,
			Detail: "nothing listening", Evidence: "dialled and refused", Fix: "pix serve"})
	}
	snap.Results = append(snap.Results, health.Result{Name: "e", Status: health.StatusUnknown,
		Detail: "could not be checked", Evidence: "no vantage point"})
	var out bytes.Buffer
	if total := launch.RenderWarnings(&out, snap, launch.WarningLimit); total != len(snap.Results) {
		t.Errorf("reported %d warnings, want %d", total, len(snap.Results))
	}
	printed := 0
	for _, ln := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "fix:") || strings.Contains(ln, "more: run") {
			continue
		}
		printed++
	}
	if printed != launch.WarningLimit {
		t.Errorf("printed %d warning rows, want at most %d:\n%s", printed, launch.WarningLimit, out.String())
	}
	if !strings.Contains(out.String(), "2 more: run `pix doctor`") {
		t.Errorf("the remainder must be a single count pointing at doctor:\n%s", out.String())
	}
	// Worst first: the unknown row is the one deferred to the count, never a
	// verified gap.
	if strings.Contains(out.String(), " e: ") {
		t.Errorf("an unknown row displaced a verified gap:\n%s", out.String())
	}
}

// TestRunWarningsRenderThroughTheSharedVocabulary: the fast surface spells no
// glyph of its own — each status renders exactly as health.Glyph says.
func TestRunWarningsRenderThroughTheSharedVocabulary(t *testing.T) {
	for _, st := range []health.Status{health.StatusAbsent, health.StatusDenied, health.StatusUnknown} {
		var out bytes.Buffer
		launch.RenderWarnings(&out, health.Snapshot{Results: []health.Result{{
			Name: "providers", Status: st, Detail: "observed", Evidence: "observed"}}}, launch.WarningLimit)
		if want := health.Glyph(st) + " providers: observed"; !strings.Contains(out.String(), want) {
			t.Errorf("%s rendered %q, want %q", st, out.String(), want)
		}
	}
	// A ready snapshot prints nothing at all: run is not a status screen.
	var out bytes.Buffer
	if n := launch.RenderWarnings(&out, health.Snapshot{Results: []health.Result{{Name: "providers", Status: health.StatusReady}}}, launch.WarningLimit); n != 0 || out.Len() != 0 {
		t.Errorf("a ready snapshot printed %d rows: %q", n, out.String())
	}
}
