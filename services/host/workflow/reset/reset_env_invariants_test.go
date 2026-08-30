// reset_env_invariants_test.go — D21 (docs/design/pix-v2-surface.md §3.8):
// `pix reset` invalidates every environment trust acceptance and deletes no
// environment source. The v1 workflow-level round trip this file used to
// prove that against (env.Register/Review/ComputeShow/Load, a config.toml
// registry entry, and workflow/env/review.go's separate
// environmentTrustStorePath) was removed along with those v1-only APIs in
// the v2 cutover (docs/design/pix-v2-surface.md §3.4: no add/edit/use/
// review/forget, no environment registration database).
//
// D21 holds even more directly under v2's single-root layout than it did
// under v1's two-location one: `pix env trust`'s acceptance record now
// lives at PIX_HOME/state/trust/environments/<name>.json
// (cmd/pix/env_cmd.go's trustRecordPath), strictly INSIDE the one root
// `pix reset` renames wholesale — there is no second, independently-rooted
// trust-store path left to keep in sync with the rename at all. Proving
// that emergent property with a real end-to-end `pix reset` + `pix env
// trust` round trip is a real gap this cutover left open; the AST sentinel
// below (a DIFFERENT, still-live half of this same file) is unaffected and
// stays.
package reset

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pix/host/cli"
	"pix/host/config"
)

// parseGoFile parses a single Go source file into an *ast.File plus the
// *token.FileSet needed to turn a node's Pos into a human line number.
func parseGoFile(path string) (*ast.File, *token.FileSet, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	return file, fset, err
}

// inspectSelectors walks file for every "X.Sel(...)" selector expression,
// reporting X's identifier name (best-effort — "" when the receiver is not a
// bare identifier, e.g. a chained call), Sel's name, and the 1-based source
// line the selector appears on.
func inspectSelectors(file *ast.File, fset *token.FileSet, fn func(pkg, sel string, line int)) {
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg := ""
		if ident, ok := sel.X.(*ast.Ident); ok {
			pkg = ident.Name
		}
		fn(pkg, sel.Sel.Name, fset.Position(sel.Pos()).Line)
		return true
	})
}

// ── AST sentinel: no reset path may ever delete an environment source ──────

// deleteSelectors are call-site method names this sentinel refuses anywhere
// in reset's own source, regardless of the receiver package: os.Remove,
// os.RemoveAll and a bare Unlink cover every shape a future "helpful"
// cleanup could take. reset_test.go's TestReset_NeverDeletes already checks
// this package's own files against os.Remove/os.RemoveAll specifically;
// this sentinel is broader (any receiver, plus Unlink) and ALSO covers the
// command layer (cmd/pix/reset_cmd.go), which is the other place a reset
// path could plausibly grow a delete call.
var deleteSelectors = map[string]bool{"Remove": true, "RemoveAll": true, "Unlink": true}

// scanFileForDeleteCalls parses path and reports every "pkg.Selector(...)"
// call whose Selector is one of deleteSelectors, as "path:line: pkg.Selector"
// strings — pure (no *testing.T) so a planted-violation test can assert it
// actually fires, the same discipline arch_test.go's own sentinels hold
// themselves to.
func scanFileForDeleteCalls(t *testing.T, path string) []string {
	t.Helper()
	var hits []string
	file, fset, err := parseGoFile(path)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	inspectSelectors(file, fset, func(pkg, sel string, line int) {
		if deleteSelectors[sel] {
			hits = append(hits, fmt.Sprintf("%s:%d: %s.%s", path, line, pkg, sel))
		}
	})
	return hits
}

// TestReset_NeverDeletesAnEnvironmentSource is the AST/source sentinel this
// unit adds: it scans every non-test .go file reset actually ships — this
// package's own sources AND the command layer that wires the sandbox sweep
// in (cmd/pix/reset_cmd.go) — for ANY Remove/RemoveAll/Unlink call, on any
// receiver. reset's whole safety property is "rename, never delete"
// (reset.go's own package doc: "There is no remove/removeAll here"); this
// test is what keeps a future edit from quietly growing one back, in either
// file, without anyone having to remember to re-derive it from a comment.
func TestReset_NeverDeletesAnEnvironmentSource(t *testing.T) {
	files := []string{
		"reset.go",
		filepath.Join("..", "..", "cmd", "pix", "reset_cmd.go"),
	}
	for _, f := range files {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("expected %s to exist (path drifted?): %v", f, err)
		}
		for _, hit := range scanFileForDeleteCalls(t, f) {
			t.Errorf("%s: reset must never delete anything — an environment source is renamed with its "+
				"parent (scaffolded) or left untouched (external), never removed", hit)
		}
	}
}

