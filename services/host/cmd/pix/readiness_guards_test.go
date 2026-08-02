package main

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"pix/host/hostenv"
	"pix/host/readiness"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

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
var readinessRenderers = []string{
	"doctor_render.go",
	"status.go",
	"run.go",
	"onboard.go",
	"onboard_helpers.go",
	"hoststate.go",
	"gworkspace.go",
	"readiness_launch.go",
	"readiness_service.go",
	"../../readiness/snapshot.go",
	"../../readiness/types.go",
}

// verdictGlyphs is the closed set of markers the vocabulary owns.
var verdictGlyphs = []string{"✓", "✗", "⚠", "⊘"}

// TestRendererPurity (FF2): no readiness renderer contains a verdict glyph in
// a STRING LITERAL. AST-scanned, so a glyph in a comment (explaining the
// mapping) is fine and a glyph in output is not. Ratcheted to zero and pinned
// there: the moment a renderer spells its own glyph, the single vocabulary is
// no longer single.
func TestRendererPurity(t *testing.T) {
	for _, file := range readinessRenderers {
		node, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
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
	if axisReady(s, readiness.AxisPack) {
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

// TestEvidenceAndFixWalk_Doctor walks a full doctor run on a cold host (no
// sbx, no ollama, nothing listening) — the worst case for evidence quality,
// because almost everything is a gap.
func TestEvidenceAndFixWalk_Doctor(t *testing.T) {
	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(name string) (string, error) { return "", errNotFoundFixture }, RunFn: func(string, ...string) (string, error) { return "", errNotFoundFixture }, GetenvFn: func(string) string { return "" }, DialLocalFn: func(int) bool { return false }, IsFileFn: func(string) bool { return false }, HomeDirFn: func() string { return t.TempDir() }}}
	cfg := &config.Config{Services: []string{"memory", "knowledge"}}
	r := runDoctor(cfg, env)
	walkEvidenceAndFix(t, "doctor", r.Snapshot().All())
}

// TestEvidenceAndFixWalk_Fast walks the shared fast snapshot the daily
// surfaces render, on the same cold host.
func TestEvidenceAndFixWalk_Fast(t *testing.T) {
	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(name string) (string, error) { return "/usr/bin/" + name, nil }, RunFn: func(string, ...string) (string, error) { return "", nil }, DialLocalFn: func(int) bool { return false }}}
	cfg := &config.Config{Services: []string{"memory", "knowledge"}}
	walkEvidenceAndFix(t, "fast", fastReadinessSnapshot(cfg, env, probeSbxKeyEvidence(env)).All())
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
	if total := renderReadinessWarnings(&out, s, launchWarningLimit); total != len(rows) {
		t.Errorf("reported %d warnings, want %d", total, len(rows))
	}
	printed := 0
	for _, ln := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "fix:") || strings.Contains(ln, "more: run") {
			continue
		}
		printed++
	}
	if printed != launchWarningLimit {
		t.Errorf("printed %d warning rows, want at most %d:\n%s", printed, launchWarningLimit, out.String())
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
		renderReadinessWarnings(&out, s, launchWarningLimit)
		if !strings.Contains(out.String(), tc.glyph+" axis: "+tc.word) {
			t.Errorf("(%s, %s) rendered %q, want glyph %q + word %q", tc.req, tc.v, out.String(), tc.glyph, tc.word)
		}
	}
}

// TestStatusJSONCarriesChecksAndExit (AC-P0-222): status --json gains the
// shared readiness rows and an `exit` sibling equal to the process exit code,
// ADDITIVELY — the v1 keys keep their names.
func TestStatusJSONCarriesChecksAndExit(t *testing.T) {
	cfg := &config.Config{Services: []string{"memory"}}
	st := gatherStatus(cfg, "default", fakeStatusEnv())
	if len(st.Checks) == 0 {
		t.Fatal("status --json must carry the shared readiness checks array")
	}
	for _, c := range st.Checks {
		if c.Axis == "" {
			t.Errorf("check %q has no axis", c.Label)
		}
	}
	if st.Exit != readiness.ExitReady {
		t.Errorf("exit = %d, want %d (a provider key is set and memory identifies itself)", st.Exit, readiness.ExitReady)
	}
	// Suppressed-3: an unverifiable axis (no sbx, i.e. inside the sandbox)
	// must never fail a script that only wanted the JSON.
	inVM := fakeStatusEnv()
	systest.Of(inVM.System).LookPathFn = func(string) (string, error) { return "", errNotFoundFixture }
	if got := gatherStatus(cfg, "default", inVM).Exit; got != readiness.ExitReady {
		t.Errorf("unverifiable axes must not fail status: exit = %d", got)
	}
	// A POSITIVELY verified core failure still exits 1.
	noKeys := fakeStatusEnv()
	systest.Of(noKeys.System).RunFn = func(name string, args ...string) (string, error) {
		if name == "sbx" && len(args) >= 1 && args[0] == "secret" {
			return "", nil // sbx answered: zero keys set
		}
		return "", nil
	}
	if got := gatherStatus(cfg, "default", noKeys).Exit; got != readiness.ExitNotReady {
		t.Errorf("a verified missing model key must exit %d, got %d", readiness.ExitNotReady, got)
	}
}

// TestStatusProbesSecretsOnce (latency): rendering readiness must not cost a
// second `sbx secret ls`. The snapshot is fed the evidence status already
// paid for, and the whole gather stays well inside a second on a fake host.
func TestStatusProbesSecretsOnce(t *testing.T) {
	env := fakeStatusEnv()
	base := systest.Of(env.System).RunFn
	secretProbes, dials := 0, map[int]int{}
	systest.Of(env.System).RunFn = func(name string, args ...string) (string, error) {
		if name == "sbx" && len(args) >= 2 && args[0] == "secret" && args[1] == "ls" {
			secretProbes++
		}
		return base(name, args...)
	}
	baseDial := systest.Of(env.System).DialLocalFn
	systest.Of(env.System).DialLocalFn = func(port int) bool { dials[port]++; return baseDial(port) }

	start := time.Now()
	gatherStatus(&config.Config{Services: []string{"memory"}}, "default", env)
	if el := time.Since(start); el > time.Second {
		t.Errorf("gatherStatus took %s on a fake host — status must stay fast", el)
	}
	if secretProbes != 1 {
		t.Errorf("`sbx secret ls` ran %d times, want exactly 1 (the snapshot reuses the evidence)", secretProbes)
	}
	for port, n := range dials {
		if n > 1 {
			t.Errorf("port %d was dialed %d times, want at most 1", port, n)
		}
	}
}
