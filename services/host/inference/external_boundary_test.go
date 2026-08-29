package inference_test

import (
	"os"
	"path/filepath"
	"testing"

	"pix/host/config"
	"pix/host/inference"
)

// externalBoundaryCfg mirrors roster_test.go's package-internal testCfg
// using only exported config types: this file lives in package
// inference_test (a genuinely different, external package) precisely so it
// cannot reach that helper, or anything else unexported in package
// inference, at all.
func externalBoundaryCfg() *config.Config {
	return &config.Config{Inference: config.InferenceConfig{
		Backends: map[string]config.InferenceBackend{
			"zai": {Driver: "openai-compatible", Auth: "none", BaseURL: "https://api.z.ai/api/paas/v4"},
		},
		Models: []config.InferenceModelBinding{
			{Model: "zai/glm-5", Backend: "zai", Upstream: "glm-5", Available: true},
		},
	}}
}

// TestExternalPackage_RuntimeManifestAndSynthesizeInferenceKit is
// the compile-and-run proof of the E3.1 boundary decision: an external
// caller (this file, and in practice cmd/pix or a future E3.3 consumer)
// constructs nothing but the exported inference.RosterInput and hands it
// straight to RuntimeManifest / SynthesizeInferenceKit — the only
// two functions this package exports for composing a roster into a
// runtime manifest. Because this file is package inference_test, not
// inference, it would fail to BUILD (not just fail an assertion) the
// moment either function's signature grew a parameter or result type this
// package does not export — the same guarantee
// TestPublicAPINeverExposesUnexported proves by static analysis from the
// inside, proved here by an actual external compilation.
func TestExternalPackage_RuntimeManifestAndSynthesizeInferenceKit(t *testing.T) {
	cfg := externalBoundaryCfg()
	roster := inference.RosterInput{
		Main:          "zai/glm-5",
		Agents:        map[string]string{"engineer": "zai/glm-5"},
		ShippedAgents: []string{"engineer"},
	}

	manifest, err := inference.RuntimeManifest(cfg, roster)
	if err != nil {
		t.Fatalf("RuntimeManifest() error = %v", err)
	}
	if len(manifest.Models) == 0 {
		t.Fatal("RuntimeManifest() produced no models from a one-backend, one-model config")
	}

	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	dir, err := inference.SynthesizeInferenceKit(cfg, roster)
	if err != nil {
		t.Fatalf("SynthesizeInferenceKit() error = %v", err)
	}
	defer os.RemoveAll(dir)
	if _, err := os.Stat(filepath.Join(dir, "spec.yaml")); err != nil {
		t.Fatalf("synthesized kit missing spec.yaml: %v", err)
	}
}
