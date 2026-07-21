package main

import (
	"os"
	"path/filepath"
	"testing"

	"pi-stack/host/config"
)

// TestWritePackContextFiles_WritesMemoryScopeAndOllamaModel is the regression
// test for the packs-v2 Phase 1 gap: `pi-stack task new` called
// applyPackToLaunch (so cfg/o picked up the pack's overrides) but never wrote
// the per-launch workspace files that carry that context INTO the sandbox, so
// a task lost the active pack's memory scope and ollama-bridge model. Both
// runRun and launchTask now call writePackContextFiles (the extracted,
// testable helper) with the SAME arguments, so this test exercises the exact
// path a task workspace goes through.
func TestWritePackContextFiles_WritesMemoryScopeAndOllamaModel(t *testing.T) {
	packRoot := t.TempDir()
	manifest := "name = \"work\"\nschema = 1\n"
	if err := os.WriteFile(filepath.Join(packRoot, packManifestName), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	ws := t.TempDir()
	cfg := &config.Config{Pack: packRoot, OllamaBridgeModel: "qwen3.5:9b"}
	o := runOpts{Workspace: ws}

	writePackContextFiles(cfg, o)

	gotScope := readFile(t, filepath.Join(ws, ".pi-stack", "profile"))
	if got := trimTrailingNewline(gotScope); got != "work" {
		t.Errorf("profile = %q, want %q", got, "work")
	}
	gotModel := readFile(t, filepath.Join(ws, ".pi-stack", "ollama-bridge.model"))
	if got := trimTrailingNewline(gotModel); got != "qwen3.5:9b" {
		t.Errorf("ollama-bridge.model = %q, want %q", got, "qwen3.5:9b")
	}
}

// TestWritePackContextFiles_NoPack_UnscopedMemoryDefaultModel: with no active
// pack, memory stays unscoped (no profile file) and the ollama-bridge model
// falls back to the config default rather than an empty file.
func TestWritePackContextFiles_NoPack_UnscopedMemoryDefaultModel(t *testing.T) {
	ws := t.TempDir()
	cfg := &config.Config{}
	o := runOpts{Workspace: ws}

	writePackContextFiles(cfg, o)

	if _, err := os.Stat(filepath.Join(ws, ".pi-stack", "profile")); err == nil {
		t.Error("no active pack should leave memory unscoped (no profile file)")
	}
	gotModel := trimTrailingNewline(readFile(t, filepath.Join(ws, ".pi-stack", "ollama-bridge.model")))
	if gotModel != config.DefaultOllamaBridgeModel {
		t.Errorf("ollama-bridge.model = %q, want default %q", gotModel, config.DefaultOllamaBridgeModel)
	}
}

// TestWritePackContextFiles_PackOverrideWins mirrors run.go's --pack override
// precedence (activePackRoot: override wins over cfg.Pack).
func TestWritePackContextFiles_PackOverrideWins(t *testing.T) {
	packRoot := t.TempDir()
	manifest := "name = \"personal\"\nschema = 1\nmemory_scope = \"personal-scope\"\n"
	if err := os.WriteFile(filepath.Join(packRoot, packManifestName), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	ws := t.TempDir()
	cfg := &config.Config{Pack: "/nonexistent/pack"}
	o := runOpts{Workspace: ws, Pack: packRoot}

	writePackContextFiles(cfg, o)

	gotScope := trimTrailingNewline(readFile(t, filepath.Join(ws, ".pi-stack", "profile")))
	if gotScope != "personal-scope" {
		t.Errorf("profile = %q, want %q (o.Pack override should win over cfg.Pack)", gotScope, "personal-scope")
	}
}

func trimTrailingNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
