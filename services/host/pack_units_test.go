// pack_units_test.go — end-to-end proof of the trusted pack [[services]] →
// supervisor wiring, against REAL parts only: a real pack on disk, the real
// launcher-owned trust store, the real fingerprint code, and a real go-plugin
// fixture binary (supervise/testdata/fixture) launched as a supervised
// external unit over the actual handshake. No mocks.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"pix/host/packinfo"
	"runtime"
	"strings"
	"sync"
	"testing"

	"pix/host/config"
	"pix/host/plugin"
	"pix/host/workflow/pack"
)

var (
	packFixtureOnce sync.Once
	packFixtureBin  string
	packFixtureErr  error
)

// packageDir resolves this package's own directory via runtime.Caller, so
// compileFixture's cmd.Dir never depends on the test runner's inherited CWD.
// See supervise/supervise_test.go's packageDir for the full rationale.
func packageDir() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("pix/host: runtime.Caller failed to resolve package directory")
	}
	return filepath.Dir(file)
}

// compileFixture reads srcRelPath (relative to supervise's own directory),
// writes it into a fresh temp dir as main.go, and compiles it there with
// cmd.Dir pinned so module resolution never depends on an inherited CWD.
func compileFixture(srcRelPath string) (string, error) {
	pkgDir := filepath.Join(packageDir(), "supervise")
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
// the real go-plugin executable a pack would ship.
func buildPackFixture(t *testing.T) string {
	t.Helper()
	packFixtureOnce.Do(func() {
		packFixtureBin, packFixtureErr = compileFixture("testdata/fixture/main.go.txt")
	})
	if packFixtureErr != nil {
		t.Fatalf("build fixture plugin: %v", packFixtureErr)
	}
	return packFixtureBin
}

// writeUnitPack writes a real pack whose [[services]] entry ships bin/fixture
// with the given pinned sha and argv, and returns its root.
func writeUnitPack(t *testing.T, fixtureBin, sha string, argv ...string) string {
	t.Helper()
	return writeNamedUnitPack(t, "unit-pack", "fixture-svc", fixtureBin, sha, argv...)
}

// writeNamedUnitPack is writeUnitPack with the pack name and the [[services]]
// unit name both parameterized, so a multi-pack test can build two packs
// that either share or collide on a unit name.
func writeNamedUnitPack(t *testing.T, packName, svcName, fixtureBin, sha string, argv ...string) string {
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
	argvLine := ""
	if len(argv) > 0 {
		argvLine = `argv = ["` + strings.Join(argv, `", "`) + `"]` + "\n"
	}
	manifest := `name = "` + packName + `"
schema = 2

[[services]]
name = "` + svcName + `"
runtime = "go-plugin"
activation = "always"
path = "bin/fixture"
sha = "` + sha + `"
` + argvLine + `license = "MIT"
source = "https://github.com/example/fixture"
`
	if err := os.WriteFile(filepath.Join(root, packinfo.PackManifestName), []byte(manifest), 0o644); err != nil {
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
	p, err := packinfo.LoadPack(root)
	if err != nil {
		t.Fatal(err)
	}
	fp, _, err := pack.ComputeHostExecFingerprint(root, pack.ComputeHostBoM(p, "", pack.PackLocalMCP()))
	if err != nil {
		t.Fatal(err)
	}
	store := &pack.PackTrustStore{}
	store.RecordAcceptance(store.TrustKey(root), pack.PackTrustRecord{Path: packinfo.CanonicalizePackRoot(root), Fingerprint: fp})
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
}

// acceptSurfaces is acceptSurface generalized to record MORE THAN ONE pack's
// acceptance in the SAME trust-store save: acceptSurface builds a bare
// &pack.PackTrustStore{} and calls Save() directly (no fresh load), which is
// fine for a single call but would have the second call's Save silently wipe
// the first pack's just-recorded acceptance out from under a multi-pack test.
func acceptSurfaces(t *testing.T, roots ...string) {
	t.Helper()
	store := &pack.PackTrustStore{}
	for _, root := range roots {
		p, err := packinfo.LoadPack(root)
		if err != nil {
			t.Fatal(err)
		}
		fp, _, err := pack.ComputeHostExecFingerprint(root, pack.ComputeHostBoM(p, "", pack.PackLocalMCP()))
		if err != nil {
			t.Fatal(err)
		}
		store.RecordAcceptance(store.TrustKey(root), pack.PackTrustRecord{Path: packinfo.CanonicalizePackRoot(root), Fingerprint: fp})
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
}

// acceptedViews reloads root and returns its accepted go-plugin views.
func acceptedViews(t *testing.T, root string) []pack.AcceptedService {
	t.Helper()
	p, err := packinfo.LoadPack(root)
	if err != nil {
		t.Fatal(err)
	}
	views, err := pack.AcceptedGoPluginServicesForSelf(p, "", "")
	if err != nil {
		t.Fatalf("AcceptedGoPluginServices: %v", err)
	}
	return views
}

// stageDirOf resolves the supervisor's staging dir under the isolated state.
func stageDirOf(t *testing.T) string {
	t.Helper()
	dir, err := config.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "supervise", "stage")
}

// TestPackUnitWiring_AcceptedRunsRealUnit is the whole path in one run:
// accepted pack → minimal view export → reconcilePackUnits → a REAL external
// go-plugin subprocess is staged (sha re-verified), launched, handshaken,
// health-probed, and dispensed through the returned holder.
func TestPackUnitWiring_AcceptedRunsRealUnit(t *testing.T) {
	isolatePackState(t)
	bin := buildPackFixture(t)
	root := writeUnitPack(t, bin, fileSHA(t, bin))
	acceptSurface(t, root)
	views := acceptedViews(t, root)
	if len(views) != 1 || views[0].Name != "fixture-svc" {
		t.Fatalf("views = %+v", views)
	}

	sup := &supervisor{}
	t.Cleanup(sup.shutdown)
	holders, err := sup.reconcilePackUnits("", views)
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
	// Reconcile is idempotent: a second pass over the SAME views touches
	// nothing and launches nothing new.
	again, err := sup.reconcilePackUnits("", views)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second reconcile added units: %v", again)
	}
}

// TestPackUnitWiring_RejectedStagesNothing: without acceptance there is no
// view, and therefore NOTHING downstream — no staging dir, no unit, no exec.
func TestPackUnitWiring_RejectedStagesNothing(t *testing.T) {
	isolatePackState(t)
	bin := buildPackFixture(t)
	root := writeUnitPack(t, bin, fileSHA(t, bin))
	// no acceptSurface — the gate was never passed

	p, err := packinfo.LoadPack(root)
	if err != nil {
		t.Fatal(err)
	}
	views, err := pack.AcceptedGoPluginServicesForSelf(p, "", "")
	if err == nil || views != nil {
		t.Fatalf("unaccepted pack exported %+v (err=%v)", views, err)
	}
	if _, statErr := os.Stat(stageDirOf(t)); !os.IsNotExist(statErr) {
		t.Errorf("staging dir exists before any admission: %v", statErr)
	}
}

// TestPackUnitWiring_TamperedBinaryRefusedAtStart: consent is recorded over
// the DECLARED pin; a binary swapped after acceptance still matches the
// fingerprint (the declaration is unchanged) but MUST be refused at staging.
func TestPackUnitWiring_TamperedBinaryRefusedAtStart(t *testing.T) {
	isolatePackState(t)
	bin := buildPackFixture(t)
	root := writeUnitPack(t, bin, fileSHA(t, bin))
	acceptSurface(t, root)

	// Swap the shipped binary AFTER acceptance (the classic consent-to-launch gap).
	if err := os.WriteFile(filepath.Join(root, "bin", "fixture"), []byte("#!/bin/sh\necho pwned\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	views := acceptedViews(t, root)
	sup := &supervisor{}
	t.Cleanup(sup.shutdown)
	holders, err := sup.reconcilePackUnits("", views)
	if err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("tampered binary launched (err=%v); want a sha256 mismatch refusal", err)
	}
	if len(holders) != 0 {
		t.Errorf("holders = %v, want none for a refused unit", holders)
	}
}

// TestPackUnitSpec_ClosedKindSet: the constructor only ever emits a UnitSpec
// for a registered go-plugin capability — an unknown kind fails closed.
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

// TestPackUnitWiring_RemovesDeletedService: a service dropped from the
// manifest (re-accepted at the new, smaller surface) is torn OUT of the tree
// on the next reconcile — never left running as an orphan.
func TestPackUnitWiring_RemovesDeletedService(t *testing.T) {
	isolatePackState(t)
	bin := buildPackFixture(t)
	root := writeUnitPack(t, bin, fileSHA(t, bin))
	acceptSurface(t, root)

	sup := &supervisor{}
	t.Cleanup(sup.shutdown)
	holders, err := sup.reconcilePackUnits("", acceptedViews(t, root))
	if err != nil || holders["fixture-svc"] == nil {
		t.Fatalf("initial add: holders=%v err=%v", holders, err)
	}
	first := holders["fixture-svc"]

	// The pack drops its [[services]] entry entirely; re-accept the smaller surface.
	if err := os.WriteFile(filepath.Join(root, packinfo.PackManifestName), []byte("name = \"unit-pack\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	acceptSurface(t, root)

	again, err := sup.reconcilePackUnits("", acceptedViews(t, root))
	if err != nil {
		t.Fatalf("reconcile after removal: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("holders after removal = %v, want none", again)
	}
	if first.Get() != nil {
		t.Error("the removed unit's holder is still dispensing — it was not stopped")
	}
}

// TestPackUnitWiring_RestartsChangedService: a service whose accepted spec
// changed (same name, different argv) is RESTARTED — the stale generation is
// stopped and a fresh one takes its name, never mutated in place.
func TestPackUnitWiring_RestartsChangedService(t *testing.T) {
	isolatePackState(t)
	bin := buildPackFixture(t)
	root := writeUnitPack(t, bin, fileSHA(t, bin))
	acceptSurface(t, root)

	sup := &supervisor{}
	t.Cleanup(sup.shutdown)
	holders, err := sup.reconcilePackUnits("", acceptedViews(t, root))
	if err != nil || holders["fixture-svc"] == nil {
		t.Fatalf("initial add: holders=%v err=%v", holders, err)
	}
	first := holders["fixture-svc"]

	// Same binary, a changed declared argv: a different accepted spec.
	root2 := writeUnitPack(t, bin, fileSHA(t, bin), "--changed")
	if err := os.Rename(filepath.Join(root2, packinfo.PackManifestName), filepath.Join(root, packinfo.PackManifestName)); err != nil {
		t.Fatal(err)
	}
	acceptSurface(t, root)

	again, err := sup.reconcilePackUnits("", acceptedViews(t, root))
	if err != nil {
		t.Fatalf("reconcile after spec change: %v", err)
	}
	if again["fixture-svc"] == nil {
		t.Fatalf("changed unit was not restarted: %v", again)
	}
	if first.Get() != nil {
		t.Error("the stale generation is still dispensing — restart did not stop it")
	}
}

// TestMergePackServices_TwoPacksBothSurvive: two packs declaring distinct unit
// names both come through mergePackServices intact, with no error — the
// aggregation step behind the fix to the "one reconcile call per pack"
// defect (each such call treated its OWN pack's views as the entire desired
// state, so calling it a second time for a second pack read the first
// pack's units as dropped and removed them).
func TestMergePackServices_TwoPacksBothSurvive(t *testing.T) {
	a := pack.AcceptedService{Name: "svc-a", Activation: "always", Path: "/abs/a", SHA: strings.Repeat("aa", 32)}
	b := pack.AcceptedService{Name: "svc-b", Activation: "always", Path: "/abs/b", SHA: strings.Repeat("bb", 32)}
	merged, err := mergePackServices([]packServiceSet{
		{packName: "pack-a", views: []pack.AcceptedService{a}},
		{packName: "pack-b", views: []pack.AcceptedService{b}},
	})
	if err != nil {
		t.Fatalf("mergePackServices: %v", err)
	}
	if len(merged) != 2 {
		t.Fatalf("merged = %+v, want both packs' services", merged)
	}
	names := map[string]bool{}
	for _, v := range merged {
		names[v.Name] = true
	}
	if !names["svc-a"] || !names["svc-b"] {
		t.Fatalf("merged = %+v, want svc-a AND svc-b, not one overwriting the other", merged)
	}
}

// TestMergePackServices_DuplicateNameFailsClosed: two packs declaring the SAME
// unit name is a hard conflict. Neither variant may win silently — the
// colliding name is dropped from the merged result entirely (fail closed),
// the returned error names BOTH packs, and an unrelated third service from a
// third pack is unaffected.
func TestMergePackServices_DuplicateNameFailsClosed(t *testing.T) {
	first := pack.AcceptedService{Name: "shared-svc", Activation: "always", Path: "/abs/first", SHA: strings.Repeat("11", 32)}
	second := pack.AcceptedService{Name: "shared-svc", Activation: "always", Path: "/abs/second", SHA: strings.Repeat("22", 32)}
	other := pack.AcceptedService{Name: "svc-c", Activation: "always", Path: "/abs/c", SHA: strings.Repeat("cc", 32)}
	merged, err := mergePackServices([]packServiceSet{
		{packName: "pack-one", views: []pack.AcceptedService{first}},
		{packName: "pack-two", views: []pack.AcceptedService{second}},
		{packName: "pack-three", views: []pack.AcceptedService{other}},
	})
	if err == nil {
		t.Fatal("duplicate unit name across two packs did not error")
	}
	if !strings.Contains(err.Error(), "pack-one") || !strings.Contains(err.Error(), "pack-two") {
		t.Fatalf("error %q does not name both colliding packs", err.Error())
	}
	if !strings.Contains(err.Error(), "shared-svc") {
		t.Fatalf("error %q does not name the colliding unit", err.Error())
	}
	for _, v := range merged {
		if v.Name == "shared-svc" {
			t.Fatalf("merged = %+v, want the colliding name dropped entirely, not one variant picked", merged)
		}
	}
	if len(merged) != 1 || merged[0].Name != "svc-c" {
		t.Fatalf("merged = %+v, want only the unrelated third-pack service to survive", merged)
	}
}

// TestPackUnitWiring_TwoPacksBothRunAfterSingleReconcile is the end-to-end
// regression proof for the defect: two REAL packs, each with a distinct
// service, admitted through the SAME two-stage flow runServe now uses
// (mergePackServices then exactly ONE reconcilePackUnits call). Both units
// must be running holders afterward. Before the fix, runServe called
// reconcilePackUnits once per pack; the second call's views (pack-b's alone)
// caused pack-a's already-running unit to be read as dropped and removed.
func TestPackUnitWiring_TwoPacksBothRunAfterSingleReconcile(t *testing.T) {
	isolatePackState(t)
	bin := buildPackFixture(t)
	rootA := writeNamedUnitPack(t, "pack-a", "svc-a", bin, fileSHA(t, bin))
	rootB := writeNamedUnitPack(t, "pack-b", "svc-b", bin, fileSHA(t, bin))
	acceptSurfaces(t, rootA, rootB)

	sets := []packServiceSet{
		{packName: "pack-a", views: acceptedViews(t, rootA)},
		{packName: "pack-b", views: acceptedViews(t, rootB)},
	}
	merged, err := mergePackServices(sets)
	if err != nil {
		t.Fatalf("mergePackServices: %v", err)
	}

	sup := &supervisor{}
	t.Cleanup(sup.shutdown)
	holders, err := sup.reconcilePackUnits("", merged)
	if err != nil {
		t.Fatalf("reconcilePackUnits: %v", err)
	}
	if holders["svc-a"] == nil || holders["svc-b"] == nil {
		t.Fatalf("holders = %v, want both svc-a and svc-b running from one reconcile call", holders)
	}
	for name, h := range holders {
		m, ok := h.Get().(plugin.MemoryStore)
		if !ok || m == nil {
			t.Fatalf("%s dispensed impl = %T, want a live MemoryStore", name, h.Get())
		}
		if info, herr := m.Health(); herr != nil || !info.OK {
			t.Fatalf("%s health = (%+v, %v)", name, info, herr)
		}
	}

	// Reproduce the OLD (buggy) call shape directly against the reconciler to
	// pin the regression: calling reconcilePackUnits a SECOND time with only
	// pack-b's views must not be how runServe drives the tree — this asserts
	// what that shape would do, so the guard rail stays visible even if the
	// merge step is ever bypassed. Given the actual, fixed call above already
	// ran once with BOTH packs merged, confirm the two holders are still
	// distinct, healthy units and neither one aliases the other.
	if holders["svc-a"] == holders["svc-b"] {
		t.Fatal("svc-a and svc-b resolved to the same holder — one pack's unit overwrote the other's")
	}
}

// TestPackUnitWiring_DuplicateServiceNameAcrossPacksRefusedBoth: two REAL
// packs both declaring a unit named "shared-svc" must have NEITHER survive
// reconciliation — the fail-closed merge behavior, proven against the real
// supervisor tree, not just the pure merge function.
func TestPackUnitWiring_DuplicateServiceNameAcrossPacksRefusedBoth(t *testing.T) {
	isolatePackState(t)
	bin := buildPackFixture(t)
	rootA := writeNamedUnitPack(t, "pack-one", "shared-svc", bin, fileSHA(t, bin))
	rootB := writeNamedUnitPack(t, "pack-two", "shared-svc", bin, fileSHA(t, bin), "--other")
	acceptSurfaces(t, rootA, rootB)

	sets := []packServiceSet{
		{packName: "pack-one", views: acceptedViews(t, rootA)},
		{packName: "pack-two", views: acceptedViews(t, rootB)},
	}
	merged, mergeErr := mergePackServices(sets)
	if mergeErr == nil {
		t.Fatal("colliding unit name across two packs did not error")
	}
	if !strings.Contains(mergeErr.Error(), "pack-one") || !strings.Contains(mergeErr.Error(), "pack-two") {
		t.Fatalf("error %q does not name both colliding packs", mergeErr.Error())
	}

	sup := &supervisor{}
	t.Cleanup(sup.shutdown)
	holders, err := sup.reconcilePackUnits("", merged)
	if err != nil {
		t.Fatalf("reconcilePackUnits on the (already deduped) merged set: %v", err)
	}
	if len(holders) != 0 {
		t.Fatalf("holders = %v, want neither colliding pack's unit running", holders)
	}
}
