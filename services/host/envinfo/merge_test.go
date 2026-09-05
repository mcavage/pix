package envinfo_test

import (
	"testing"

	"pix/host/envinfo"
)

func mustParseBytes(t *testing.T, yaml, source, sourceDir string) *envinfo.Document {
	t.Helper()
	doc, err := envinfo.ParseBytes([]byte(yaml), source, sourceDir)
	if err != nil {
		t.Fatalf("ParseBytes(%s): %v", source, err)
	}
	return doc
}

func TestMerge_NestedMapsByKey(t *testing.T) {
	base := mustParseBytes(t, `schemaVersion: "1"
env:
  A: base-a
  SHARED: base-shared
`, "base.yaml", "/envs/home")
	overlay := mustParseBytes(t, `schemaVersion: "1"
env:
  B: overlay-b
  SHARED: overlay-shared
`, "overlay.yaml", "/envs/home")

	merged, err := envinfo.Merge(base, overlay)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if merged.Env["A"].Value != "base-a" {
		t.Errorf("env.A = %q, want base-a", merged.Env["A"].Value)
	}
	if merged.Env["B"].Value != "overlay-b" {
		t.Errorf("env.B = %q, want overlay-b", merged.Env["B"].Value)
	}
	if merged.Env["SHARED"].Value != "overlay-shared" {
		t.Errorf("env.SHARED = %q, want overlay-shared (later file replaces)", merged.Env["SHARED"].Value)
	}
	if merged.Env["SHARED"].Source != "overlay.yaml" {
		t.Errorf("env.SHARED source = %q, want overlay.yaml", merged.Env["SHARED"].Source)
	}
}

func TestMerge_ListsConcatenate(t *testing.T) {
	base := mustParseBytes(t, `schemaVersion: "1"
kits:
  - ./a
`, "base.yaml", "/envs/home")
	overlay := mustParseBytes(t, `schemaVersion: "1"
kits:
  - ./b
`, "overlay.yaml", "/envs/home")

	merged, err := envinfo.Merge(base, overlay)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if len(merged.Kits) != 2 {
		t.Fatalf("Kits = %+v, want 2 entries", merged.Kits)
	}
	if merged.Kits[0].Raw != "./a" || merged.Kits[1].Raw != "./b" {
		t.Errorf("Kits = %+v, want [./a ./b] in argument order", merged.Kits)
	}
}

func TestMerge_LaterScalarsReplace(t *testing.T) {
	base := mustParseBytes(t, `schemaVersion: "1"
agent: pix
sandboxOptions:
  memory: 6g
`, "base.yaml", "/envs/home")
	overlay := mustParseBytes(t, `schemaVersion: "1"
sandboxOptions:
  memory: 60g
`, "overlay.yaml", "/envs/home")

	merged, err := envinfo.Merge(base, overlay)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if merged.Agent != "pix" {
		t.Errorf("Agent = %q, want pix (overlay left it unset)", merged.Agent)
	}
	if merged.SandboxOptions["memory"].Value != "60g" {
		t.Errorf("sandboxOptions.memory = %q, want 60g", merged.SandboxOptions["memory"].Value)
	}
	if merged.SandboxOptions["memory"].Source != "overlay.yaml" {
		t.Errorf("sandboxOptions.memory source = %q, want overlay.yaml", merged.SandboxOptions["memory"].Source)
	}
}

// TestMerge_StoryZeroFixtureShapes_RecreateBoundary mirrors
// services/host/uatenvmatrix/fixtures.go's own recreateBoundaryFixtureYAML
// / recreateBoundaryMutatedFixtureYAML pair — same shape (schemaVersion,
// agent, kits, sandboxOptions.memory 6g -> 60g), authored independently
// here rather than imported (uatenvmatrix proves its own literal fixture
// bytes; this package must never be the thing that agreement is measured
// against — see arch_test.go's TestArchitecture_UatenvmatrixNeverImportsEnvinfo).
// This proves envinfo's merge produces the exact same "later file replaces
// a scalar" outcome Story 0's recreate-boundary check exercises against a
// real sbx binary.
func TestMerge_StoryZeroFixtureShapes_RecreateBoundary(t *testing.T) {
	baseline := mustParseBytes(t, `schemaVersion: "1"
agent: pix
name: pix-uatenv-fixture-recreate

kits:
  - ./kit

sandboxOptions:
  memory: 6g
`, "recreate.yaml", "/envs/recreate")
	mutated := mustParseBytes(t, `schemaVersion: "1"
agent: pix
name: pix-uatenv-fixture-recreate

kits:
  - ./kit

sandboxOptions:
  memory: 60g
`, "recreate.yaml", "/envs/recreate")

	baselineMerged, err := envinfo.Merge(baseline)
	if err != nil {
		t.Fatalf("Merge(baseline): %v", err)
	}
	if baselineMerged.SandboxOptions["memory"].Value != "6g" {
		t.Fatalf("baseline memory = %q, want 6g", baselineMerged.SandboxOptions["memory"].Value)
	}

	mutatedMerged, err := envinfo.Merge(mutated)
	if err != nil {
		t.Fatalf("Merge(mutated): %v", err)
	}
	if mutatedMerged.SandboxOptions["memory"].Value != "60g" {
		t.Fatalf("mutated memory = %q, want 60g", mutatedMerged.SandboxOptions["memory"].Value)
	}
	if mutatedMerged.Name != baselineMerged.Name {
		t.Errorf("Name drifted across the recreate-boundary facet change: %q vs %q", baselineMerged.Name, mutatedMerged.Name)
	}
}

