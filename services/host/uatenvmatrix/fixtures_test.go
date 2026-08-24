// fixtures_test.go audits the literal identity every hand-authored fixture
// in this package declares against the contract its own checks assume:
// `strings.Contains(createOut, fixture.Name)` can only ever be a real
// positive receipt against a real `sbx env create` if the authored YAML
// itself tells sbx which name to use. docs/design/environments.md section
// 5.1 says a NORMAL registered environment omits `name` because Pix's own
// naming algorithm (Story 1's envinfo) writes it only to the generated
// effective file — but these are not Pix-composed effective files, they are
// Story 0's own raw, hand-authored fixtures proving the upstream contract
// directly, and fixtures.go's own doc comment is explicit that "Story 0
// owns this name directly". Before this test existed, every literal YAML
// body omitted `name` outright, so every check's own receipt assertion could
// only ever pass against a scripted test fake, never against a real `sbx`
// binary — this is the dishonesty this test exists to catch and pins closed.
package uatenvmatrix

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFixtureYAML_DeclaresOwnNameExplicitly(t *testing.T) {
	const candidateTag = "docker.io/mcavage/pix:uat-test"
	cases := []struct {
		label string
		name  string
		yaml  []byte
	}{
		{"customAgentFixture", customAgentFixture().Name, customAgentFixture().YAML},
		{"recreateBoundaryFixtureYAML (baseline)", recreateBoundaryFixtureName, recreateBoundaryFixtureYAML()},
		{"recreateBoundaryMutatedFixtureYAML (drifted)", recreateBoundaryFixtureName, recreateBoundaryMutatedFixtureYAML()},
		{"ollamaCapabilityFixtureYAML", ollamaCapabilityFixtureName, ollamaCapabilityFixtureYAML()},
		{"candidateImageFixture", candidateImageFixture(candidateTag).Name, candidateImageFixture(candidateTag).YAML},
		{"failedCreateCleanupFixtureYAML", failedCreateCleanupFixtureName, failedCreateCleanupFixtureYAML()},
		{"interpDefinedDefaultFixture", interpDefinedDefaultFixture().Name, interpDefinedDefaultFixture().YAML},
		{"interpMissingFixture", interpMissingFixture().Name, interpMissingFixture().YAML},
	}
	for _, c := range cases {
		want := "name: " + c.name
		if !strings.Contains(string(c.yaml), want) {
			t.Errorf("%s does not declare %q; a real `sbx env create` has no other way to know which name to use, so this fixture's own check assertion would only ever pass against a scripted fake, never a real sbx binary. Got:\n%s", c.label, want, c.yaml)
		}
	}
}

