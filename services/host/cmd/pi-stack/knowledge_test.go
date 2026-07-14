package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pi-stack/host/config"
	"pi-stack/host/okf"
)

// TestKnowledgeInit_Scaffold: init into a temp dir writes a spec-correct OKF
// skeleton (root index.md has okf_version frontmatter, the concept has a `type`,
// log.md has NO frontmatter) and wires config (knowledge_bundles gains the dir,
// services gains knowledge). The bundle parses with okf.ReadBundle.
func TestKnowledgeInit_Scaffold(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("PI_STACK_CONFIG", cfgFile)
	dir := filepath.Join(t.TempDir(), "kb")

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := knowledgeInit(cfg, dir, &buf); err != nil {
		t.Fatalf("knowledgeInit: %v", err)
	}

	// Root index.md carries okf_version frontmatter (and only that).
	index := readFile(t, filepath.Join(dir, "index.md"))
	if !strings.HasPrefix(index, "---\n") || !strings.Contains(index, "okf_version") {
		t.Errorf("index.md missing okf_version frontmatter:\n%s", index)
	}

	// The concept has a required `type` frontmatter key + a Citations section.
	concept := readFile(t, filepath.Join(dir, "reference", "getting-started.md"))
	if !strings.Contains(concept, "type: reference") {
		t.Errorf("concept missing `type` frontmatter:\n%s", concept)
	}
	if !strings.Contains(concept, "# Citations") {
		t.Errorf("concept missing # Citations section:\n%s", concept)
	}

	// log.md has NO frontmatter (reserved file rule).
	logMd := readFile(t, filepath.Join(dir, "log.md"))
	if strings.HasPrefix(logMd, "---") {
		t.Errorf("log.md must NOT have frontmatter:\n%s", logMd)
	}

	// The bundle parses cleanly: the concept is picked up with its type; the
	// reserved files are captured, not indexed as concepts.
	b, err := okf.ReadBundle(dir)
	if err != nil {
		t.Fatalf("okf.ReadBundle: %v", err)
	}
	if len(b.Warnings) != 0 {
		t.Errorf("ReadBundle warnings: %v", b.Warnings)
	}
	gs := b.Concept("reference/getting-started")
	if gs == nil {
		t.Fatalf("getting-started concept missing; concepts=%v", conceptIDList(b))
	}
	if gs.Type != "reference" {
		t.Errorf("concept type = %q, want reference", gs.Type)
	}
	if b.Index() == "" {
		t.Error("expected an index body")
	}
	if b.Concept("log") != nil || b.Concept("index") != nil {
		t.Error("reserved files must not be indexed as concepts")
	}

	// Config wired + persisted.
	got, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	absDir, _ := filepath.Abs(dir)
	if !containsStr(got.KnowledgeBundles, absDir) {
		t.Errorf("knowledge_bundles = %v, want %q", got.KnowledgeBundles, absDir)
	}
	if !containsStr(got.Services, "knowledge") {
		t.Errorf("services = %v, want knowledge", got.Services)
	}
}

