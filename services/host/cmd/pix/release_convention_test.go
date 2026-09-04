package main

// release_convention_test.go pins the release convention: CI starts from the
// committed semver, selects the next patch whose v<version> tag is unused,
// stamps Makefile/package.json/pi-kit/spec.yaml, and commits the published
// version back. The three files must remain mutually consistent so a consumer's
// pinned image and kit tag always identify the same release.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoRootForRelease walks up from the test's working directory to the repo
// root (the directory holding .github/workflows/publish.yml). Skips when the
// test runs outside a full checkout (e.g. a vendored module cache).
func repoRootForRelease(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".github", "workflows", "publish.yml")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("no repo root with .github/workflows/publish.yml above the test dir")
		}
		dir = parent
	}
}

func mustReadRepoFile(t *testing.T, root string, rel ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{root}, rel...)...))
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Join(rel...), err)
	}
	return string(b)
}

// TestPublishWorkflowStampsVersions proves CI starts at the committed version,
// never overwrites an existing release tag, and stamps all lockstep files.
func TestPublishWorkflowStampsVersions(t *testing.T) {
	root := repoRootForRelease(t)
	wf := mustReadRepoFile(t, root, ".github", "workflows", "publish.yml")

	for _, selector := range []string{
		`require('./package.json').version`,
		`refs/tags/v${version}`,
		`patch=$((patch + 1))`,
		`fetch-depth: 0`,
	} {
		if !strings.Contains(wf, selector) {
			t.Errorf("publish.yml missing version-selection invariant %q", selector)
		}
	}
	if strings.Contains(wf, "0.0.${{ github.run_number }}") {
		t.Error("publish.yml must not reset Pix releases to 0.0.<run_number>")
	}
	for _, stamp := range []string{
		"Makefile",         // sed on VERSION ?=
		"package.json",     // sed on "version":
		"pi-kit/spec.yaml", // sed on image: docker.io/.../pix-agent:<v>
	} {
		if !strings.Contains(wf, stamp) {
			t.Errorf("publish.yml must stamp %s with the computed version", stamp)
		}
	}
	if !strings.Contains(wf, "git commit -m \"chore: publish $V [skip ci]\"") {
		t.Error("publish.yml must commit the version bump back to main ([skip ci])")
	}
}

// TestVersionFilesAgreeAtBase pins the pinned-tag invariant on the WORKING
// TREE: the three version-bearing files always carry the SAME version (the
// last CI stamp) — changing only one would desync the release identity.
func TestVersionFilesAgreeAtBase(t *testing.T) {
	root := repoRootForRelease(t)

	mk := regexp.MustCompile(`(?m)^VERSION\s*\?=\s*(\S+)`).FindStringSubmatch(mustReadRepoFile(t, root, "Makefile"))
	if mk == nil {
		t.Fatal("Makefile: no VERSION ?= line")
	}
	pj := regexp.MustCompile(`"version"\s*:\s*"([^"]+)"`).FindStringSubmatch(mustReadRepoFile(t, root, "package.json"))
	if pj == nil {
		t.Fatal("package.json: no version field")
	}
	spec := regexp.MustCompile(`image:\s*"docker\.io/[^"]*/pix-agent:([^"]+)"`).FindStringSubmatch(mustReadRepoFile(t, root, "pi-kit", "spec.yaml"))
	if spec == nil {
		t.Fatal("pi-kit/spec.yaml: no pinned pix image tag")
	}
	if mk[1] != pj[1] || pj[1] != spec[1] {
		t.Errorf("version files disagree: Makefile=%s package.json=%s spec.yaml=%s — versions are CI-stamped together and must never be hand-bumped apart",
			mk[1], pj[1], spec[1])
	}
	if spec[1] == "latest" {
		t.Error("the kit image tag must be pinned, never :latest")
	}
}
