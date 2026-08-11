package pack

import (
	"path/filepath"
	"pix/host/packinfo"
	"slices"
	"testing"

	"pix/host/config"
)

func TestPersistPackStackComposesAllFacetsAndKeepsPerPackOwnership(t *testing.T) {
	state := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(state, "config.toml"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(state, "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(state, "state"))

	baseline := &config.Config{MCP: []string{"manual-mcp"}, OllamaBridgeModel: "manual-model"}
	if err := baseline.Save(); err != nil {
		t.Fatal(err)
	}

	// ollama_bridge_model is the ONE pack-declared config scalar left (gog_account
	// went with the built-in Google Workspace surface), so it carries the whole
	// last-writer-wins + per-pack-prior-value contract on its own: `first` sets
	// it, `second` overwrites it, and each activation records the value it
	// displaced so a reverse unwind lands exactly back on the baseline.
	first, second := t.TempDir(), t.TempDir()
	mustWritePack(t, first, packinfo.Manifest{
		Name: "first", Schema: 1, OllamaBridgeModel: "first-model",
		Integrations: []packinfo.Integration{{Name: "first", MCP: "first-mcp", Command: "first-mcp-bin"}},
	})
	mustWritePack(t, second, packinfo.Manifest{
		Name: "second", Schema: 1, OllamaBridgeModel: "second-model",
		Integrations: []packinfo.Integration{{Name: "second", MCP: "second-mcp", URL: "https://second.example.test/mcp"}},
	})

	if err := PersistPackStack([]string{first, second}); err != nil {
		t.Fatal(err)
	}
	assertComposed := func() {
		t.Helper()
		cfg, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.OllamaBridgeModel != "second-model" {
			t.Fatalf("last-writer scalar composition failed: ollama=%q", cfg.OllamaBridgeModel)
		}
		for _, name := range []string{"manual-mcp", "first-mcp", "second-mcp"} {
			if !slices.Contains(cfg.MCP, name) {
				t.Fatalf("MCP %q missing from %v", name, cfg.MCP)
			}
		}
	}
	assertComposed()

	store, err := loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	if len(store.Activations) != 2 {
		t.Fatalf("activation ledger = %+v", store.Activations)
	}
	if store.Activations[0].PriorOllamaBridgeModel != "manual-model" ||
		store.Activations[1].PriorOllamaBridgeModel != "first-model" {
		t.Fatalf("scalar restore chain = %+v", store.Activations)
	}

	// Re-composition must unwind the existing ledger first, not claim user
	// entries or accumulate duplicate contributions.
	if err := PersistPackStack([]string{first, second}); err != nil {
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
	if cfg.OllamaBridgeModel != "manual-model" {
		t.Fatalf("reverse removal did not restore the baseline scalar: ollama=%q", cfg.OllamaBridgeModel)
	}
	if !slices.Contains(cfg.MCP, "manual-mcp") || slices.Contains(cfg.MCP, "first-mcp") || slices.Contains(cfg.MCP, "second-mcp") {
		t.Fatalf("reverse removal violated MCP ownership: %v", cfg.MCP)
	}
}
