package main

import (
	"os"
	"path/filepath"
	"testing"

	"pix/host/config"
)

func TestPersistPackStackComposesAllFacetsAndKeepsPerPackOwnership(t *testing.T) {
	state := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(state, "config.toml"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(state, "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(state, "state"))

	manualKnowledge := filepath.Join(state, "manual-knowledge")
	firstKnowledge := filepath.Join(state, "first-knowledge")
	secondKnowledge := filepath.Join(state, "second-knowledge")
	for _, dir := range []string{manualKnowledge, firstKnowledge, secondKnowledge} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	baseline := &config.Config{MCP: []string{"manual-mcp"}, OllamaBridgeModel: "manual-model"}
	baseline.SetGogAccount("manual@example.com")
	baseline.AddKnowledgeBundle(manualKnowledge)
	if err := baseline.Save(); err != nil {
		t.Fatal(err)
	}

	first, second := t.TempDir(), t.TempDir()
	mustWritePack(t, first, packManifest{
		Name: "first", Schema: 1, GogAccount: "first@example.com", OllamaBridgeModel: "first-model",
		Integrations: []packIntegration{{Name: "first", MCP: "first-mcp"}},
		Knowledge:    []packKnowledge{{Name: "first-ref", Source: firstKnowledge}},
	})
	mustWritePack(t, second, packManifest{
		Name: "second", Schema: 1, GogAccount: "second@example.com",
		Integrations: []packIntegration{{Name: "second", MCP: "second-mcp"}},
		Knowledge:    []packKnowledge{{Name: "second-ref", Source: secondKnowledge}},
	})

	if err := persistPackStack([]string{first, second}); err != nil {
		t.Fatal(err)
	}
	assertComposed := func() {
		t.Helper()
		cfg, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.GogAccount != "second@example.com" || cfg.OllamaBridgeModel != "first-model" {
			t.Fatalf("last-writer scalar composition failed: gog=%q ollama=%q", cfg.GogAccount, cfg.OllamaBridgeModel)
		}
		for _, name := range []string{"manual-mcp", "first-mcp", "second-mcp"} {
			if !containsStr(cfg.MCP, name) {
				t.Fatalf("MCP %q missing from %v", name, cfg.MCP)
			}
		}
		for _, dir := range []string{manualKnowledge, firstKnowledge, secondKnowledge} {
			if !containsStr(cfg.KnowledgeBundles, canonicalizeKnowledgeBundle(dir)) {
				t.Fatalf("knowledge %q missing from %v", dir, cfg.KnowledgeBundles)
			}
		}
	}
	assertComposed()

	store, err := loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	if store.Activation != nil || len(store.Activations) != 2 {
		t.Fatalf("activation ledger = single:%+v stack:%+v", store.Activation, store.Activations)
	}
	if !containsStr(store.Activations[0].Knowledge, canonicalizeKnowledgeBundle(firstKnowledge)) ||
		containsStr(store.Activations[0].Knowledge, canonicalizeKnowledgeBundle(secondKnowledge)) ||
		!containsStr(store.Activations[1].Knowledge, canonicalizeKnowledgeBundle(secondKnowledge)) {
		t.Fatalf("knowledge ownership is not per-pack: %+v", store.Activations)
	}
	if store.Activations[0].PriorGogAccount != "manual@example.com" ||
		store.Activations[1].PriorGogAccount != "first@example.com" {
		t.Fatalf("scalar restore chain = %+v", store.Activations)
	}

	// Re-composition must unwind the existing ledger first, not claim user
	// entries or accumulate duplicate contributions.
	if err := persistPackStack([]string{first, second}); err != nil {
		t.Fatal(err)
	}
	assertComposed()
	store, err = loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	for i := len(store.Activations) - 1; i >= 0; i-- {
		revertPackPriorContribution(cfg, store.activationFor(store.Activations[i].Path))
	}
	if cfg.GogAccount != "manual@example.com" || cfg.OllamaBridgeModel != "manual-model" {
		t.Fatalf("reverse removal did not restore baseline scalars: gog=%q ollama=%q", cfg.GogAccount, cfg.OllamaBridgeModel)
	}
	if !containsStr(cfg.MCP, "manual-mcp") || containsStr(cfg.MCP, "first-mcp") || containsStr(cfg.MCP, "second-mcp") {
		t.Fatalf("reverse removal violated MCP ownership: %v", cfg.MCP)
	}
	if !containsStr(cfg.KnowledgeBundles, canonicalizeKnowledgeBundle(manualKnowledge)) ||
		containsStr(cfg.KnowledgeBundles, canonicalizeKnowledgeBundle(firstKnowledge)) ||
		containsStr(cfg.KnowledgeBundles, canonicalizeKnowledgeBundle(secondKnowledge)) {
		t.Fatalf("reverse removal violated knowledge ownership: %v", cfg.KnowledgeBundles)
	}
}