// TestWriteAuthoredFixture_MaterializesRelativeKitPaths is the regression
// test for host UAT run run-20260823-200503-777c37e1: `sbx env create`
// failed with `resolve kits: kit reference "./kit": path does not exist`
// because customAgentFixture's authored YAML declares `kits: - ./kit` but
// nothing ever created that directory on disk. It proves writeAuthoredFixture
// materializes every path a fixture's RelativeKits names, resolved relative
// to the authored file's OWN directory (the same way sbx itself resolves a
// relative kit reference), and that the materialized kit is a real,
// minimally valid kit-spec v2 shape — not an empty placeholder that would
// just move today's "path does not exist" failure to a later "invalid kit"
// one. It covers every fixture this package's own checks pass through
// writeAuthoredFixture with a non-empty RelativeKits list, so the fix cannot
// silently land for one named check while a sibling (e.g.
// environment_custom_agent_ollama) keeps the identical bug under a
// different check name.
func TestWriteAuthoredFixture_MaterializesRelativeKitPaths(t *testing.T) {
	cases := []struct {
		label   string
		fixture EnvironmentFixture
	}{
		{"customAgentFixture", customAgentFixture()},
		{"ollamaCapabilityFixture", ollamaCapabilityFixture()},
		{"candidateImageFixture", candidateImageFixture("docker.io/mcavage/pix:uat-test")},
		{"recreateBoundaryFixture", recreateBoundaryFixture()},
		{"interpDefinedDefaultFixture", interpDefinedDefaultFixture()},
		{"interpMissingFixture", interpMissingFixture()},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			if len(c.fixture.RelativeKits) == 0 {
				t.Fatalf("%s declares no RelativeKits; its YAML references `kits: - ./kit`, so this test would silently prove nothing", c.label)
			}

			phaseDir := t.TempDir()
			fixturePath, err := writeAuthoredFixture(phaseDir, "authored.sbxenv.yaml", c.fixture)
			if err != nil {
				t.Fatalf("writeAuthoredFixture: %v", err)
			}
			if _, err := os.Stat(fixturePath); err != nil {
				t.Fatalf("authored fixture was not written: %v", err)
			}

			for _, rel := range c.fixture.RelativeKits {
				kitDir := filepath.Join(filepath.Dir(fixturePath), rel)
				fi, err := os.Stat(kitDir)
				if err != nil {
					t.Fatalf("relative kit path %q referenced by the authored fixture does not exist on disk: %v (this is the exact `resolve kits: kit reference ... path does not exist` failure host UAT run run-20260823-200503-777c37e1 hit)", rel, err)
				}
				if !fi.IsDir() {
					t.Fatalf("relative kit path %q was materialized as a non-directory", rel)
				}

				specPath := filepath.Join(kitDir, "spec.yaml")
				specBytes, err := os.ReadFile(specPath)
				if err != nil {
					t.Fatalf("materialized kit %q has no spec.yaml: %v", rel, err)
				}
				spec := string(specBytes)
				for _, want := range []string{`schemaVersion: "2"`, "kind: sandbox", "sandbox:", "image:"} {
					if !strings.Contains(spec, want) {
						t.Errorf("materialized kit spec.yaml for %q missing %q; got:\n%s", rel, want, spec)
					}
				}
			}
		})
	}
}

// TestWriteAuthoredFixture_MaterializedKitDeclaresAuthoredAgentIdentity is
// the regression test for host UAT run run-20260824-082317-e58d0587: `sbx
// env create` failed with `ERROR: agent "pix" does not match agent kit name
// "kit" (set agent: "pix" in .sbxenv.yaml)` because writeAuthoredFixture
// named every materialized kit's spec.yaml after its directory basename
// ("./kit" -> "kit") instead of the identity the fixture's own authored
// YAML declares under its top-level `agent:` field ("pix"). sbx's real
// agent-kit identity check compares the environment's declared `agent:`
// value against the referenced kit's own declared name, never the kit
// directory's basename — so this proves the materialized `./kit/spec.yaml`
// declares the SAME agent identity ("pix") the authored environment names,
// for every fixture this package routes through writeAuthoredFixture, not
// just customAgentFixture.
func TestWriteAuthoredFixture_MaterializedKitDeclaresAuthoredAgentIdentity(t *testing.T) {
	cases := []struct {
		label   string
		fixture EnvironmentFixture
	}{
		{"customAgentFixture", customAgentFixture()},
		{"ollamaCapabilityFixture", ollamaCapabilityFixture()},
		{"candidateImageFixture", candidateImageFixture("docker.io/mcavage/pix:uat-test")},
		{"recreateBoundaryFixture", recreateBoundaryFixture()},
		{"interpDefinedDefaultFixture", interpDefinedDefaultFixture()},
		{"interpMissingFixture", interpMissingFixture()},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			var declaredAgent string
			for _, line := range strings.Split(string(c.fixture.YAML), "\n") {
				trimmed := strings.TrimSpace(line)
				if rest, ok := strings.CutPrefix(trimmed, "agent:"); ok {
					declaredAgent = strings.TrimSpace(rest)
					break
				}
			}
			if declaredAgent == "" {
				t.Fatalf("%s's authored YAML declares no top-level `agent:` field; this test cannot prove the materialized kit agrees with it", c.label)
			}

			phaseDir := t.TempDir()
			fixturePath, err := writeAuthoredFixture(phaseDir, "authored.sbxenv.yaml", c.fixture)
			if err != nil {
				t.Fatalf("writeAuthoredFixture: %v", err)
			}

			for _, rel := range c.fixture.RelativeKits {
				specPath := filepath.Join(filepath.Dir(fixturePath), rel, "spec.yaml")
				specBytes, err := os.ReadFile(specPath)
				if err != nil {
					t.Fatalf("materialized kit %q has no spec.yaml: %v", rel, err)
				}
				spec := string(specBytes)
				want := "name: " + declaredAgent
				if !strings.Contains(spec, want) {
					t.Errorf("materialized kit spec.yaml for %q does not declare %q; it declares an identity derived from the kit directory's basename instead of the authored environment's `agent: %s` field, which is exactly the `agent %q does not match agent kit name` failure host UAT run run-20260824-082317-e58d0587 hit. Got:\n%s", rel, want, declaredAgent, declaredAgent, spec)
				}
			}
		})
	}
}

