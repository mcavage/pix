package main

import (
	"strings"
	"testing"
	"time"
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
	cfg.Slack.ClientID = "abc123.public"
	cfg.Slack.RedirectURI = "http://localhost:17373/slack/callback"
	cfg.Slack.OAuthVaultID = "vault-1"
	cfg.Slack.OAuthDocumentID = "doc-1"
	grantExpiry := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	cfg.Slack.OAuthGrantExpiresAt = grantExpiry

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
		{key: "slack.client_id", want: "abc123.public"},
		{key: "slack.redirect_uri", want: "http://localhost:17373/slack/callback"},
		{key: "slack.oauth_vault_id", want: "vault-1"},
		{key: "slack.oauth_document_id", want: "doc-1"},
		{key: "slack.oauth_grant_expires_at", want: grantExpiry.Format(time.RFC3339)},
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

// TestConfigValue_SlackGrantExpiresAtZero: an unset (zero-time) grant expiry
// prints as an empty string, not "0001-01-01...", so a diagnostic read looks
// like "not set" rather than a bogus date.
func TestConfigValue_SlackGrantExpiresAtZero(t *testing.T) {
	cfg := defaultCfg()
	got, err := configValue(cfg, "slack.oauth_grant_expires_at")
	if err != nil || got != "" {
		t.Errorf("configValue(slack.oauth_grant_expires_at) on zero time = %q, %v; want \"\", nil", got, err)
	}
}