func TestMerge_BindingDomainsConcatenateAcrossFiles(t *testing.T) {
	base := mustParseBytes(t, `schemaVersion: "1"
bindings:
  anthropic:
    apiKey:
      domains:
        - api.anthropic.com
`, "base.yaml", "/envs/home")
	overlay := mustParseBytes(t, `schemaVersion: "1"
bindings:
  anthropic:
    apiKey:
      domains:
        - api.anthropic.com.internal
`, "overlay.yaml", "/envs/home")

	merged, err := envinfo.Merge(base, overlay)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	domains := merged.Bindings["anthropic"].Domains
	if len(domains) != 2 {
		t.Fatalf("domains = %+v, want 2", domains)
	}
	if domains[0].Domain != "api.anthropic.com" || domains[0].Source != "base.yaml" {
		t.Errorf("domains[0] = %+v", domains[0])
	}
	if domains[1].Domain != "api.anthropic.com.internal" || domains[1].Source != "overlay.yaml" {
		t.Errorf("domains[1] = %+v", domains[1])
	}
}

func TestMerge_SecretRecordReplacedWholesaleOnCollision(t *testing.T) {
	base := mustParseBytes(t, `schemaVersion: "1"
secrets:
  anthropic:
    ref: op://Personal/Anthropic/api-key
`, "base.yaml", "/envs/home")
	overlay := mustParseBytes(t, `schemaVersion: "1"
secrets:
  anthropic:
    command: ["op-alt", "read"]
`, "overlay.yaml", "/envs/home")

	merged, err := envinfo.Merge(base, overlay)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	got := merged.Secrets["anthropic"]
	if got.Ref != "" {
		t.Errorf("secrets.anthropic.ref = %q, want empty (wholesale replace)", got.Ref)
	}
	if len(got.Command) != 2 {
		t.Errorf("secrets.anthropic.command = %v, want the overlay's command", got.Command)
	}
	if got.Source != "overlay.yaml" {
		t.Errorf("source = %q, want overlay.yaml", got.Source)
	}
}

func TestMerge_ListGrowthAttributionIsStableAcrossGrowth(t *testing.T) {
	a := mustParseBytes(t, `schemaVersion: "1"
kits:
  - ./a
`, "a.yaml", "/envs/home")
	b := mustParseBytes(t, `schemaVersion: "1"
kits:
  - ./b
`, "b.yaml", "/envs/home")
	c := mustParseBytes(t, `schemaVersion: "1"
kits:
  - ./c
`, "c.yaml", "/envs/home")

	twoFile, err := envinfo.Merge(a, b)
	if err != nil {
		t.Fatalf("Merge(a,b): %v", err)
	}
	threeFile, err := envinfo.Merge(a, b, c)
	if err != nil {
		t.Fatalf("Merge(a,b,c): %v", err)
	}

	// Appending a third document must not change either earlier item's
	// attribution: index 0 stays a.yaml, index 1 stays b.yaml, in both merges.
	if twoFile.Kits[0].Source != "a.yaml" || threeFile.Kits[0].Source != "a.yaml" {
		t.Errorf("Kits[0].Source drifted: two-file=%q three-file=%q", twoFile.Kits[0].Source, threeFile.Kits[0].Source)
	}
	if twoFile.Kits[1].Source != "b.yaml" || threeFile.Kits[1].Source != "b.yaml" {
		t.Errorf("Kits[1].Source drifted: two-file=%q three-file=%q", twoFile.Kits[1].Source, threeFile.Kits[1].Source)
	}
	if threeFile.Kits[2].Source != "c.yaml" {
		t.Errorf("Kits[2].Source = %q, want c.yaml", threeFile.Kits[2].Source)
	}

	// And building the tree from each surfaces the SAME key-path -> source
	// pairing for the shared entries, proving the tree layer doesn't
	// renumber or reattribute on growth either.
	twoTree, err := envinfo.BuildTree(twoFile)
	if err != nil {
		t.Fatalf("BuildTree(two): %v", err)
	}
	threeTree, err := envinfo.BuildTree(threeFile)
	if err != nil {
		t.Fatalf("BuildTree(three): %v", err)
	}
	if twoTree.Kits[0] != threeTree.Kits[0] {
		t.Errorf("Kits[0] node drifted: %+v vs %+v", twoTree.Kits[0], threeTree.Kits[0])
	}
	if twoTree.Kits[1] != threeTree.Kits[1] {
		t.Errorf("Kits[1] node drifted: %+v vs %+v", twoTree.Kits[1], threeTree.Kits[1])
	}
}

func TestMerge_NoDocumentsRefused(t *testing.T) {
	if _, err := envinfo.Merge(); err == nil {
		t.Fatal("Merge(): expected an error with no documents, got nil")
	}
}
