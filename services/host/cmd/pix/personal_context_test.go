package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
)

func TestPersonalContextUsesXDGDataAndGeneratesAgentLayer(t *testing.T) {
	data := t.TempDir()
	state := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("XDG_STATE_HOME", state)
	if want := filepath.Join(data, "pix", "context"); config.ContextDir() != want {
		t.Fatalf("ContextDir = %q, want %q", config.ContextDir(), want)
	}
	if err := os.MkdirAll(config.ContextDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config.ContextDir(), "AGENTS.md"), []byte("Prefer concise answers.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	kit, err := synthesizePersonalContextKit()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(kit, "spec.yaml"))
	if err != nil || !strings.Contains(string(b), "agentInstructions:\n  content: |\n    Prefer concise answers.") {
		t.Fatalf("spec = %q, err=%v", b, err)
	}
}
