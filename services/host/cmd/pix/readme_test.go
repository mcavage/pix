package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readRepoFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRootForVersionLockstep(t), name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func primaryReadmePath(t *testing.T) string {
	t.Helper()
	s := readRepoFile(t, "README.md")
	const start = "<!-- PIX_PRIMARY_PATH_START -->"
	const end = "<!-- PIX_PRIMARY_PATH_END -->"
	if strings.Count(s, start) != 1 || strings.Count(s, end) != 1 {
		t.Fatalf("README must contain exactly one primary-path marker pair")
	}
	body := strings.SplitN(s, start, 2)[1]
	body = strings.SplitN(body, end, 2)[0]
	if strings.Contains(body, "sbx run") {
		t.Fatal("README primary path must use the Pix launcher, not raw sbx run")
	}
	return body
}

func TestReadmeHasOnePrimaryPath(t *testing.T) {
	body := primaryReadmePath(t)
	for _, phrase := range []string{"alternatively", "or you can"} {
		if strings.Contains(strings.ToLower(body), phrase) {
			t.Errorf("README primary path branches with %q", phrase)
		}
	}
}

func TestReadmePrimaryPathUsesMarsh(t *testing.T) {
	body := primaryReadmePath(t)
	want := "brew install mcavage/tap/marsh\nmarsh\npix"
	if !strings.Contains(body, want) {
		t.Fatalf("README primary path must be the marsh flow:\n%s", want)
	}
	for _, retired := range []string{"curl ", "mcavage/tap/pix"} {
		if strings.Contains(body, retired) {
			t.Fatalf("README primary path must not mention retired install path %q", retired)
		}
	}
}

func TestReadmeHasNoLegacyInstallerPath(t *testing.T) {
	s := readRepoFile(t, "README.md")
	if strings.Contains(s, "install.sh") || strings.Contains(s, "curl -fsSL") {
		t.Fatal("README must not document the deprecated curl installer")
	}
}

// TestReadmePackExamplesStayPublic guards the OPEN-CORE boundary: whatever pack
// example the README shows must be the placeholder, never a real organization's
// repository.
//
// It used to also require that an example be PRESENT, which conflated two
// different things. How much pack detail the front door carries is an editorial
// call (it was cut back hard once, for being evaluate-stage material dumped on a
// first-time reader); which repository an example names is a leak. Only the
// second is a test's business, so the check is conditional on there being an
// example at all.
func TestReadmePackExamplesStayPublic(t *testing.T) {
	s := readRepoFile(t, "README.md")
	if strings.Contains(s, "pix setup --pack docker/") {
		t.Fatal("README pack examples must not name an organization-specific repository")
	}
	if !strings.Contains(s, "--pack") {
		return // no pack example to police
	}
	if !strings.Contains(s, "your-org") {
		t.Fatal("README shows a --pack example that is not the public your-org placeholder")
	}
}
