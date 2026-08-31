package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/workflow/launch"
)

func TestPersonalContextUsesXDGDataAndGeneratesAgentLayer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	if want := filepath.Join(home, "context"); config.ContextDir() != want {
		t.Fatalf("ContextDir = %q, want %q", config.ContextDir(), want)
	}
	if err := os.MkdirAll(config.ContextDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config.ContextDir(), "AGENTS.md"), []byte("Prefer concise answers.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	kit, err := launch.SynthesizePersonalContextKit()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(kit, "spec.yaml"))
	if err != nil || !strings.Contains(string(b), "agentInstructions:\n  content: |\n    Prefer concise answers.") {
		t.Fatalf("spec = %q, err=%v", b, err)
	}
}
