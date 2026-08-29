// reset_env_invariants_test.go — E1.14: `pix reset` invalidates every
// environment host-exec acceptance (AC-69/70, docs/design/environments.md
// D21: "`pix reset` invalidates every environment trust acceptance,
// scaffolded or externally registered, and deletes no environment
// source"), while never touching an environment's SOURCE files.
//
// This is deliberately a WORKFLOW-level test, not a CLI one: `pix env add`/
// `use`/`forget` (E1.10/E1.11) have not landed yet, and this file's whole
// point is to prove the invariant true of the primitives that already exist
// (env.Register, env.Review, env.ComputeShow, env.Load) so it never conflicts
// with those units landing later — see this package's own doc comment
// pattern (env's doc.go) for why a workflow test reaches for the library
// entry points a future CLI verb will call, not a verb that does not exist.
//
// Two registration shapes are load-bearing here, per D21's own wording:
//
//   - an EXTERNAL source: a directory reset never owns or moves at all
//     (outside config/data/state), whose acceptance is invalidated purely
//     because the launcher-owned trust store that recorded it moves away.
//   - a SCAFFOLDED source: a directory living UNDER the Pix data root (what
//     a future `pix env add` with no path would create there), which reset's
//     existing data-root rename carries along for free — same directory
//     entries, same bytes, new parent path.
//
// Neither case needs reset.go to know anything about environments at all:
// the whole design is that hosttrust's environment-trust.json lives beside
// config.toml (workflow/env/review.go's environmentTrustStorePath), and
// reset already renames that whole directory unconditionally. These tests
// exist to make that emergent property a proven, regression-guarded one
// rather than an accident nobody wrote down.
package reset

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/hosttrust"
	"pix/host/workflow/env"
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

// ── the workflow-level round trip ───────────────────────────────────────────

// envHostFixture sets up ONE $HOME the both sides of the invariant agree on:
// the REAL process env vars workflow/env's trust store resolves through
// (config.Path()/config.StateDir() read os.Getenv directly, never an
// injected sys.System), and a resetHost fake whose OWN env map carries the
// byte-identical values, so ResolvePaths(fake) names the SAME three
// directories config's own resolution would. Getting these two out of step
// would silently test two different hosts talking about each other's paths.
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
// cfg (the environments test needs the SAME *config.Config it just
// registered against, not defaultCfg()'s fixed memory-only shape).
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

// tinySbxenv is the minimal Tier1 native document: a secret with a COMMAND
// (restriction 3: a secret command is host execution whether or not it is
// ever bound) is the smallest fixture that forces BillOfMaterials.Tier1(),
// so Review actually gates and records an acceptance rather than the "no
// output, no store write" Tier0 shortcut.
const tinySbxenv = `schemaVersion: "1"
agent: pix

secrets:
  demo:
    command: ["echo", "shh"]
`

func writeEnvSource(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".sbxenv.yaml"), []byte(tinySbxenv), 0o644); err != nil {
		t.Fatal(err)
	}
}

