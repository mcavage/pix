package main

import (
	"pix/host/workflow/provision"
	"strings"
	"testing"
)

// TestConfigValue is the table-driven contract for `config get`: one resolved
// value per key, no decoration, lists space-separated, unknown keys loud. The
// Makefile's operational targets shell out to this, so an unexpected format
// here breaks `make run`/`make serve` sourcing.
func TestConfigValue(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{testMCPServer, "slack"}
	cfg.Services = []string{"memory", "knowledge"}
	cfg.MemoryWatcherModel = "qwen3.5:9b"
	cfg.MemoryEmbedModel = "nomic-embed-text"
	cfg.OllamaBridgeModel = "qwen3.5:9b"
	cfg.RunIntent = "overlord"
	cfg.MemoryCapture = "experimental-auto"
	cfg.Pack = "/packs/work"

	tests := []struct {
		key     string
		want    string
		wantErr bool
	}{
		{key: "mcp", want: testMCPServer + " slack"},
		{key: "services", want: "memory knowledge"},
		{key: "memory_watcher_model", want: "qwen3.5:9b"},
		{key: "memory_embed_model", want: "nomic-embed-text"},
		{key: "ollama_bridge_model", want: "qwen3.5:9b"},
		{key: "run_intent", want: "overlord"},
		{key: "memory_capture", want: "experimental-auto"},
		{key: "pack", want: "/packs/work"},
		{key: "host.autoserve", want: "true"},
		{key: "nope", wantErr: true},
		{key: "", wantErr: true},
		// The retired per-vendor account key reads as any other unknown key.
		{key: "google_workspace_account", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			got, err := provision.ConfigValue(cfg, tt.key)
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "unknown key") {
					t.Errorf("provision.ConfigValue(%q): expected unknown-key error, got %v", tt.key, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("provision.ConfigValue(%q): %v", tt.key, err)
			}
			if got != tt.want {
				t.Errorf("provision.ConfigValue(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

// TestConfigValue_KnowledgeBundlesRetired: knowledge_bundles (the built-in OKF
// knowledge service, retired W2 U03A) is a distinct refusal, not a plain
// "unknown key" — the caller should be told it once did something, not that
// it's a typo.
func TestConfigValue_KnowledgeBundlesRetired(t *testing.T) {
	cfg := defaultCfg()
	if _, err := provision.ConfigValue(cfg, "knowledge_bundles"); err == nil {
		t.Error("expected config get knowledge_bundles to refuse (retired key)")
	}
}

// TestConfigValue_EmptyList: an empty list key prints as an empty string (MCP
// legitimately empty = dynamic-discovery mode), NOT an error — the Makefile
// treats empty-MCP as valid config, and only a missing binary as fatal.
func TestConfigValue_EmptyList(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = nil
	got, err := provision.ConfigValue(cfg, "mcp")
	if err != nil || got != "" {
		t.Errorf("provision.ConfigValue(mcp) on empty list = %q, %v; want \"\", nil", got, err)
	}
}
