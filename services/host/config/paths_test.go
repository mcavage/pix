package config

import (
	"os"
	"path/filepath"
	"testing"
)

// hermeticHome points HOME at a fresh temp dir and clears every XDG base and
// artifact override, so each path helper resolves against a clean slate with the
// real filesystem underneath.
func hermeticHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, k := range []string{
		"XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CONFIG_HOME", "PI_STACK_CONFIG",
		"MEMORY_DB", "KNOWLEDGE_DB", "KNOWLEDGE_CACHE_DIR",
	} {
		t.Setenv(k, "")
	}
	return home
}

// mkFile creates path (and its parents) as an empty real file.
func mkFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBaseResolvers(t *testing.T) {
	home := hermeticHome(t)

	// home defaults
	if got, _ := ConfigDir(); got != filepath.Join(home, ".config", "pi-stack") {
		t.Errorf("ConfigDir home = %q", got)
	}
	if got, _ := DataDir(); got != filepath.Join(home, ".local", "share", "pi-stack") {
		t.Errorf("DataDir home = %q", got)
	}
	if got, _ := StateDir(); got != filepath.Join(home, ".local", "state", "pi-stack") {
		t.Errorf("StateDir home = %q", got)
	}

	// XDG override
	xd, xs, xc := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("XDG_DATA_HOME", xd)
	t.Setenv("XDG_STATE_HOME", xs)
	t.Setenv("XDG_CONFIG_HOME", xc)
	if got, _ := DataDir(); got != filepath.Join(xd, "pi-stack") {
		t.Errorf("DataDir xdg = %q", got)
	}
	if got, _ := StateDir(); got != filepath.Join(xs, "pi-stack") {
		t.Errorf("StateDir xdg = %q", got)
	}
	if got, _ := ConfigDir(); got != filepath.Join(xc, "pi-stack") {
		t.Errorf("ConfigDir xdg = %q", got)
	}

	// PI_STACK_CONFIG's parent wins for ConfigDir (explicit-file override)
	cfg := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(cfg, "config.toml"))
	if got, _ := ConfigDir(); got != cfg {
		t.Errorf("ConfigDir PI_STACK_CONFIG = %q, want %q", got, cfg)
	}
}

func TestMemoryDBPathFallback(t *testing.T) {
	home := hermeticHome(t)
	newPath := filepath.Join(home, ".local", "share", "pi-stack", "memory", "memory.db")
	legacy := filepath.Join(home, ".pi-stack", "memory", "memory.db")

	// fresh: nothing on disk -> new path
	if got := MemoryDBPath(); got != newPath {
		t.Fatalf("fresh = %q, want %q", got, newPath)
	}
	// legacy exists, new absent -> legacy
	mkFile(t, legacy)
	if got := MemoryDBPath(); got != legacy {
		t.Fatalf("legacy = %q, want %q", got, legacy)
	}
	// new exists -> flips to new even with legacy still present
	mkFile(t, newPath)
	if got := MemoryDBPath(); got != newPath {
		t.Fatalf("flip = %q, want %q", got, newPath)
	}
	// MemoryLockPath converges on the resolved db dir
	if got, want := MemoryLockPath(), filepath.Join(filepath.Dir(newPath), ".memory.lock"); got != want {
		t.Fatalf("lock = %q, want %q", got, want)
	}
	// explicit override beats everything
	t.Setenv("MEMORY_DB", "/custom/mem.db")
	if got := MemoryDBPath(); got != "/custom/mem.db" {
		t.Fatalf("override = %q, want /custom/mem.db", got)
	}
	if got, want := MemoryLockPath(), filepath.Join("/custom", ".memory.lock"); got != want {
		t.Fatalf("override lock = %q, want %q", got, want)
	}
}