// TestFixtureYAML_RecreateBoundaryDeclaresSameIdentityAcrossDrift proves the
// one property environment_recreate_boundary's whole assertion rests on: the
// baseline and drifted fixture bodies declare the SAME environment identity
// (same `name:`), differing only in the one mutated facet
// (recreateBoundaryMutatedFacet) — never a full rewrite that would also
// change which sandbox name is under test.
func TestFixtureYAML_RecreateBoundaryDeclaresSameIdentityAcrossDrift(t *testing.T) {
	baseline := string(recreateBoundaryFixtureYAML())
	drifted := string(recreateBoundaryMutatedFixtureYAML())
	want := "name: " + recreateBoundaryFixtureName
	if !strings.Contains(baseline, want) || !strings.Contains(drifted, want) {
		t.Fatalf("baseline and drifted fixtures must both declare %q; baseline=%q drifted=%q", want, baseline, drifted)
	}
}

// TestFixtureYAML_RecreateBoundaryDeclaresKits is the regression test for
// fresh UAT run run-20260824-095511-de9ece08: environment_recreate_boundary's
// baseline create failed with `ERROR: "pix" is not a known agent` because
// neither recreateBoundaryFixtureYAML nor recreateBoundaryMutatedFixtureYAML
// ever declared a `kits:` entry — the established host contract
// (fixtures.go's own package doc; customAgentFixture, ollamaCapabilityFixture,
// candidateImageFixture) is that every `agent: pix` fixture must reference a
// materialized kit whose own declared name is "pix". It proves both fixture
// bodies declare the same `kits: [./kit]` entry recreateBoundaryFixture's
// RelativeKits materializes.
func TestFixtureYAML_RecreateBoundaryDeclaresKits(t *testing.T) {
	want := "kits:\n  - ./kit"
	baseline := string(recreateBoundaryFixtureYAML())
	drifted := string(recreateBoundaryMutatedFixtureYAML())
	if !strings.Contains(baseline, want) {
		t.Errorf("baseline recreate-boundary fixture does not declare %q:\n%s", want, baseline)
	}
	if !strings.Contains(drifted, want) {
		t.Errorf("drifted recreate-boundary fixture does not declare %q:\n%s", want, drifted)
	}
}

// TestFixtureYAML_RecreateBoundaryOnlyMemoryFacetDiffers proves the baseline
// and drifted fixture bodies are identical apart from the one mutated facet
// recreateBoundaryMutatedFacet names — never a broader rewrite (e.g. adding
// `kits:` only to one side) that would dilute the check's one-fact mutation
// or change which sandbox/kit identity is under test.
func TestFixtureYAML_RecreateBoundaryOnlyMemoryFacetDiffers(t *testing.T) {
	baselineLines := strings.Split(string(recreateBoundaryFixtureYAML()), "\n")
	driftedLines := strings.Split(string(recreateBoundaryMutatedFixtureYAML()), "\n")
	if len(baselineLines) != len(driftedLines) {
		t.Fatalf("baseline and drifted fixtures have different line counts (%d vs %d); the only intended change is the memory facet's value", len(baselineLines), len(driftedLines))
	}
	diffs := 0
	for i := range baselineLines {
		if baselineLines[i] != driftedLines[i] {
			diffs++
			if !strings.Contains(baselineLines[i], "memory:") || !strings.Contains(driftedLines[i], "memory:") {
				t.Errorf("unexpected non-memory line diff at line %d: %q vs %q", i, baselineLines[i], driftedLines[i])
			}
		}
	}
	if diffs != 1 {
		t.Errorf("expected exactly 1 differing line between baseline and drifted fixtures, got %d", diffs)
	}
}
