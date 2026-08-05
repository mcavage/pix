// pack_units_test.go — U07d: end-to-end proof of the trusted pack
// [[services]] → supervisor wiring, against REAL parts only: a real pack on
// disk, the real launcher-owned trust store, the real fingerprint code, and a
// real go-plugin fixture binary (supervise/testdata/fixture) launched as a
// supervised external unit over the actual handshake. No mocks.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/plugin"
	"pix/host/sys/systest"
	"pix/host/workflow/pack"
)

var (
	packFixtureOnce sync.Once
	packFixtureBin  string
	packFixtureErr  error
)

// packageDir returns the directory containing this test file, resolved via
// runtime.Caller rather than the process's actual working directory. See
// supervise/supervise_test.go's packageDir for the full rationale: `go test`
// happens to set the process CWD to the package directory, but compileFixture
// pins it explicitly so module resolution doesn't depend on that invariant
// holding. See TestPackFixtureBuildSurvivesCWDChange.
func packageDir() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("pix/host: runtime.Caller failed to resolve package directory")
	}
	return filepath.Dir(file)
}

// compileFixture reads srcRelPath (relative to this package's own directory),
// writes it into a fresh temp dir as main.go, and compiles it there with
// cmd.Dir pinned to packageDir() so resolving "pix/host/plugin" and the
// cached go-plugin dependency never depends on an inherited process CWD.
func compileFixture(srcRelPath string) (string, error) {
	pkgDir := packageDir()
	dir, err := os.MkdirTemp("", "pack-units-fixture")
	if err != nil {
		return "", err
	}
	src, err := os.ReadFile(filepath.Join(pkgDir, srcRelPath))
	if err != nil {
		return "", err
	}
	mainGo := filepath.Join(dir, "main.go")
	if err := os.WriteFile(mainGo, src, 0o644); err != nil {
		return "", err
	}
	out := filepath.Join(dir, "fixture")
	cmd := exec.Command("go", "build", "-o", out, mainGo)
	cmd.Dir = pkgDir // pin module resolution explicitly; do not rely on inherited CWD
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", errors.New(string(b))
	}
	return out, nil
}

// buildPackFixture compiles the supervise test fixture once per test binary —
// the real go-plugin executable a pack would ship. Its source lives at
// supervise/testdata/fixture/main.go.txt — a .txt, not a .go file, on purpose:
// it is a real, complete go-plugin program, but naming it main.go would make
// it a second copy of production Go source outside any *_test.go file,
// indistinguishable from shipped code to a LOC/production-metrics scanner
// (anything that globs *.go and excludes *_test.go, same as `go build ./...`
// itself). Reading it as data and writing it into a temp dir as main.go right
// before the compile keeps this a REAL compiled executable, unchanged from
// before, while its source counts as test fixture data, not production Go.
func buildPackFixture(t *testing.T) string {
	t.Helper()
	packFixtureOnce.Do(func() {
		packFixtureBin, packFixtureErr = compileFixture("supervise/testdata/fixture/main.go.txt")
	})
	if packFixtureErr != nil {
		t.Fatalf("build fixture plugin: %v", packFixtureErr)
	}
	return packFixtureBin
}

// TestPackFixtureBuildSurvivesCWDChange is the regression test for the
// concern that compiling the fixture from an absolute temp-dir path could
// lose module resolution for its "pix/host/plugin" import and its cached
// github.com/hashicorp/go-plugin dependency. It moves the process's working
// directory away from the package directory (something buildPackFixture's
// old, CWD-implicit form would have broken under) and gives itself a
// private, empty GOCACHE (equivalent to a clean cache, but scoped to this
// test — `go clean -cache` is process-global and would race every other
// package's build under a parallel `go test ./...`), then proves
// compileFixture still resolves both offline under GOPROXY=off.
func TestPackFixtureBuildSurvivesCWDChange(t *testing.T) {
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOCACHE", filepath.Join(t.TempDir(), "gocache"))
	t.Chdir(t.TempDir()) // simulate a caller whose process CWD is not this package

	bin, err := compileFixture("supervise/testdata/fixture/main.go.txt")
	if err != nil {
		t.Fatalf("fixture build lost module resolution after a CWD change: %v", err)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("compiled fixture binary missing: %v", err)
	}
}