func TestBackupsDirFallback(t *testing.T) {
	home := hermeticHome(t)
	newPath := filepath.Join(home, ".local", "share", "pi-stack", "backups")
	legacy := filepath.Join(home, ".pi-stack", "backups")

	if got, _ := BackupsDir(); got != newPath {
		t.Fatalf("fresh = %q, want %q", got, newPath)
	}
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if got, _ := BackupsDir(); got != legacy {
		t.Fatalf("legacy = %q, want %q", got, legacy)
	}
	if err := os.MkdirAll(newPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if got, _ := BackupsDir(); got != newPath {
		t.Fatalf("flip = %q, want %q", got, newPath)
	}
}

func TestKnowledgeIndexPathFullPrecedence(t *testing.T) {
	home := hermeticHome(t)
	state := filepath.Join(home, ".local", "state", "pi-stack", "knowledge")
	indexNew := filepath.Join(state, "index.db")
	stateLegacyName := filepath.Join(state, "knowledge.db")
	legacy := filepath.Join(home, ".pi-stack", "knowledge", "knowledge.db")

	// fresh -> STATE/knowledge/index.db
	if got := KnowledgeIndexPath(); got != indexNew {
		t.Fatalf("fresh = %q, want %q", got, indexNew)
	}
	// legacy ~/.pi-stack name only
	mkFile(t, legacy)
	if got := KnowledgeIndexPath(); got != legacy {
		t.Fatalf("legacy = %q, want %q", got, legacy)
	}
	// STATE/knowledge/knowledge.db beats legacy
	mkFile(t, stateLegacyName)
	if got := KnowledgeIndexPath(); got != stateLegacyName {
		t.Fatalf("state-legacy-name = %q, want %q", got, stateLegacyName)
	}
	// STATE/knowledge/index.db beats knowledge.db
	mkFile(t, indexNew)
	if got := KnowledgeIndexPath(); got != indexNew {
		t.Fatalf("index = %q, want %q", got, indexNew)
	}
	// explicit override wins
	t.Setenv("KNOWLEDGE_DB", "/custom/kb.db")
	if got := KnowledgeIndexPath(); got != "/custom/kb.db" {
		t.Fatalf("override = %q, want /custom/kb.db", got)
	}
}

func TestKnowledgeCacheDirFallback(t *testing.T) {
	home := hermeticHome(t)
	newPath := filepath.Join(home, ".local", "state", "pi-stack", "knowledge-cache")
	legacy := filepath.Join(home, ".config", "pi-stack", "knowledge-cache")

	if got := KnowledgeCacheDir(); got != newPath {
		t.Fatalf("fresh = %q, want %q", got, newPath)
	}
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := KnowledgeCacheDir(); got != legacy {
		t.Fatalf("legacy = %q, want %q", got, legacy)
	}
	if err := os.MkdirAll(newPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := KnowledgeCacheDir(); got != newPath {
		t.Fatalf("flip = %q, want %q", got, newPath)
	}
	t.Setenv("KNOWLEDGE_CACHE_DIR", "/custom/cache")
	if got := KnowledgeCacheDir(); got != "/custom/cache" {
		t.Fatalf("override = %q, want /custom/cache", got)
	}
}

func TestKnowledgeBundleDefault(t *testing.T) {
	home := hermeticHome(t)
	if got, _ := KnowledgeBundleDefault(); got != filepath.Join(home, ".local", "share", "pi-stack", "knowledge") {
		t.Errorf("home = %q", got)
	}
	xd := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xd)
	if got, _ := KnowledgeBundleDefault(); got != filepath.Join(xd, "pi-stack", "knowledge") {
		t.Errorf("xdg = %q", got)
	}
}

func TestTasksRoot(t *testing.T) {
	home := hermeticHome(t)
	if got, _ := TasksRoot(); got != filepath.Join(home, ".local", "state", "pi-stack", "tasks") {
		t.Errorf("home = %q", got)
	}
	xs := t.TempDir()
	t.Setenv("XDG_STATE_HOME", xs)
	if got, _ := TasksRoot(); got != filepath.Join(xs, "pi-stack", "tasks") {
		t.Errorf("xdg = %q", got)
	}
}

func TestBrokerTokenPath(t *testing.T) {
	home := hermeticHome(t)
	if got, want := BrokerTokenPath(), filepath.Join(home, ".config", "pi-stack", "broker-token"); got != want {
		t.Errorf("BrokerTokenPath() = %q, want %q", got, want)
	}
	if BrokerTokenPath() != TokenPath() {
		t.Errorf("BrokerTokenPath %q != TokenPath %q", BrokerTokenPath(), TokenPath())
	}
}
