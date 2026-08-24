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
		{"candidateImageFixtureYAML", candidateImageFixtureName, candidateImageFixtureYAML(candidateTag)},
		{"failedCreateCleanupFixtureYAML", failedCreateCleanupFixtureName, failedCreateCleanupFixtureYAML()},
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
