package main

import (
	"bytes"
	"io"
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
// TestResolveKnowledgeInitArgs_TrailingJunk is the F2 gate: init validates EVERY
// token, not just argv[0]. A trailing flag typo or a second positional must be a
// usage error (no dir, no help) so nothing is scaffolded / no config is mutated.
func TestResolveKnowledgeInitArgs_TrailingJunk(t *testing.T) {
	bad := [][]string{
		{"./kb", "--jsom"},  // trailing flag typo after a valid DIR
		{"--jsom", "./kb"},  // leading flag typo
		{"./kb", "./other"}, // two positionals
		{"a", "b", "c"},     // several positionals
	}
	for _, argv := range bad {
		dir, help, err := resolveKnowledgeInitArgs(argv)
		if err == nil || help || dir != "" {
			t.Errorf("resolveKnowledgeInitArgs(%v) = (%q,%v,%v), want ('',false,error)", argv, dir, help, err)
		}
	}
	// A help token anywhere still wins over trailing junk.
	if _, help, err := resolveKnowledgeInitArgs([]string{"./kb", "--help"}); !help || err != nil {
		t.Errorf("resolveKnowledgeInitArgs([./kb --help]) help=%v err=%v, want help,nil", help, err)
	}
}

// TestKnowledgeInitTrailingJunk_NoSideEffects: `knowledge init ./kb --jsom` must
// NOT scaffold ./kb or touch config — the trailing typo is rejected before any
// filesystem/config side effect.
func TestKnowledgeInitTrailingJunk_NoSideEffects(t *testing.T) {
	tmp := t.TempDir()
	cfgFile := filepath.Join(tmp, "config.toml")
	t.Setenv("PI_STACK_CONFIG", cfgFile)
	cwd, _ := os.Getwd()
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	// runKnowledgeInit calls os.Exit(2) on a usage error, so exercise the pure
	// resolver + assert no bundle/config was created as a proxy for "no side effect".
	if _, _, err := resolveKnowledgeInitArgs([]string{"./kb", "--jsom"}); err == nil {
		t.Fatal("expected a usage error for trailing junk")
	}
	if _, err := os.Stat(filepath.Join(tmp, "kb")); err == nil {
		t.Error("trailing-junk init created the ./kb bundle dir")
	}
	if _, err := os.Stat(cfgFile); err == nil {
		t.Error("trailing-junk init wrote config")
	}
}

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

// TestPortablePointerRef: the committed .pi-stack/knowledge pointer holds a
// PORTABLE ref, never a resolved host-local cache path (F1). A git URL is
// written verbatim; a local path under the repo is written repo-relative; a
// local path outside the repo falls back to absolute.
func TestPortablePointerRef(t *testing.T) {
	// Git URL: verbatim, never a cache path.
	url := "https://github.com/acme/kb.git"
	if got := portablePointerRef(url, t.TempDir()); got != url {
		t.Errorf("portablePointerRef(%q) = %q, want the URL verbatim", url, got)
	}

	// Local path UNDER the repo dir -> repo-relative.
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "docs", "kb"), 0o755); err != nil {
		t.Fatal(err)
	}
	under := filepath.Join(repo, "docs", "kb")
	if got := portablePointerRef(under, repo); got != filepath.Join("docs", "kb") {
		t.Errorf("portablePointerRef(under-repo) = %q, want %q", got, filepath.Join("docs", "kb"))
	}

	// Local path OUTSIDE the repo -> absolute.
	outside := t.TempDir()
	wantAbs, _ := filepath.Abs(outside)
	if got := portablePointerRef(outside, repo); got != wantAbs {
		t.Errorf("portablePointerRef(outside-repo) = %q, want %q", got, wantAbs)
	}
}

// TestCacheDirForURL_DisambiguatesSameRepoName: two DIFFERENT git URLs that
// share a final repo name must map to DISTINCT cache dirs (F7), and the same
// URL must map stably to the same dir. Pure func, no clone.
func TestCacheDirForURL_DisambiguatesSameRepoName(t *testing.T) {
	cache := "/cache"
	a := cacheDirForURL(cache, "https://github.com/acme/kb.git")
	b := cacheDirForURL(cache, "https://github.com/other/kb.git")
	if a == b {
		t.Errorf("same-name repos collided: %q == %q", a, b)
	}
	// Both live under the cache dir.
	if filepath.Dir(a) != cache || filepath.Dir(b) != cache {
		t.Errorf("cache dirs not under %q: %q, %q", cache, a, b)
	}
	// Stable: same URL -> same dir.
	if again := cacheDirForURL(cache, "https://github.com/acme/kb.git"); again != a {
		t.Errorf("cacheDirForURL not stable: %q != %q", again, a)
	}
}

// TestSameGitURL: normalized equality ignores trailing slash and .git suffix.
func TestSameGitURL(t *testing.T) {
	if !sameGitURL("https://h/o/kb.git", "https://h/o/kb") {
		t.Error("sameGitURL should ignore .git suffix")
	}
	if sameGitURL("https://h/acme/kb", "https://h/other/kb") {
		t.Error("sameGitURL must distinguish different orgs")
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

// L1: `knowledge init` and `knowledge use` save daemon-affecting config
// (knowledge_bundles + services), so they must run the SAME config propagation
// as `config set knowledge_bundles` — otherwise the docs' "daemon-affecting
// writes auto-restart" claim is false on these paths and the user's running
// daemon never indexes the new bundle.
func TestKnowledgeInitAndUsePropagateServeConfig(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("PI_STACK_CONFIG", cfgFile)

	propagations := 0
	orig := knowledgePropagate
	knowledgePropagate = func(io.Writer) { propagations++ }
	defer func() { knowledgePropagate = orig }()

	dir := filepath.Join(t.TempDir(), "kb")
	runKnowledgeInit([]string{dir})
	if propagations != 1 {
		t.Fatalf("knowledge init propagated %d times, want 1", propagations)
	}

	runKnowledgeUse([]string{dir})
	if propagations != 2 {
		t.Fatalf("knowledge use propagated %d more times, want 1 more (total 2, got %d)", propagations-1, propagations)
	}
}