// TestKnowledgeInit_Idempotent: a second init on an existing bundle does not
// clobber hand-authored content and re-wires config without duplicating.
func TestKnowledgeInit_Idempotent(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("PI_STACK_CONFIG", cfgFile)
	dir := filepath.Join(t.TempDir(), "kb")

	cfg, _ := config.Load()
	if err := knowledgeInit(cfg, dir, new(bytes.Buffer)); err != nil {
		t.Fatal(err)
	}
	// Hand-edit the concept, then re-init.
	concept := filepath.Join(dir, "reference", "getting-started.md")
	if err := os.WriteFile(concept, []byte("---\ntype: reference\n---\nMINE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg2, _ := config.Load()
	if err := knowledgeInit(cfg2, dir, new(bytes.Buffer)); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, concept); !strings.Contains(got, "MINE") {
		t.Errorf("re-init clobbered hand-authored concept: %q", got)
	}
	absDir, _ := filepath.Abs(dir)
	if n := countStr(cfg2.KnowledgeBundles, absDir); n != 1 {
		t.Errorf("knowledge_bundles has %q %d times, want 1: %v", absDir, n, cfg2.KnowledgeBundles)
	}
}

// TestKnowledgeUse_LocalPath: use <localpath> adds the abs path + enables the
// service, hermetically (no clone).
func TestKnowledgeUse_LocalPath(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("PI_STACK_CONFIG", cfgFile)
	bundle := t.TempDir()

	cfg, _ := config.Load()
	if err := knowledgeUse(cfg, bundle, new(bytes.Buffer)); err != nil {
		t.Fatalf("knowledgeUse: %v", err)
	}
	absDir, _ := filepath.Abs(bundle)
	if !containsStr(cfg.KnowledgeBundles, absDir) {
		t.Errorf("knowledge_bundles = %v, want %q", cfg.KnowledgeBundles, absDir)
	}
	if !containsStr(cfg.Services, "knowledge") {
		t.Errorf("services = %v, want knowledge", cfg.Services)
	}
}

// TestIsGitURL: the URL-vs-path classifier (pure, no clone needed).
func TestIsGitURL(t *testing.T) {
	urls := []string{
		"https://github.com/me/kb.git",
		"http://example.com/kb",
		"git@github.com:me/kb.git",
		"ssh://git@host/me/kb",
		"git://host/kb",
		"me/kb.git",
	}
	for _, u := range urls {
		if !isGitURL(u) {
			t.Errorf("isGitURL(%q) = false, want true", u)
		}
	}
	paths := []string{
		"/abs/path/kb",
		"./rel/kb",
		"../kb",
		"kb",
		"~/kb",
	}
	for _, p := range paths {
		if isGitURL(p) {
			t.Errorf("isGitURL(%q) = true, want false", p)
		}
	}
}

// TestRepoSlug: cache dir name derivation from a git URL.
func TestRepoSlug(t *testing.T) {
	cases := map[string]string{
		"https://github.com/me/kb.git": "kb",
		"git@github.com:me/kb.git":     "kb",
		"https://host/me/kb":           "kb",
		"ssh://git@host/me/kb.git":     "kb",
	}
	for url, want := range cases {
		if got := repoSlug(url); got != want {
			t.Errorf("repoSlug(%q) = %q, want %q", url, got, want)
		}
	}
}

// TestKnowledgeLs: renders the configured bundles + degrades when the daemon is
// down and when the service is disabled.
func TestKnowledgeLs(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("PI_STACK_CONFIG", cfgFile)

	cfg := defaultCfg()
	cfg.KnowledgeBundles = []string{"/tmp/kb"}
	cfg.Services = append(cfg.Services, "knowledge")

	// Daemon up.
	var up bytes.Buffer
	knowledgeLs(cfg, fakeEnv{ports: map[int]bool{11436: true}}.env(), &up)
	if !strings.Contains(up.String(), "/tmp/kb") || !strings.Contains(up.String(), "up") {
		t.Errorf("ls (up) = %q", up.String())
	}

	// Daemon down.
	var down bytes.Buffer
	knowledgeLs(cfg, fakeEnv{ports: map[int]bool{}}.env(), &down)
	if !strings.Contains(down.String(), "down") {
		t.Errorf("ls (down) = %q", down.String())
	}

	// Service disabled.
	cfg.Services = defaultCfg().Services // no knowledge
	var off bytes.Buffer
	knowledgeLs(cfg, fakeEnv{ports: map[int]bool{11436: true}}.env(), &off)
	if !strings.Contains(off.String(), "disabled") {
		t.Errorf("ls (disabled) = %q", off.String())
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func conceptIDList(b *okf.Bundle) []string {
	var ids []string
	for _, c := range b.Concepts() {
		ids = append(ids, c.ID)
	}
	return ids
}
