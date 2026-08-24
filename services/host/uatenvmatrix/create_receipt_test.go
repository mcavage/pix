// create_receipt_test.go proves the PURE, independently-testable parsing
// and validation helpers checkEnvironmentUsesLocalCandidateImage builds on
// (parseTemplateListRepoTag/templateListHasExactRepoTag,
// extractPrepareImageCheckRef/validateCreateReceiptResolvedImage) against
// the verbatim shape fresh UAT run run-20260824-092338-d4c384f5 actually
// observed: "PREPARE IMAGE \u2192 check <exact tag>" followed by a neutral
// "\u2713 image ready" line — never a fabricated "image digest: sha256:..."
// line the prior version of this check assumed.
package uatenvmatrix

import "testing"

func TestSplitImageRef(t *testing.T) {
	cases := []struct {
		ref      string
		wantRepo string
		wantTag  string
		wantOK   bool
	}{
		{"docker.io/mcavage/pix:uat-test", "docker.io/mcavage/pix", "uat-test", true},
		{"docker.io/mcavage/pix", "", "", false},
		{"docker.io/mcavage/pix:", "", "", false},
		{":uat-test", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		repo, tag, ok := splitImageRef(c.ref)
		if ok != c.wantOK || repo != c.wantRepo || tag != c.wantTag {
			t.Errorf("splitImageRef(%q) = (%q, %q, %v), want (%q, %q, %v)", c.ref, repo, tag, ok, c.wantRepo, c.wantTag, c.wantOK)
		}
	}
}

func TestParseTemplateListRepoTag_ExactColumnsNeverJoinedSubstring(t *testing.T) {
	// A row whose REPOSITORY ends in the exact characters another row's TAG
	// begins with (or vice versa) must never be treated as a match via a
	// joined "repo:tag" substring scan — only the parsed, positional
	// REPOSITORY and TAG columns count.
	out := `REPOSITORY               TAG        IMAGE ID       CREATED         SIZE
docker.io/mcavage/pix    x          sha256:aaaa    2 minutes ago   1.2GB
docker.io/mcavage/pi     uat-test   sha256:eeee    3 days ago      1.1GB
`
	has, err := templateListHasExactRepoTag(out, "docker.io/mcavage/pix", "uat-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if has {
		t.Fatal("templateListHasExactRepoTag matched a joined substring across two different rows' columns; it must only match a single row's own REPOSITORY+TAG pair")
	}
}

func TestParseTemplateListRepoTag_ExactMatchFound(t *testing.T) {
	out := `REPOSITORY               TAG        IMAGE ID       CREATED         SIZE
docker.io/mcavage/pix    uat-test   sha256:aaaa    2 minutes ago   1.2GB
`
	has, err := templateListHasExactRepoTag(out, "docker.io/mcavage/pix", "uat-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !has {
		t.Fatal("expected an exact REPOSITORY+TAG match")
	}
}

func TestParseTemplateListRepoTag_MissingHeaderColumnsErrors(t *testing.T) {
	out := "NAME  ID\nfoo   bar\n"
	if _, err := parseTemplateListRepoTag(out); err == nil {
		t.Fatal("expected an error when the header has no REPOSITORY/TAG columns")
	}
}

func TestParseTemplateListRepoTag_EmptyOutputErrors(t *testing.T) {
	if _, err := parseTemplateListRepoTag(""); err == nil {
		t.Fatal("expected an error for empty sbx template ls output")
	}
}

func TestExtractPrepareImageCheckRef(t *testing.T) {
	out := "PREPARE IMAGE \u2192 check docker.io/mcavage/pix:uat-test\n\u2713 image ready\ncreated pix-uatenv-fixture-image-uat-test (positively identified)\n"
	ref, found := extractPrepareImageCheckRef(out)
	if !found {
		t.Fatal("expected to find the PREPARE IMAGE line")
	}
	if ref != "docker.io/mcavage/pix:uat-test" {
		t.Fatalf("extractPrepareImageCheckRef = %q, want %q", ref, "docker.io/mcavage/pix:uat-test")
	}
}

func TestExtractPrepareImageCheckRef_MissingLine(t *testing.T) {
	_, found := extractPrepareImageCheckRef("created foo (positively identified)\n")
	if found {
		t.Fatal("expected no PREPARE IMAGE line to be found")
	}
}

func TestValidateCreateReceiptResolvedImage_AcceptsVerbatimObservedShape(t *testing.T) {
	imageTag := "docker.io/mcavage/pix:uat-test"
	out := "PREPARE IMAGE \u2192 check " + imageTag + "\n\u2713 image ready\ncreated pix-uatenv-fixture-image-uat-test (positively identified)\n"
	if err := validateCreateReceiptResolvedImage(out, imageTag); err != nil {
		t.Fatalf("expected the verbatim observed shape to validate cleanly, got: %v", err)
	}
}

func TestValidateCreateReceiptResolvedImage_RejectsMissingLine(t *testing.T) {
	imageTag := "docker.io/mcavage/pix:uat-test"
	out := "created pix-uatenv-fixture-image-uat-test (positively identified)\n"
	if err := validateCreateReceiptResolvedImage(out, imageTag); err == nil {
		t.Fatal("expected an error when no PREPARE IMAGE line is present")
	}
}

func TestValidateCreateReceiptResolvedImage_RejectsExactTagSubstitution(t *testing.T) {
	imageTag := "docker.io/mcavage/pix:uat-test"
	out := "PREPARE IMAGE \u2192 check docker.io/mcavage/pix:latest\n\u2713 image ready\n"
	if err := validateCreateReceiptResolvedImage(out, imageTag); err == nil {
		t.Fatal("expected an error when the receipt resolves a substituted tag for the same repository")
	}
}

func TestValidateCreateReceiptResolvedImage_RejectsMixedRepoRefs(t *testing.T) {
	imageTag := "docker.io/mcavage/pix:uat-test"
	out := "PREPARE IMAGE \u2192 check " + imageTag + "\n\u2713 image ready\ncached layer for docker.io/mcavage/pix:latest\n"
	if err := validateCreateReceiptResolvedImage(out, imageTag); err == nil {
		t.Fatal("expected an error when the receipt mentions the same repository with a different tag elsewhere")
	}
}

func TestValidateCreateReceiptResolvedImage_MalformedImageTagErrors(t *testing.T) {
	if err := validateCreateReceiptResolvedImage("anything", "no-colon-here"); err == nil {
		t.Fatal("expected an error for a malformed candidate image tag")
	}
}

func TestCandidateImageFixtureName_DeterministicAndBounded(t *testing.T) {
	tag := "docker.io/mcavage/pix:uat-run-20260824-092338-d4c384f5"
	a := candidateImageFixtureName(tag)
	b := candidateImageFixtureName(tag)
	if a != b {
		t.Fatalf("candidateImageFixtureName is not deterministic: %q != %q", a, b)
	}
	if len(a) > candidateImageFixtureNameMaxLen {
		t.Fatalf("candidateImageFixtureName exceeded the bound: %q (%d chars)", a, len(a))
	}
	if !hasPrefixUatenv(a) {
		t.Fatalf("candidateImageFixtureName %q does not start with the required uatenv fixture namespace", a)
	}
}

func TestCandidateImageFixtureName_DifferentTagsYieldDifferentNames(t *testing.T) {
	a := candidateImageFixtureName("docker.io/mcavage/pix:uat-run-20260824-092338-d4c384f5")
	b := candidateImageFixtureName("docker.io/mcavage/pix:uat-run-20260824-093000-aaaaaaaa")
	if a == b {
		t.Fatalf("two different run-unique candidate tags derived the SAME fixture name %q; a leaked prior run's fixture could then block this run's own attempt", a)
	}
}

func TestCandidateImageFixtureName_PriorFixedLiteralNameNeverReintroduced(t *testing.T) {
	// The regression this whole redesign exists to prevent: a fixed literal
	// name ("pix-uatenv-fixture-image") that a leaked prior run's residue
	// could permanently occupy, blocking every future run's own attempt at
	// this check.
	got := candidateImageFixtureName("docker.io/mcavage/pix:uat-test")
	if got == "pix-uatenv-fixture-image" {
		t.Fatalf("candidateImageFixtureName reintroduced the fixed prior literal name %q", got)
	}
}

func TestCandidateImageFixtureName_HandlesOversizedTagSegment(t *testing.T) {
	longTag := "docker.io/mcavage/pix:uat-run-" + repeatChar('a', 200)
	got := candidateImageFixtureName(longTag)
	if len(got) > candidateImageFixtureNameMaxLen {
		t.Fatalf("candidateImageFixtureName did not bound an oversized tag segment: %q (%d chars)", got, len(got))
	}
}

func repeatChar(c byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return string(b)
}

func hasPrefixUatenv(name string) bool {
	return len(name) >= len(uatenvFixturePrefix) && name[:len(uatenvFixturePrefix)] == uatenvFixturePrefix
}
