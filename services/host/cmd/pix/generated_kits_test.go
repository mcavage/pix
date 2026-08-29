package main

import (
	"os"
	"path/filepath"
	"testing"

	"pix/host/config"
	"pix/host/inference"
	"pix/host/workflow/launch"
)

func TestCleanupGeneratedKitDirsRemovesSynthesizedKitsOnly(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	cfg := &config.Config{Inference: config.InferenceConfig{
		Backends: map[string]config.InferenceBackend{
			"gateway": {Driver: "openai-compatible", Auth: "none", BaseURL: "http://127.0.0.1:9000/v1"},
		},
		Models: []config.InferenceModelBinding{
			{Model: "openai/gpt-5.6-sol", Backend: "gateway", Upstream: "reasoner", Available: true},
		},
	}}
	inferenceKit, err := inference.SynthesizeInferenceKit(cfg, inference.RosterInput{})
	if err != nil {
		t.Fatal(err)
	}
	contextRoot := config.ContextDir()
	if err := os.MkdirAll(contextRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(contextRoot, "AGENTS.md"), []byte("personal instructions\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	contextKit, err := launch.SynthesizePersonalContextKit()
	if err != nil {
		t.Fatal(err)
	}
	packKit := t.TempDir()

	if err := launch.CleanupGeneratedKitDirs([]string{inferenceKit, contextKit, inferenceKit}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{inferenceKit, contextKit} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("generated kit still exists at %s: %v", path, err)
		}
	}
	if _, err := os.Stat(packKit); err != nil {
		t.Fatalf("untracked pack kit was removed: %v", err)
	}
}
