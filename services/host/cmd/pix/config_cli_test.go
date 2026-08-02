package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"pix/host/config"
)

// TestApplyConfigChange_GogAccount: set writes the value, unset clears it.
func TestApplyConfigChange_GogAccount(t *testing.T) {
	cfg := defaultCfg()
	sum, err := applyConfigChange(cfg, false, "google_workspace_account", []string{"me@x.com"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GogAccount != "me@x.com" || !strings.Contains(sum, "me@x.com") {
		t.Errorf("set gog_account: cfg=%q summary=%q", cfg.GogAccount, sum)
	}
	if _, err := applyConfigChange(cfg, true, "google_workspace_account", nil); err != nil {
		t.Fatal(err)
	}
	if cfg.GogAccount != "" {
		t.Errorf("unset gog_account: cfg=%q, want empty", cfg.GogAccount)
	}
	// set with the wrong arity errors.
	if _, err := applyConfigChange(cfg, false, "google_workspace_account", nil); err == nil {
		t.Error("expected an arity error for set gog_account with no value")
	}
}

// TestApplyConfigChange_MCP: set adds (idempotent), unset removes.
func TestApplyConfigChange_MCP(t *testing.T) {
	cfg := defaultCfg()
	if _, err := applyConfigChange(cfg, false, "mcp", []string{config.GWServerName}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cfg.MCP, config.GWServerName) {
		t.Errorf("MCP = %v, want gog added", cfg.MCP)
	}
	// Adding again is a no-op (no duplicate).
	_, _ = applyConfigChange(cfg, false, "mcp", []string{config.GWServerName})
	if n := countStr(cfg.MCP, config.GWServerName); n != 1 {
		t.Errorf("MCP should contain gog exactly once, got %d in %v", n, cfg.MCP)
	}
	if _, err := applyConfigChange(cfg, true, "mcp", []string{config.GWServerName}); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(cfg.MCP, config.GWServerName) {
		t.Errorf("MCP = %v, want gog removed", cfg.MCP)
	}
	if _, err := applyConfigChange(cfg, false, "mcp", nil); err == nil {
		t.Error("expected an error for mcp with no server name")
	}
}

// TestApplyConfigChange_Services: set adds, unset removes.
func TestApplyConfigChange_Services(t *testing.T) {
	cfg := defaultCfg()
	if _, err := applyConfigChange(cfg, false, "services", []string{"knowledge"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cfg.Services, "knowledge") {
		t.Errorf("Services = %v, want knowledge added", cfg.Services)
	}
	if _, err := applyConfigChange(cfg, true, "services", []string{"knowledge"}); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(cfg.Services, "knowledge") {
		t.Errorf("Services = %v, want knowledge removed", cfg.Services)
	}
}

// TestApplyConfigChange_KnowledgeBundles: set adds the abs bundle path AND
// enables the knowledge service; unset removes the bundle. Adds are deduped and
// canonicalized; the value round-trips through Save/Load into config.toml.
func TestApplyConfigChange_KnowledgeBundles(t *testing.T) {
	t.Setenv("PIX_CONFIG", t.TempDir()+"/config.toml")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	abs, _ := filepath.Abs("bundles/okf")
	sum, err := applyConfigChange(cfg, false, "knowledge_bundles", []string{"bundles/okf"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cfg.KnowledgeBundles, abs) {
		t.Errorf("KnowledgeBundles = %v, want abs path %q added", cfg.KnowledgeBundles, abs)
	}
	// Setting a bundle must also ensure the knowledge service is enabled.
	if !slices.Contains(cfg.Services, "knowledge") {
		t.Errorf("Services = %v, want knowledge enabled", cfg.Services)
	}
	if !strings.Contains(sum, "knowledge") {
		t.Errorf("summary = %q, want it to mention knowledge", sum)
	}

	// Adding again is a no-op (dedupe on the canonical path).
	_, _ = applyConfigChange(cfg, false, "knowledge_bundles", []string{"bundles/okf"})
	if n := countStr(cfg.KnowledgeBundles, abs); n != 1 {
		t.Errorf("KnowledgeBundles should contain %q once, got %d in %v", abs, n, cfg.KnowledgeBundles)
	}

	// Save + reload: the config.toml carries the abs path and the service.
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got.KnowledgeBundles, abs) || !slices.Contains(got.Services, "knowledge") {
		t.Errorf("round-trip lost data: bundles=%v services=%v", got.KnowledgeBundles, got.Services)
	}

	// Unset removes the bundle (the knowledge service stays; unset targets the
	// bundle path only).
	if _, err := applyConfigChange(got, true, "knowledge_bundles", []string{"bundles/okf"}); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(got.KnowledgeBundles, abs) {
		t.Errorf("KnowledgeBundles = %v, want bundle removed", got.KnowledgeBundles)
	}

	// Arity error: no path.
	if _, err := applyConfigChange(got, false, "knowledge_bundles", nil); err == nil {
		t.Error("expected an arity error for knowledge_bundles with no value")
	}
}

// TestApplyConfigChange_Models: set overrides, unset resets to the default.
func TestApplyConfigChange_Models(t *testing.T) {
	cfg := defaultCfg()
	if _, err := applyConfigChange(cfg, false, "memory_watcher_model", []string{"llama3"}); err != nil {
		t.Fatal(err)
	}
	if cfg.MemoryWatcherModel != "llama3" {
		t.Errorf("watcher = %q, want llama3", cfg.MemoryWatcherModel)
	}
	if _, err := applyConfigChange(cfg, true, "memory_watcher_model", nil); err != nil {
		t.Fatal(err)
	}
	if cfg.MemoryWatcherModel != config.DefaultMemoryWatcherModel {
		t.Errorf("watcher after unset = %q, want default", cfg.MemoryWatcherModel)
	}
	if _, err := applyConfigChange(cfg, false, "memory_embed_model", []string{"embed-x"}); err != nil {
		t.Fatal(err)
	}
	if cfg.MemoryEmbedModel != "embed-x" {
		t.Errorf("embed = %q, want embed-x", cfg.MemoryEmbedModel)
	}
}

// TestApplyConfigChange_SlackClientID: set writes the public client id; unset
// clears it AND every OAuth locator/grant field (vault id, document id, cached
// grant expiry) so a stale locator can never point at a mismatched app once
// the client id it was minted under is gone.
func TestApplyConfigChange_SlackClientID(t *testing.T) {
	cfg := defaultCfg()
	if _, err := applyConfigChange(cfg, false, "slack.client_id", []string{"abc123.public"}); err != nil {
		t.Fatal(err)
	}
	if cfg.Slack.ClientID != "abc123.public" {
		t.Errorf("slack.client_id = %q, want abc123.public", cfg.Slack.ClientID)
	}
	// Managed state a real `pix slack setup` would have written alongside it.
	cfg.Slack.OAuthVaultID = "vault-1"
	cfg.Slack.OAuthDocumentID = "doc-1"
	cfg.Slack.OAuthGrantExpiresAt = time.Now()

	if _, err := applyConfigChange(cfg, true, "slack.client_id", nil); err != nil {
		t.Fatal(err)
	}
	if cfg.Slack.ClientID != "" {
		t.Errorf("unset slack.client_id: ClientID = %q, want empty", cfg.Slack.ClientID)
	}
	if cfg.Slack.OAuthVaultID != "" || cfg.Slack.OAuthDocumentID != "" || !cfg.Slack.OAuthGrantExpiresAt.IsZero() {
		t.Errorf("unset slack.client_id must also clear OAuth locator/grant fields, got vault=%q document=%q expiry=%v",
			cfg.Slack.OAuthVaultID, cfg.Slack.OAuthDocumentID, cfg.Slack.OAuthGrantExpiresAt)
	}

	// set with the wrong arity errors.
	if _, err := applyConfigChange(cfg, false, "slack.client_id", nil); err == nil {
		t.Error("expected an arity error for set slack.client_id with no value")
	}
}

// TestApplyConfigChange_SlackRedirectURI: set overrides the redirect uri;
// unset restores the built-in default when a client id is configured (so the
// OAuth flow still has somewhere to send Slack), but clears to empty when no
// client id is set (matching config.applyDefaults, which only ever resolves
// the default off a non-empty ClientID).
func TestApplyConfigChange_SlackRedirectURI(t *testing.T) {
	cfg := defaultCfg()
	cfg.Slack.ClientID = "abc123.public"
	if _, err := applyConfigChange(cfg, false, "slack.redirect_uri", []string{"http://localhost:9999/slack/callback"}); err != nil {
		t.Fatal(err)
	}
	if cfg.Slack.RedirectURI != "http://localhost:9999/slack/callback" {
		t.Errorf("slack.redirect_uri = %q, want the override", cfg.Slack.RedirectURI)
	}
	if _, err := applyConfigChange(cfg, true, "slack.redirect_uri", nil); err != nil {
		t.Fatal(err)
	}
	if cfg.Slack.RedirectURI != config.DefaultSlackOAuthRedirectURI {
		t.Errorf("unset slack.redirect_uri with a client id set = %q, want default %q", cfg.Slack.RedirectURI, config.DefaultSlackOAuthRedirectURI)
	}

	// No client id configured: unset clears to empty rather than resolving a
	// default nothing will use.
	cfg2 := defaultCfg()
	cfg2.Slack.RedirectURI = "http://localhost:9999/slack/callback"
	if _, err := applyConfigChange(cfg2, true, "slack.redirect_uri", nil); err != nil {
		t.Fatal(err)
	}
	if cfg2.Slack.RedirectURI != "" {
		t.Errorf("unset slack.redirect_uri with no client id = %q, want empty", cfg2.Slack.RedirectURI)
	}

	if _, err := applyConfigChange(cfg, false, "slack.redirect_uri", nil); err == nil {
		t.Error("expected an arity error for set slack.redirect_uri with no value")
	}
}

// TestApplyConfigChange_SlackManagedFieldsNotSettable: the OAuth locator/grant
// fields are managed state written only by `pix slack setup` (and cleared by
// unset slack.client_id) — set/unset on them directly is refused so a stale
// hand-set vault/document id can never point at credentials that don't match
// the current client id.
func TestApplyConfigChange_SlackManagedFieldsNotSettable(t *testing.T) {
	for _, key := range []string{"slack.oauth_vault_id", "slack.oauth_document_id", "slack.oauth_grant_expires_at"} {
		cfg := defaultCfg()
		if _, err := applyConfigChange(cfg, false, key, []string{"x"}); err == nil {
			t.Errorf("expected set %s to be refused (managed by pix slack setup)", key)
		}
		if _, err := applyConfigChange(cfg, true, key, nil); err == nil {
			t.Errorf("expected unset %s to be refused (managed by pix slack setup)", key)
		}
	}
}

// TestApplyConfigChange_UnknownKey errors and lists the supported keys.
func TestApplyConfigChange_UnknownKey(t *testing.T) {
	_, err := applyConfigChange(defaultCfg(), false, "nope", []string{"x"})
	if err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Errorf("expected unknown-key error, got %v", err)
	}
}

// TestConfigSaveRoundTrip proves the write half of the repo-less workflow: a
// set applied + Save()d + Load()ed back preserves the value (no hand-editing).
func TestConfigSaveRoundTrip(t *testing.T) {
	t.Setenv("PIX_CONFIG", t.TempDir()+"/config.toml")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyConfigChange(cfg, false, "google_workspace_account", []string{"round@trip.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := applyConfigChange(cfg, false, "mcp", []string{config.GWServerName}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.GogAccount != "round@trip.com" || !slices.Contains(got.MCP, config.GWServerName) {
		t.Errorf("round-trip lost data: %+v", got)
	}
}

func countStr(list []string, s string) int {
	n := 0
	for _, v := range list {
		if v == s {
			n++
		}
	}
	return n
}
