package inference

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"pix/host/config"
)

// TestSynthesizeInferenceKitMixinLayout pins the exact create-time mixin
// layout SynthesizeInferenceKit must produce. This is a regression guard for
// the filename bug where the manifest was written as a file literally named
// "json" (filepath.Join(agentDir, "json")) instead of "inference.json": the
// mixin directory looked complete (two files present, both non-empty), the
// spec.yaml carried valid permissions/credentials, and the sandbox still
// booted — but extensions/inference.ts and extensions/ollama-bridge.ts both
// hardcode `path.join(getAgentDir(), "inference.json")`, so they silently
// found nothing, registered no generated docker-* provider, and pi fell back
// to whatever native Anthropic credential was present (a 401 with no error
// pointing at this directory). Assert the POSITIVE (inference.json exists and
// parses as the manifest) and the NEGATIVE (no stray "json" file, nothing
// else in the agent dir) in the same test so a future rename cannot silently
// reintroduce either half of the bug.
func TestSynthesizeInferenceKitMixinLayout(t *testing.T) {
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

	dir, err := SynthesizeInferenceKit(cfg, RosterInput{})
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	agentDir := filepath.Join(dir, "files", "home", ".pi", "agent")
	entries, err := os.ReadDir(agentDir)
	if err != nil {
		t.Fatalf("agent dir missing: %v", err)
	}
	names := make(map[string]bool, len(entries))
	for _, e := range entries {
		names[e.Name()] = true
	}

	// Positive: the exact two generated files, nothing else.
	want := map[string]bool{"inference.json": true, "routing.json": true}
	if len(names) != len(want) {
		t.Fatalf("agent dir has unexpected entries: got %v, want exactly %v", names, want)
	}
	for name := range want {
		if !names[name] {
			t.Fatalf("agent dir missing %q: got %v", name, names)
		}
	}

	// Negative: the historical bug's filename must never reappear.
	if names["json"] {
		t.Fatalf("agent dir contains the stray %q file from the filename bug: %v", "json", names)
	}

	// inference.json must exist AND parse as the manifest the extensions
	// expect (version 1, backends map, models slice) — a present-but-empty or
	// truncated file would pass a bare os.Stat check and still be invisible
	// to the extension's shape guard.
	raw, err := os.ReadFile(filepath.Join(agentDir, inferenceManifestFilename))
	if err != nil {
		t.Fatalf("inference.json unreadable: %v", err)
	}
	var manifest runtimeInferenceManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("inference.json did not parse: %v\ncontent: %s", err, raw)
	}
	if manifest.Version != 1 {
		t.Fatalf("inference.json version = %d, want 1", manifest.Version)
	}
	if len(manifest.Models) == 0 {
		t.Fatalf("inference.json has no models: %+v", manifest)
	}

	// routing.json must also exist and parse — the mixin's other generated
	// file, written by the same call, unaffected by the manifest bug but
	// asserted here so this test is the one place the whole layout is pinned.
	routingRaw, err := os.ReadFile(filepath.Join(agentDir, "routing.json"))
	if err != nil {
		t.Fatalf("routing.json unreadable: %v", err)
	}
	var routingDoc map[string]any
	if err := json.Unmarshal(routingRaw, &routingDoc); err != nil {
		t.Fatalf("routing.json did not parse: %v\ncontent: %s", err, routingRaw)
	}
}