// hashTree hashes every regular file under root, keyed by its root-relative
// path, in sorted order — the same "pure function of path+content" discipline
// bom.go's own hashDir documents, reimplemented here rather than imported
// (workflow/reset may not import workflow/env's internals, and this is a
// three-line hash, not a capability worth a shared package for one test).
func hashTree(t *testing.T, root string) string {
	t.Helper()
	var files []string
	if err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, p)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(files)
	h := sha256.New()
	for _, f := range files {
		rel, _ := filepath.Rel(root, f)
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		h.Write([]byte(rel))
		h.Write([]byte{0})
		h.Write(data)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func acceptTier1(t *testing.T, cfg *config.Config, name string) {
	t.Helper()
	var out bytes.Buffer
	res, err := env.Review(cfg, name, nil, noopLookPath, env.ReviewOptions{Yes: true, Out: &out})
	if err != nil {
		t.Fatalf("Review(%q, --yes): %v\n%s", name, err, out.String())
	}
	if res == nil || !res.Accepted || res.Fingerprint == "" {
		t.Fatalf("Review(%q, --yes) = %+v, want an accepted Tier1 result", name, res)
	}
}

func noopLookPath(string) (string, error) { return "", fmt.Errorf("not found") }

// TestReset_ExternalEnvironmentSource_UntouchedByteIdenticalAcceptanceGone is
// AC-69/70's external-registration half: a source OUTSIDE config/data/state
// is never moved, never modified, and re-registering it after a reset finds
// no acceptance at all — because the trust store that recorded one just
// moved away with the config dir, never because reset walked into the
// environment's own directory.
func TestReset_ExternalEnvironmentSource_UntouchedByteIdenticalAcceptanceGone(t *testing.T) {
	f := newEnvHostFixture(t)
	stubServeProbe(t, false, false)

	// t.TempDir() lands under os.TempDir(), never under f.home — genuinely
	// external to every directory reset knows how to touch.
	root := t.TempDir()
	writeEnvSource(t, root)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if _, err := env.Register(cfg, "extenv", root); err != nil {
		t.Fatalf("Register: %v", err)
	}
	acceptTier1(t, cfg, "extenv")

	before := hashTree(t, root)
	if !exists(filepath.Join(f.configDir, "environment-trust.json")) {
		t.Fatal("acceptance must have written environment-trust.json beside config.toml")
	}

	out, err := f.runReset(t, cfg, NewOpts(false, false, true, false))
	if err != nil {
		t.Fatalf("reset: %v\n%s", err, out)
	}

	// The environment source itself: same path, same bytes, no backup sibling
	// of ITS OWN — reset never touched it at all.
	if !exists(root) {
		t.Fatalf("external environment source %s must survive at the SAME path", root)
	}
	if got := hashTree(t, root); got != before {
		t.Errorf("external environment source content changed: got %s, want %s", got, before)
	}
	if matches, _ := filepath.Glob(root + ".bak-*"); len(matches) != 0 {
		t.Errorf("external environment source must never get its own .bak- sibling, found %v", matches)
	}

	// The config dir (and with it, environment-trust.json) is gone from the
	// live path — this is the WHOLE mechanism: acceptance evaporates because
	// the launcher-owned store moved, not because anyone edited a record.
	if exists(f.configDir) {
		t.Fatal("config dir must have moved aside")
	}
	backupOf(t, f.configDir) // exactly one .bak- sibling

	// Re-registering the SAME external path into a FRESH config (post-reset:
	// config.toml is gone, so a fresh config.Load() has no [environments] at
	// all) finds NO acceptance — env show reports it, and Review refuses,
	// naming itself as the fix, exactly as a first-time environment would.
	cfg2, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load (post-reset): %v", err)
	}
	if len(cfg2.Environments) != 0 {
		t.Fatalf("post-reset config must start with no registrations, got %v", cfg2.Environments)
	}
	if _, err := env.Register(cfg2, "extenv", root); err != nil {
		t.Fatalf("re-Register: %v", err)
	}

	show, err := env.ComputeShow(cfg2, "extenv")
	if err != nil {
		t.Fatalf("ComputeShow: %v", err)
	}
	if show.Accepted {
		t.Error("env show must report the re-registered external source as UNACCEPTED after reset")
	}
	if show.Fingerprint != "" {
		t.Errorf("Fingerprint = %q, want empty for an unaccepted environment", show.Fingerprint)
	}

	var reviewOut bytes.Buffer
	_, reviewErr := env.Review(cfg2, "extenv", nil, noopLookPath, env.ReviewOptions{
		TTY: false, Yes: false, Out: &reviewOut,
	})
	if reviewErr == nil {
		t.Fatal("Review must refuse a non-interactive re-acceptance of a Tier1 environment with no record")
	}
	if got := cli.ExitCode(reviewErr); got != 2 {
		t.Errorf("cli.ExitCode(reviewErr) = %d, want 2 (a refusal, not an operational failure)", got)
	}
	// The retry command lives in the refusal's own three-part text now
	// (C7), never a second, output-only line on reviewOut.
	wantFix := "pix env review extenv --yes"
	if !strings.Contains(reviewErr.Error(), wantFix) {
		t.Errorf("Review's refusal must name itself as the fix (%q); got:\n%s", wantFix, reviewErr.Error())
	}
}

