package main

// release_convention_test.go — pins the RELEASE CONVENTION: versions are
// stamped by CI (.github/workflows/publish.yml computes 0.0.<run_number> on
// every push to main, stamps Makefile/package.json/pi-kit/spec.yaml, and
// commits the bump back). A feature branch therefore NEVER hand-bumps those
// files — they must stay mutually consistent at the base version, and the
// workflow must keep stamping all three, or a consumer's pinned-tag invariant
// (Makefile VERSION == package.json version == spec.yaml image tag) breaks.

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

// TestPublishWorkflowStampsVersions proves the CI workflow is the ONE place
// versions come from: it computes the version from the run number and stamps
// (then commits back) all three version-bearing files.
func TestPublishWorkflowStampsVersions(t *testing.T) {
	root := repoRootForRelease(t)
	wf := mustReadRepoFile(t, root, ".github", "workflows", "publish.yml")

	if !strings.Contains(wf, "0.0.${{ github.run_number }}") {
		t.Error("publish.yml must compute the version from github.run_number (0.0.<run_number>)")
	}
	for _, stamp := range []string{
		"Makefile",         // sed on VERSION ?=
		"package.json",     // sed on "version":
		"pi-kit/spec.yaml", // sed on image: docker.io/.../pi-stack:<v>
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
// last CI stamp) — a feature branch hand-bumping any one of them (or all
// three) would either desync them or collide with CI's next run_number stamp.
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
	spec := regexp.MustCompile(`image:\s*"docker\.io/[^"]*/pi-stack:([^"]+)"`).FindStringSubmatch(mustReadRepoFile(t, root, "pi-kit", "spec.yaml"))
	if spec == nil {
		t.Fatal("pi-kit/spec.yaml: no pinned pi-stack image tag")
	}
	if mk[1] != pj[1] || pj[1] != spec[1] {
		t.Errorf("version files disagree: Makefile=%s package.json=%s spec.yaml=%s — versions are CI-stamped together and must never be hand-bumped apart",
			mk[1], pj[1], spec[1])
	}
	if spec[1] == "latest" {
		t.Error("the kit image tag must be pinned, never :latest")
	}
}
