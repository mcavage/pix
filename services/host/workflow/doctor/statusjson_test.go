// statusjson_test.go — moved from cmd/pix: the subject is GatherStatus and the
// status JSON shape, both of which live here now.
package doctor

import (
	"errors"
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
	"pix/host/readiness"
	"pix/host/sys/systest"
)

// TestStatusJSONCarriesChecksAndExit (AC-P0-222): status --json gains the
// shared readiness rows and an `exit` sibling equal to the process exit code,
// ADDITIVELY — the v1 keys keep their names.
func TestStatusJSONCarriesChecksAndExit(t *testing.T) {
	cfg := &config.Config{Services: []string{"memory"}}
	st := GatherStatus(cfg, "default", fakeStatusEnv())
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
	if got := GatherStatus(cfg, "default", inVM).Exit; got != readiness.ExitReady {
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
	if got := GatherStatus(cfg, "default", noKeys).Exit; got != readiness.ExitNotReady {
		t.Errorf("a verified missing model key must exit %d, got %d", readiness.ExitNotReady, got)
	}
}

// TestStatusProbesSecretsOnce (latency): rendering readiness must not cost a
// second `sbx secret ls`. The snapshot is fed the evidence status already
// paid for, and the whole gather stays well inside a second on a fake host.

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
	GatherStatus(&config.Config{Services: []string{"memory"}}, "default", env)
	if el := time.Since(start); el > time.Second {
		t.Errorf("GatherStatus took %s on a fake host — status must stay fast", el)
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
var glyphExempt = map[string]bool{"cmd/pix/setup.go": true}

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