// TestReset_ScaffoldedEnvironmentSource_TravelsWithDataDirRename is AC-69/70's
// scaffolded half: a source living UNDER the Pix data root (what a future
// `pix env add` with no path scaffolds there) moves WITH the data root's
// existing rename — same relative layout, same bytes, at the NEW
// `<data-root>.bak-<ts>/...` path — and the ORIGINAL path is genuinely gone
// (stat fails), never a source Load could still find and falsely trust.
func TestReset_ScaffoldedEnvironmentSource_TravelsWithDataDirRename(t *testing.T) {
	f := newEnvHostFixture(t)
	stubServeProbe(t, false, false)

	root := filepath.Join(f.dataRoot, "environments", "demo")
	writeEnvSource(t, root)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if _, err := env.Register(cfg, "demo", root); err != nil {
		t.Fatalf("Register: %v", err)
	}
	acceptTier1(t, cfg, "demo")
	before := hashTree(t, root)

	out, err := f.runReset(t, cfg, NewOpts(false, false, true, false))
	if err != nil {
		t.Fatalf("reset: %v\n%s", err, out)
	}

	if exists(root) {
		t.Fatalf("scaffolded source %s must not exist at its original path after reset", root)
	}
	if exists(f.dataRoot) {
		t.Fatal("data root must have moved aside")
	}
	dataBackup := backupOf(t, f.dataRoot)
	moved := filepath.Join(dataBackup, "environments", "demo")
	if !exists(moved) {
		t.Fatalf("scaffolded source must travel WITH the data-dir rename to %s", moved)
	}
	if got := hashTree(t, moved); got != before {
		t.Errorf("scaffolded source content changed across the rename: got %s, want %s", got, before)
	}
	// It must not ALSO get an independent .bak- of its own: exactly one move
	// happened (the data root's), never two.
	if matches, _ := filepath.Glob(root + ".bak-*"); len(matches) != 0 {
		t.Errorf("scaffolded source must not get its own separate .bak- sibling, found %v", matches)
	}

	// A caller who re-registers the OLD (now-gone) path must get an honest
	// failure, never a false claim that the original path still holds the
	// environment: Load's required-file check fails on a directory that no
	// longer exists rather than silently succeeding against stale data.
	cfg2, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load (post-reset): %v", err)
	}
	if _, err := env.Register(cfg2, "demo", root); err != nil {
		t.Fatalf("re-Register: %v", err)
	}
	if _, err := env.Load(cfg2, &hosttrust.AcceptanceStore{}, "demo", nil, noopLookPath); err == nil {
		t.Error("Load must refuse a registration whose original scaffolded path no longer exists, not silently succeed")
	}

	// The environment IS still there, byte-identical, one level down in the
	// backup — proving reset's promise ("nothing deleted, only renamed")
	// rather than merely asserting the negative above.
	cfg3, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if _, err := env.Register(cfg3, "demo-recovered", moved); err != nil {
		t.Fatalf("Register the recovered path: %v", err)
	}
	loaded, err := env.Load(cfg3, &hosttrust.AcceptanceStore{}, "demo-recovered", nil, noopLookPath)
	if err != nil {
		t.Fatalf("Load the recovered environment: %v", err)
	}
	if loaded.Accepted {
		t.Error("even the recovered copy must show unaccepted: acceptance never survives a reset")
	}
}

// TestReset_DataBlockedFailure_StillInvalidatesAcceptance_NeverMovesTheSource
// is the "reset failures" case the task calls out: when the daemon cannot be
// confirmed down, the DANGEROUS data-root move (and any scaffolded source
// riding along with it) is refused and left exactly where it was — but the
// config dir is not gated on that at all (see reset.go's plan: only
// Dangerous targets are skipped under dataBlocked), so acceptance is STILL
// invalidated even out of a failed, partial reset. A source is never deleted
// OR silently relocated on a failure path either.
func TestReset_DataBlockedFailure_StillInvalidatesAcceptance_NeverMovesTheSource(t *testing.T) {
	f := newEnvHostFixture(t)
	stubServeProbe(t, true, true) // up before the stop, still up after it: refuses the data move

	root := filepath.Join(f.dataRoot, "environments", "demo")
	writeEnvSource(t, root)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if _, err := env.Register(cfg, "demo", root); err != nil {
		t.Fatalf("Register: %v", err)
	}
	acceptTier1(t, cfg, "demo")
	before := hashTree(t, root)

	_, err = f.runReset(t, cfg, NewOpts(false, false, true, false))
	if err == nil {
		t.Fatal("a reset that cannot confirm the daemon down must report a failure")
	}
	if !strings.Contains(err.Error(), "STILL running") {
		t.Errorf("error = %v, want it to name the still-running daemon", err)
	}

	// The source never moved AND was never deleted: still at its original
	// path, byte-identical.
	if !exists(root) {
		t.Fatal("a blocked data move must leave the scaffolded source exactly where it was")
	}
	if got := hashTree(t, root); got != before {
		t.Errorf("source content changed on a FAILED reset: got %s, want %s", got, before)
	}
	if matches, _ := filepath.Glob(f.dataRoot + ".bak-*"); len(matches) != 0 {
		t.Error("the data root must not have moved when the daemon could not be confirmed down")
	}

	// Acceptance is STILL gone: the config dir move is not gated on the data
	// move's own guard at all.
	if exists(f.configDir) {
		t.Fatal("the config dir must still move aside even when the data move was refused")
	}
	backupOf(t, f.configDir)

	cfg2, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load (post-reset): %v", err)
	}
	if _, err := env.Register(cfg2, "demo", root); err != nil {
		t.Fatalf("re-Register: %v", err)
	}
	loaded, err := env.Load(cfg2, &hosttrust.AcceptanceStore{}, "demo", nil, noopLookPath)
	if err != nil {
		t.Fatalf("Load the still-in-place source: %v", err)
	}
	if loaded.Accepted {
		t.Error("acceptance must be gone even out of a partially-failed reset")
	}
}
