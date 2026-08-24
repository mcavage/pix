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
