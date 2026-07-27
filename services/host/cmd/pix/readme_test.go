package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"pix/host/config"
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

func TestReadmeDocumentsSbxWithoutRequiringDockerDesktop(t *testing.T) {
	body := strings.ToLower(primaryReadmePath(t))
	if strings.Contains(body, "install docker desktop") {
		t.Fatal("README must not require Docker Desktop; sbx runs without it")
	}
	if !strings.Contains(body, "docker desktop is not required") {
		t.Fatal("README must state that Docker Desktop is not required")
	}
	if !strings.Contains(body, "brew install docker/tap/sbx@nightly") {
		t.Fatal("README must install the supported nightly Docker Sandboxes CLI")
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
	for _, required := range []string{"pix setup", "pix doctor", "pix run", "pix help"} {
		if !strings.Contains(body, required) {
			t.Errorf("README primary path missing %q", required)
		}
	}
}

func TestReadmeModelTagsMatchDefaults(t *testing.T) {
	s := readRepoFile(t, "README.md")
	for _, model := range []string{config.DefaultMemoryWatcherModel, config.DefaultMemoryEmbedModel, config.DefaultOllamaBridgeModel} {
		if !strings.Contains(s, model) {
			t.Errorf("README does not name configured default model %q", model)
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
