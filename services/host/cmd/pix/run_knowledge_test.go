package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
)

// TestKnowledgeUseProject_LocalPath: `knowledge use --project <localpath>` writes
// <dir>/.pix/knowledge containing the resolved absolute bundle path, and
// does NOT touch global config.
func TestKnowledgeUseProject_LocalPath(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("PIX_CONFIG", cfgFile)

	repo := t.TempDir()
	bundle := t.TempDir()

	var buf bytes.Buffer
	if err := knowledgeUseProject(bundle, repo, &buf); err != nil {
		t.Fatalf("knowledgeUseProject: %v", err)
	}

	pointer := filepath.Join(repo, ".pix", "knowledge")
	got := strings.TrimSpace(readFile(t, pointer))
	wantAbs, _ := filepath.Abs(bundle)
	if got != wantAbs {
		t.Errorf("pointer = %q, want %q", got, wantAbs)
	}

	// Global config untouched (no file written / no bundles wired).
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.KnowledgeBundles) != 0 {
		t.Errorf("--project must not touch global config; knowledge_bundles = %v", cfg.KnowledgeBundles)
	}
}

// TestReadProjectPointer_SkipsCommentsAndBlanks: the pointer reader returns the
// first meaningful line and tolerates comments/blank lines a hand-author adds.
func TestReadProjectPointer_SkipsCommentsAndBlanks(t *testing.T) {
	repo := t.TempDir()
	mustWritePointer(t, repo, "\n# a comment\n\n/abs/bundle\n/second\n")
	if got := readProjectPointer(repo); got != "/abs/bundle" {
		t.Errorf("readProjectPointer = %q, want /abs/bundle", got)
	}
	// Absent pointer -> empty.
	if got := readProjectPointer(t.TempDir()); got != "" {
		t.Errorf("readProjectPointer(absent) = %q, want empty", got)
	}
}

// TestWireKnowledgeScope_GlobalOnly: no project pointer -> scope file holds just
// the canonical global bundle id.
func TestWireKnowledgeScope_GlobalOnly(t *testing.T) {
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	global := t.TempDir()
	ws := t.TempDir()

	cfg := &config.Config{KnowledgeBundles: []string{global}}
	wireKnowledgeScope(cfg, ws, knowledgeRPC{up: func() bool { return false }})

	lines := scopeLines(t, ws)
	want := []string{canonicalizeKnowledgeBundle(global)}
	assertLines(t, lines, want)
}

// TestWireKnowledgeScope_GlobalPlusProject: a pointer adds the project bundle as
// a second canonical id, in order, de-duplicated.
func TestWireKnowledgeScope_GlobalPlusProject(t *testing.T) {
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	global := t.TempDir()
	project := t.TempDir()
	ws := t.TempDir()
	mustWritePointer(t, ws, project+"\n")

	cfg := &config.Config{KnowledgeBundles: []string{global}}
	wireKnowledgeScope(cfg, ws, knowledgeRPC{up: func() bool { return false }})

	lines := scopeLines(t, ws)
	want := []string{canonicalizeKnowledgeBundle(global), canonicalizeKnowledgeBundle(project)}
	assertLines(t, lines, want)
}

// TestWireKnowledgeScope_RelativePointerResolvesToWorkspace: a relative pointer
// resolves against the workspace dir.
func TestWireKnowledgeScope_RelativePointerResolvesToWorkspace(t *testing.T) {
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "kb"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWritePointer(t, ws, "kb\n")

	cfg := &config.Config{}
	wireKnowledgeScope(cfg, ws, knowledgeRPC{up: func() bool { return false }})

	lines := scopeLines(t, ws)
	want := []string{canonicalizeKnowledgeBundle(filepath.Join(ws, "kb"))}
	assertLines(t, lines, want)
}

// TestWireKnowledgeScope_NoBundles: no global bundles + no pointer -> no scope
// file written (recall = all/none).
func TestWireKnowledgeScope_NoBundles(t *testing.T) {
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	ws := t.TempDir()

	cfg := &config.Config{}
	wireKnowledgeScope(cfg, ws, knowledgeRPC{up: func() bool { return false }})

	if _, err := os.Stat(filepath.Join(ws, ".pix", "knowledge.scope")); !os.IsNotExist(err) {
		t.Errorf("expected no scope file, stat err = %v", err)
	}
}

