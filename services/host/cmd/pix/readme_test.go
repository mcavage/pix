package main

import (
	"os"
	"path/filepath"
	"regexp"
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

func TestReadmePrimaryPathUsesHomebrew(t *testing.T) {
	body := primaryReadmePath(t)
	want := "brew install mcavage/tap/pix\npix setup\npix run"
	if !strings.Contains(body, want) {
		t.Fatalf("README primary path must be the three-command Homebrew flow:\n%s", want)
	}
	if strings.Contains(body, "curl ") {
		t.Fatal("README primary path must not mention the deprecated curl installer")
	}
}

func TestReadmeHasNoLegacyInstallerPath(t *testing.T) {
	s := readRepoFile(t, "README.md")
	if strings.Contains(s, "install.sh") || strings.Contains(s, "curl -fsSL") {
		t.Fatal("README must document Homebrew as the only supported install path")
	}
}

func TestReadmePackExamplesStayPublic(t *testing.T) {
	s := readRepoFile(t, "README.md")
	if !strings.Contains(s, "git+https://github.com/your-org/work-pack.git#ref=main") {
		t.Fatal("README pack examples must use the public placeholder repository")
	}
	if strings.Contains(s, "pix setup --pack docker/") {
		t.Fatal("README pack examples must not name an organization-specific repository")
	}
}

func TestReadmePrimaryPathCommandsExist(t *testing.T) {
	body := primaryReadmePath(t)
	known := map[string]bool{
		"version": true, "setup": true, "doctor": true, "run": true, "help": true,
	}
	re := regexp.MustCompile(`(?m)^pix ([a-z][a-z0-9-]*)[^\n]*$`)
	matches := re.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		t.Fatal("README primary path contains no Pix commands")
	}
	for _, m := range matches {
		if !known[m[1]] {
			t.Errorf("README primary path documents unknown Pix verb %q", m[1])
		}
	}
	for _, required := range []string{"pix setup", "pix run"} {
		if !strings.Contains(body, required) {
			t.Errorf("README primary path missing %q", required)
		}
	}
}

func TestDocumentedExitCodesMatchImplementation(t *testing.T) {
	man := readRepoFile(t, "services/host/cmd/pix/pix.1")
	for _, code := range []int{exitReady, exitNotReady, exitUsage, exitUnverifiable} {
		needle := ".B " + string(rune('0'+code))
		if !strings.Contains(man, needle) {
			t.Errorf("man page missing exit status %d", code)
		}
	}
	if !strings.Contains(man, "cannot be verified from the\ncurrent environment") {
		t.Error("exit 3 documentation must mean unverifiable, not a positively observed failure")
	}
}
