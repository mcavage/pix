// docs_evidence_test.go is Story 0's E0.7 anti-drift guard: it pins the
// FACTS docs/upstream/sbx-0.39-environments.md must keep stating, not a
// brittle full-prose snapshot of the document. A future edit that softens,
// removes, or silently "improves" the observed-contract record (the exact
// run identity, the six check names, the three interpolation outcomes, the
// exact-tag/no-pull correction, the Ollama capability result, or the
// cleanup debt disclosure) fails here instead of drifting unnoticed.
//
// This lives in uatenvmatrix, not a generic docs test, because CheckNames()
// is the actual source of truth for the six names asserted below: the doc
// must never enumerate a check this package does not really run, and this
// package must never grow a seventh check the doc silently fails to cover.
package uatenvmatrix

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readUpstreamEnvironmentsDoc(t *testing.T) string {
	t.Helper()
	// services/host/uatenvmatrix -> repo root is three levels up.
	path := filepath.Join("..", "..", "..", "docs", "upstream", "sbx-0.39-environments.md")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

func TestUpstreamEnvironmentsDoc_NamesEveryRealCheckByExactName(t *testing.T) {
	doc := readUpstreamEnvironmentsDoc(t)
	names := CheckNames()
	if len(names) != 6 {
		t.Fatalf("CheckNames() returned %d names, want 6 (update this test's expectations deliberately, not incidentally): %v", len(names), names)
	}
	for _, name := range names {
		if !strings.Contains(doc, name) {
			t.Errorf("upstream doc does not name the real check %q", name)
		}
	}
}

func TestUpstreamEnvironmentsDoc_NamesTheFinalRunAndCandidate(t *testing.T) {
	doc := readUpstreamEnvironmentsDoc(t)
	for _, want := range []string{
		"run-20260824-110322-d24dac52",
		"33499a056a4390b5095d0b50d51475b3580cd2ec",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("upstream doc does not name %q (the authoritative final host run/candidate)", want)
		}
	}
}

func TestUpstreamEnvironmentsDoc_NamesTheObservedSbxVersion(t *testing.T) {
	doc := readUpstreamEnvironmentsDoc(t)
	if !strings.Contains(doc, "v0.39.0") {
		t.Errorf("upstream doc does not name the observed sbx version v0.39.0")
	}
}

func TestUpstreamEnvironmentsDoc_RecordsAllThreeInterpolationOutcomes(t *testing.T) {
	doc := readUpstreamEnvironmentsDoc(t)
	for _, want := range []string{
		// defined ${VAR}: resolves to the exact known host value.
		"pix-uat-story0-defined-value",
		// missing ${VAR:-default}: resolves to the exact literal default.
		"fallback-value",
		// bare missing ${VAR}, no default: create succeeded, resolved to
		// empty string — never "refused" and never "literal unexpanded".
		"empty string",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("upstream doc does not record the interpolation outcome %q", want)
		}
	}
}

func TestUpstreamEnvironmentsDoc_CorrectsCandidateImageEvidenceToExactTagNoDigest(t *testing.T) {
	doc := readUpstreamEnvironmentsDoc(t)
	for _, want := range []string{
		"uat-run-20260824-110322-d24dac52",
		"no registry pull",
		"does not expose a created-sandbox digest field",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("upstream doc does not record the exact-tag/no-pull evidence fact %q", want)
		}
	}
	// The whole point of the correction: an explicit disclaimer that this is
	// never digest equality against a created sandbox, not merely silence on
	// the topic.
	if !strings.Contains(doc, "never digest equality") {
		t.Errorf("upstream doc does not explicitly disclaim created-sandbox digest equality as unobservable")
	}
}

func TestUpstreamEnvironmentsDoc_RecordsOllamaUnsupportedAndBridgeRequirement(t *testing.T) {
	doc := readUpstreamEnvironmentsDoc(t)
	for _, want := range []string{
		"unsupported",
		"extensions/ollama-bridge.ts",
		"--model` not found",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("upstream doc does not record the Ollama capability fact %q", want)
		}
	}
}

func TestUpstreamEnvironmentsDoc_RecordsCleanupDebtWithoutClaimingLeakFree(t *testing.T) {
	doc := readUpstreamEnvironmentsDoc(t)
	for _, want := range []string{
		"pix-uatenv-fixture-image",
		"run-20260824-092338-d4c384f5",
		"external cleanup debt",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("upstream doc does not record the cleanup-debt fact %q", want)
		}
	}
	if !strings.Contains(doc, "does not make Story 0 leak-free") {
		t.Errorf("upstream doc must explicitly refuse to call Story 0 leak-free, not merely mention the leaked fixture")
	}
	// No embedded destructive command presented as already run against the
	// leaked fixture: the only sanctioned host command in this section is the
	// read-only listing.
	if !strings.Contains(doc, "sbx ls --json") {
		t.Errorf("upstream doc does not name the safe read-only confirmation command (sbx ls --json)")
	}
	for _, destructive := range []string{
		"sbx rm pix-uatenv-fixture-image",
		"sbx env rm -f pix-uatenv-fixture-image",
		"docker rm pix-uatenv-fixture-image",
	} {
		if strings.Contains(doc, destructive) {
			t.Errorf("upstream doc embeds a destructive removal command against the leaked fixture as though already run: %q", destructive)
		}
	}
}
