package main

import (
	"strings"
	"testing"

	"pi-stack/host/config"
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
	cfg.ActiveProfile = "work"

	tests := []struct {
		key     string
		want    string
		wantErr bool
	}{
		{key: "gog_account", want: "me@x.com"},
		{key: "mcp", want: "gog slack"},
		{key: "services", want: "memory knowledge"},
		{key: "knowledge_bundles", want: "/kb/a /kb/b"},
		{key: "memory_watcher_model", want: "qwen3.5:9b"},
		{key: "memory_embed_model", want: "nomic-embed-text"},
		{key: "ollama_bridge_model", want: "qwen3.5:9b"},
		{key: "active_profile", want: "work"},
		{key: "nope", wantErr: true},
		{key: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			got, err := configValue(cfg, tt.key)
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "unknown key") {
					t.Errorf("configValue(%q): expected unknown-key error, got %v", tt.key, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("configValue(%q): %v", tt.key, err)
			}
			if got != tt.want {
				t.Errorf("configValue(%q) = %q, want %q", tt.key, got, tt.want)
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
	got, err := configValue(cfg, "mcp")
	if err != nil || got != "" {
		t.Errorf("configValue(mcp) on empty list = %q, %v; want \"\", nil", got, err)
	}
}

// TestConfigValue_ProfileOverride: `config get` is profile-aware via
// config.Resolve — a profile's present override REPLACES the base value, an
// absent field INHERITS it, exactly like every other consumer of Resolve.
func TestConfigValue_ProfileOverride(t *testing.T) {
	cfg := defaultCfg()
	cfg.GogAccount = "me@home.com"
	cfg.MCP = []string{"slack"}
	cfg.MemoryWatcherModel = "qwen3.5:9b"
	work := []string{"gog"}
	cfg.Profiles = map[string]config.Profile{
		"work": {GogAccount: "me@work.com", MCP: &work},
	}

	tests := []struct {
		name    string
		profile string
		key     string
		want    string
	}{
		{name: "base gog_account", profile: "", key: "gog_account", want: "me@home.com"},
		{name: "override gog_account", profile: "work", key: "gog_account", want: "me@work.com"},
		{name: "base mcp", profile: "", key: "mcp", want: "slack"},
		{name: "override mcp replaces", profile: "work", key: "mcp", want: "gog"},
		// memory_* is global (no per-profile override exists): inherit the base.
		{name: "global model inherits", profile: "work", key: "memory_watcher_model", want: "qwen3.5:9b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := configValue(cfg.Resolve(tt.profile), tt.key)
			if err != nil {
				t.Fatalf("configValue: %v", err)
			}
			if got != tt.want {
				t.Errorf("profile %q key %q = %q, want %q", tt.profile, tt.key, got, tt.want)
			}
		})
	}
}