// writeUnitPack writes a real pack whose [[services]] entry ships bin/fixture
// with the given pinned sha, and returns its root.
func writeUnitPack(t *testing.T, fixtureBin, sha string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(fixtureBin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "fixture"), b, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `name = "unit-pack"
schema = 2

[[services]]
name = "fixture-svc"
runtime = "go-plugin"
activation = "always"
path = "bin/fixture"
sha = "` + sha + `"
license = "MIT"
source = "https://github.com/example/fixture"
`
	if err := os.WriteFile(filepath.Join(root, pack.PackManifestName), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func fileSHA(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// isolatePackState points config + state (trust store, trust lock, supervisor
// stage/reattach dirs) at temp dirs.
func isolatePackState(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
}

// acceptSurface records root's CURRENT host-exec surface in the real trust
// store — the test-side stand-in for saying yes at the Tier-1 gate.
func acceptSurface(t *testing.T, root string) {
	t.Helper()
	p, err := pack.LoadPack(root)
	if err != nil {
		t.Fatal(err)
	}
	fp, _, err := pack.ComputeHostExecFingerprint(root, pack.ComputeHostBoM(p, "", pack.PackLocalMCP()))
	if err != nil {
		t.Fatal(err)
	}
	store := &pack.PackTrustStore{}
	store.RecordAcceptance(store.TrustKey(root), pack.PackTrustRecord{Path: pack.CanonicalizePackRoot(root), Fingerprint: fp})
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
}

func packEnv() hostenv.Env { return hostenv.Env{System: &systest.Fake{}} }

// stageDirOf resolves the supervisor's staging dir under the isolated state.
func stageDirOf(t *testing.T) string {
	t.Helper()
	dir, err := config.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "supervise", "stage")
}

// TestPackUnitWiring_AcceptedRunsRealUnit is the whole U07d path in one run:
// accepted pack → minimal view export → reconcilePackUnits → a REAL external
// go-plugin subprocess is staged (sha re-verified), launched, handshaken,
// health-probed, and dispensed through the returned holder.
func TestPackUnitWiring_AcceptedRunsRealUnit(t *testing.T) {
	isolatePackState(t)
	bin := buildPackFixture(t)
	root := writeUnitPack(t, bin, fileSHA(t, bin))
	acceptSurface(t, root)

	p, err := pack.LoadPack(root)
	if err != nil {
		t.Fatal(err)
	}
	views, err := pack.AcceptedGoPluginServices(p, "", packEnv())
	if err != nil {
		t.Fatalf("AcceptedGoPluginServices: %v", err)
	}
	if len(views) != 1 || views[0].Name != "fixture-svc" {
		t.Fatalf("views = %+v", views)
	}

	sup := &supervisor{}
	t.Cleanup(sup.shutdown)
	holders, err := sup.reconcilePackUnits("", views, func(pack.AcceptedService) string { return "memory" })
	if err != nil {
		t.Fatalf("reconcilePackUnits: %v", err)
	}
	h := holders["fixture-svc"]
	if h == nil {
		t.Fatal("no holder for fixture-svc")
	}
	m, ok := h.Get().(plugin.MemoryStore)
	if !ok || m == nil {
		t.Fatalf("dispensed impl = %T, want a MemoryStore over real rpc", h.Get())
	}
	info, err := m.Health()
	if err != nil || !info.OK {
		t.Fatalf("Health over the pack unit = (%+v, %v)", info, err)
	}
	// The exec'd binary is the supervisor-owned STAGED copy, never the pack's.
	if entries, err := os.ReadDir(stageDirOf(t)); err != nil || len(entries) == 0 {
		t.Errorf("staging dir = (%v, %v), want the verified staged copy", entries, err)
	}
	// Reconcile is idempotent and add-only: a second pass with the same views
	// touches nothing and launches nothing new.
	again, err := sup.reconcilePackUnits("", views, func(pack.AcceptedService) string { return "memory" })
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second reconcile added units: %v", again)
	}
}

// TestPackUnitWiring_RejectedStagesNothing: without acceptance there is no
// view, and therefore NOTHING downstream — no staging dir, no unit, no exec.
// Admission (fingerprint/consent) strictly precedes staging/start.
func TestPackUnitWiring_RejectedStagesNothing(t *testing.T) {
	isolatePackState(t)
	bin := buildPackFixture(t)
	root := writeUnitPack(t, bin, fileSHA(t, bin))
	// no acceptSurface — the gate was never passed

	p, err := pack.LoadPack(root)
	if err != nil {
		t.Fatal(err)
	}
	views, err := pack.AcceptedGoPluginServices(p, "", packEnv())
	if err == nil || views != nil {
		t.Fatalf("unaccepted pack exported %+v (err=%v)", views, err)
	}
	if _, statErr := os.Stat(stageDirOf(t)); !os.IsNotExist(statErr) {
		t.Errorf("staging dir exists before any admission: %v", statErr)
	}
}

// TestPackUnitWiring_TamperedBinaryRefusedAtStart: consent is recorded over
// the DECLARED pin; a binary swapped after acceptance still matches the
// fingerprint (the declaration is unchanged) but MUST be refused at staging —
// the supervisor re-hashes the bytes it stages and never execs a mismatch.
func TestPackUnitWiring_TamperedBinaryRefusedAtStart(t *testing.T) {
	isolatePackState(t)
	bin := buildPackFixture(t)
	root := writeUnitPack(t, bin, fileSHA(t, bin))
	acceptSurface(t, root)

	// Swap the shipped binary AFTER acceptance (the classic consent-to-launch gap).
	if err := os.WriteFile(filepath.Join(root, "bin", "fixture"), []byte("#!/bin/sh\necho pwned\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := pack.LoadPack(root)
	if err != nil {
		t.Fatal(err)
	}
	views, err := pack.AcceptedGoPluginServices(p, "", packEnv())
	if err != nil {
		t.Fatalf("view export (declaration unchanged) should pass the fingerprint: %v", err)
	}
	sup := &supervisor{}
	t.Cleanup(sup.shutdown)
	holders, err := sup.reconcilePackUnits("", views, func(pack.AcceptedService) string { return "memory" })
	if err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("tampered binary launched (err=%v); want a sha256 mismatch refusal", err)
	}
	if len(holders) != 0 {
		t.Errorf("holders = %v, want none for a refused unit", holders)
	}
}

// TestPackUnitSpec_ClosedKindSet: the constructor only ever emits a UnitSpec
// for a registered go-plugin capability — a pack (or integrator typo) can
// never introduce a new dispense kind.
func TestPackUnitSpec_ClosedKindSet(t *testing.T) {
	v := pack.AcceptedService{Name: "x", Path: "/abs/bin", SHA: strings.Repeat("ab", 32)}
	if _, err := packUnitSpec(v, "not-a-kind"); err == nil || !strings.Contains(err.Error(), "unknown plugin kind") {
		t.Fatalf("unknown kind accepted: %v", err)
	}
	if _, err := packUnitSpec(v, ""); err == nil {
		t.Fatal("empty kind accepted")
	}
	spec, err := packUnitSpec(v, "memory")
	if err != nil {
		t.Fatalf("valid view refused: %v", err)
	}
	if spec.SelfExec || spec.Path != "/abs/bin" {
		t.Errorf("spec = %+v, want an external (never self-exec) unit", spec)
	}
}
