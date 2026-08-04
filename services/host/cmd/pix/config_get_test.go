package main

import (
	"pix/host/workflow/setup"
	"strings"
	"testing"
)

// TestConfigValue is the table-driven contract for `config get`: one resolved
// value per key, no decoration, lists space-separated, unknown keys loud. The
// Makefile's operational targets shell out to this, so an unexpected format
// here breaks `make run`/`make serve` sourcing.
func TestConfigValue(t *testing.T) {
	cfg := defaultCfg()
	cfg.GogAccount = "me@x.com"
	cfg.MCP = []string{"gog", "slack"}
	cfg.Services = []string{"memory", "knowledge"}
	cfg.KnowledgeBundles = []string{"/kb/a", "/kb/b"}
	cfg.MemoryWatcherModel = "qwen3.5:9b"
	cfg.MemoryEmbedModel = "nomic-embed-text"
	cfg.OllamaBridgeModel = "qwen3.5:9b"

	tests := []struct {
		key     string
		want    string
		wantErr bool
	}{
		{key: "google_workspace_account", want: "me@x.com"},
		{key: "mcp", want: "gog slack"},
		{key: "services", want: "memory knowledge"},
		{key: "knowledge_bundles", want: "/kb/a /kb/b"},
		{key: "memory_watcher_model", want: "qwen3.5:9b"},
		{key: "memory_embed_model", want: "nomic-embed-text"},
		{key: "ollama_bridge_model", want: "qwen3.5:9b"},
		{key: "nope", wantErr: true},
		{key: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			got, err := setup.ConfigValue(cfg, tt.key)
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "unknown key") {
					t.Errorf("setup.ConfigValue(%q): expected unknown-key error, got %v", tt.key, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("setup.ConfigValue(%q): %v", tt.key, err)
			}
			if got != tt.want {
				t.Errorf("setup.ConfigValue(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

// TestConfigValue_EmptyList: an empty list key prints as an empty string (MCP
// legitimately empty = dynamic-discovery mode), NOT an error — the Makefile
// treats empty-MCP as valid config, and only a missing binary as fatal.
func TestConfigValue_EmptyList(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = nil
	got, err := setup.ConfigValue(cfg, "mcp")
	if err != nil || got != "" {
		t.Errorf("setup.ConfigValue(mcp) on empty list = %q, %v; want \"\", nil", got, err)
	}
}