// TestWireKnowledgeScope_RemovesStaleScope: a previously-written scope file is
// DELETED when the resolved id set is now empty (F3), so recall stops
// forwarding stale bundle ids and falls back to all/none.
func TestWireKnowledgeScope_RemovesStaleScope(t *testing.T) {
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	ws := t.TempDir()
	// Simulate a stale scope file left by an earlier run.
	if err := writeKnowledgeScope(ws, []string{"/stale/bundle"}); err != nil {
		t.Fatal(err)
	}
	scope := filepath.Join(ws, ".pix", "knowledge.scope")
	if _, err := os.Stat(scope); err != nil {
		t.Fatalf("precondition: stale scope file missing: %v", err)
	}

	// No global bundles + no pointer -> empty id set -> the stale file is removed.
	wireKnowledgeScope(&config.Config{}, ws, knowledgeRPC{up: func() bool { return false }})

	if _, err := os.Stat(scope); !os.IsNotExist(err) {
		t.Errorf("expected stale scope file removed, stat err = %v", err)
	}
}

// TestWireKnowledgeScope_LazyReindexGating: reindex fires only when the daemon is
// up AND the project bundle is absent from health.bundles.
func TestWireKnowledgeScope_LazyReindexGating(t *testing.T) {
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	project := t.TempDir()
	canon := canonicalizeKnowledgeBundle(project)

	newCase := func(up bool, health []string) (*[]string, knowledgeRPC) {
		var called []string
		return &called, knowledgeRPC{
			up:      func() bool { return up },
			health:  func() ([]string, error) { return health, nil },
			reindex: func(b string) error { called = append(called, b); return nil },
		}
	}

	// Daemon down: never reindex.
	{
		ws := t.TempDir()
		mustWritePointer(t, ws, project+"\n")
		called, rpc := newCase(false, nil)
		wireKnowledgeScope(&config.Config{}, ws, rpc)
		if len(*called) != 0 {
			t.Errorf("daemon down: reindex called %v, want none", *called)
		}
	}

	// Daemon up, project unknown: reindex the canonical project id once.
	{
		ws := t.TempDir()
		mustWritePointer(t, ws, project+"\n")
		called, rpc := newCase(true, []string{"/some/other/bundle"})
		wireKnowledgeScope(&config.Config{}, ws, rpc)
		if len(*called) != 1 || (*called)[0] != canon {
			t.Errorf("daemon up + unknown: reindex called %v, want [%s]", *called, canon)
		}
	}

	// Daemon up, project already indexed: skip reindex.
	{
		ws := t.TempDir()
		mustWritePointer(t, ws, project+"\n")
		called, rpc := newCase(true, []string{canon})
		wireKnowledgeScope(&config.Config{}, ws, rpc)
		if len(*called) != 0 {
			t.Errorf("daemon up + known: reindex called %v, want none", *called)
		}
	}
}

// TestToStringSlice: coercion tolerates []any (decoded JSON) and []string.
func TestToStringSlice(t *testing.T) {
	if got := toStringSlice([]any{"a", 1, "b"}); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("toStringSlice([]any) = %v", got)
	}
	if got := toStringSlice([]string{"x"}); len(got) != 1 || got[0] != "x" {
		t.Errorf("toStringSlice([]string) = %v", got)
	}
	if got := toStringSlice(nil); got != nil {
		t.Errorf("toStringSlice(nil) = %v, want nil", got)
	}
}

// --- helpers ---------------------------------------------------------------

func mustWritePointer(t *testing.T, workspace, content string) {
	t.Helper()
	dir := filepath.Join(workspace, ".pix")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "knowledge"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func scopeLines(t *testing.T, workspace string) []string {
	t.Helper()
	b := readFile(t, filepath.Join(workspace, ".pix", "knowledge.scope"))
	if !strings.HasSuffix(b, "\n") {
		t.Errorf("scope file must end with a newline; got %q", b)
	}
	var out []string
	for _, ln := range strings.Split(strings.TrimRight(b, "\n"), "\n") {
		if ln != "" {
			out = append(out, ln)
		}
	}
	return out
}

func assertLines(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("scope lines = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("scope line %d = %q, want %q", i, got[i], want[i])
		}
	}
}