// TestDeleteCallSentinelDetectsAPlantedViolation proves the sentinel above
// actually fires, mirroring arch_test.go's own
// TestForbiddenSymbolSentinelDetectsAPlantedViolation: a passing
// TestReset_NeverDeletesAnEnvironmentSource is only evidence of a clean tree
// if this test proves the checker can fail at all.
func TestDeleteCallSentinelDetectsAPlantedViolation(t *testing.T) {
	dir := t.TempDir()
	planted := filepath.Join(dir, "planted.go")
	src := "package x\n\nimport \"os\"\n\nfunc f(root string) {\n\tos.RemoveAll(root)\n}\n"
	if err := os.WriteFile(planted, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	hits := scanFileForDeleteCalls(t, planted)
	if len(hits) == 0 {
		t.Fatal("expected the planted os.RemoveAll(root) to be caught, but the sentinel found nothing")
	}
}

// ── shared fixture: one $HOME both a real config.Load() and a fake
// resetHost agree on ─────────────────────────────────────────────────────

// envHostFixture sets up ONE $HOME the real process env vars (config.Path()/
// config.StateDir() read os.Getenv directly, never an injected sys.System)
// and a resetHost fake (whose OWN env map carries the byte-identical
// values) both agree on, so ResolvePaths(fake) names the SAME directories
// config's own resolution would. Getting these two out of step would
// silently test two different hosts talking about each other's paths.
//
// This fixture is deliberately kept even though the v1 environment-
// registration tests that first introduced it (env.Register/Review/Load,
// this file's own header comment) were removed in the v2 cutover:
// hmackey_reset_test.go's HMAC-key invalidation/rotation proof needs the
// SAME real-config-dir + fake-resetHost pairing and has no reason to
// duplicate it.
type envHostFixture struct {
	home      string
	configDir string
	dataRoot  string
	stateDir  string
	fake      resetHost
}

func newEnvHostFixture(t *testing.T) *envHostFixture {
	t.Helper()
	home := t.TempDir()
	configDir := filepath.Join(home, ".config", "pix")
	stateHome := filepath.Join(home, ".local", "state")
	t.Setenv("PIX_CONFIG", filepath.Join(configDir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_CONFIG_HOME", "") // never let a developer's real XDG override this fixture
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("MEMORY_DB", "")

	f := &envHostFixture{
		home:      home,
		configDir: configDir,
		dataRoot:  filepath.Join(home, ".local", "share", "pix"),
		stateDir:  filepath.Join(stateHome, "pix"),
	}
	f.fake = resetHost{
		home: home,
		envVars: map[string]string{
			"PIX_CONFIG":      filepath.Join(configDir, "config.toml"),
			"XDG_STATE_HOME":  stateHome,
			"XDG_CONFIG_HOME": "",
			"XDG_DATA_HOME":   "",
		},
		binaries: map[string]bool{},
		output:   map[string]string{},
		ports:    map[int]bool{},
	}
	return f
}

// runReset drives the real verb (plan -> guard -> execute) against f's fake
// host, exactly like stack.run in fixture_test.go, but with an injectable
// cfg.
func (f *envHostFixture) runReset(t *testing.T, cfg *config.Config, opts Opts) (out string, err error) {
	t.Helper()
	var buf, errBuf bytes.Buffer
	rt := Runtime{
		FS:   DefaultResetFS(),
		Env:  f.fake,
		IO:   cli.IO{In: strings.NewReader(""), Out: &buf, IsTTY: false},
		ErrW: &errBuf,
		Sweep: func(io.Writer, io.Writer) error {
			return nil
		},
		Now: func() time.Time { return time.Unix(1700000000, 0) },
	}
	err = Run(cfg, ResolvePaths(f.fake), opts, rt)
	return buf.String(), err
}
