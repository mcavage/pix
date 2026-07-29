package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// These tests are the regression guard for config-default PETRIFICATION: the
// first `pix config set <anything>` used to freeze the then-current
// defaults (applied in memory by Load/applyDefaults) into config.toml forever,
// so a future default change (e.g. a new memory_watcher_model) never reached
// users. Save must persist ONLY explicit deviations from defaults.

// tempConfig points PIX_CONFIG at a fresh temp file and returns its path.
func tempConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("PIX_CONFIG", path)
	return path
}

func rawFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// (a) Setting an UNRELATED key must not petrify untouched defaults: after
// `config set google_workspace_account x` the raw file contains google_workspace_account but NOT the
// resolved memory_watcher_model / memory_embed_model / ollama_bridge_model /
// services defaults.
func TestSaveDoesNotPetrifyUntouchedDefaults(t *testing.T) {
	path := tempConfig(t)

	cfg, err := Load() // absent file -> defaults applied in memory
	if err != nil {
		t.Fatal(err)
	}
	cfg.SetGogAccount("x@example.com")
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	raw := rawFile(t, path)
	if !strings.Contains(raw, `google_workspace_account = "x@example.com"`) {
		t.Errorf("raw file missing the explicit google_workspace_account:\n%s", raw)
	}
	for _, key := range []string{"memory_watcher_model", "memory_embed_model", "ollama_bridge_model"} {
		if strings.Contains(raw, key) {
			t.Errorf("raw file petrified untouched default %q:\n%s", key, raw)
		}
	}
	if strings.Contains(raw, "services") {
		t.Errorf("raw file petrified the default services list:\n%s", raw)
	}

	// Reload from disk: the defaults still resolve for readers.
	got, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.MemoryWatcherModel != DefaultMemoryWatcherModel {
		t.Errorf("MemoryWatcherModel = %q, want default %q", got.MemoryWatcherModel, DefaultMemoryWatcherModel)
	}
	if got.GogAccount != "x@example.com" {
		t.Errorf("GogAccount = %q, want the explicit value", got.GogAccount)
	}
}

// (b) An explicit NON-default value round-trips: it is written to the file and
// resolves back to itself on load.
func TestSaveExplicitNonDefaultRoundTrips(t *testing.T) {
	path := tempConfig(t)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.MemoryWatcherModel = "my-custom-watcher"
	cfg.Services = []string{"memory", "knowledge"}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	raw := rawFile(t, path)
	if !strings.Contains(raw, `memory_watcher_model = "my-custom-watcher"`) {
		t.Errorf("raw file missing the explicit memory_watcher_model:\n%s", raw)
	}
	if !strings.Contains(raw, `services = ["memory", "knowledge"]`) {
		t.Errorf("raw file missing the explicit services list:\n%s", raw)
	}

	got, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.MemoryWatcherModel != "my-custom-watcher" {
		t.Errorf("MemoryWatcherModel = %q, want my-custom-watcher", got.MemoryWatcherModel)
	}
	if len(got.Services) != 2 || got.Services[1] != "knowledge" {
		t.Errorf("Services = %v, want [memory knowledge]", got.Services)
	}
}

// (c) A value set EQUAL to the current default is omitted from the file (the
// documented tradeoff) but still resolves to the default on load.
func TestSaveValueEqualToDefaultIsOmittedButResolves(t *testing.T) {
	path := tempConfig(t)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.MemoryWatcherModel = DefaultMemoryWatcherModel // explicit, but == default
	cfg.Services = append([]string(nil), DefaultServices...)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	raw := rawFile(t, path)
	if strings.Contains(raw, "memory_watcher_model") {
		t.Errorf("value equal to the default should be omitted:\n%s", raw)
	}
	if strings.Contains(raw, "services") {
		t.Errorf("services equal to DefaultServices should be omitted:\n%s", raw)
	}

	got, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.MemoryWatcherModel != DefaultMemoryWatcherModel {
		t.Errorf("MemoryWatcherModel = %q, want default %q", got.MemoryWatcherModel, DefaultMemoryWatcherModel)
	}
	if !slices.Equal(got.Services, DefaultServices) {
		t.Errorf("Services = %v, want DefaultServices %v", got.Services, DefaultServices)
	}
}

// (d) Simulate a DEFAULT CHANGE reaching an existing user: a config file with
// no watcher key must resolve to the CURRENT DefaultMemoryWatcherModel, proving
// future default bumps propagate to saved configs.
func TestLoadResolvesCurrentDefaultWhenKeyAbsent(t *testing.T) {
	path := tempConfig(t)
	if err := os.WriteFile(path, []byte("google_workspace_account = \"y@example.com\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.MemoryWatcherModel != DefaultMemoryWatcherModel {
		t.Errorf("MemoryWatcherModel = %q, want current default %q (default changes must reach users)",
			got.MemoryWatcherModel, DefaultMemoryWatcherModel)
	}
	if got.MemoryEmbedModel != DefaultMemoryEmbedModel || got.OllamaBridgeModel != DefaultOllamaBridgeModel {
		t.Errorf("embed/bridge models = %q/%q, want current defaults", got.MemoryEmbedModel, got.OllamaBridgeModel)
	}
	if got.GogAccount != "y@example.com" {
		t.Errorf("GogAccount = %q, want the explicit value", got.GogAccount)
	}
}

// A save-load-save cycle stays sparse: saving a loaded config (no changes)
// never grows the file with resolved defaults.
func TestSaveLoadSaveStaysSparse(t *testing.T) {
	path := tempConfig(t)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.SetGogAccount("z@example.com")
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	again, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := again.Save(); err != nil {
		t.Fatal(err)
	}
	raw := rawFile(t, path)
	for _, key := range []string{"memory_watcher_model", "memory_embed_model", "ollama_bridge_model"} {
		if strings.Contains(raw, key) {
			t.Errorf("save-load-save petrified %q:\n%s", key, raw)
		}
	}
}

// Removing the last explicitly enabled service must survive a Save -> Load ->
// Save round trip as an empty set.
func TestRemoveLastServiceRoundTripsExplicitEmpty(t *testing.T) {
	path := tempConfig(t)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Services = []string{"memory"}
	if !cfg.RemoveService("memory") {
		t.Fatalf("RemoveService(memory) reported no change; Services = %v", cfg.Services)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if raw := rawFile(t, path); strings.Contains(raw, "services =") {
		t.Fatalf("default-empty services should be omitted:\n%s", raw)
	}

	// Load -> the empty set MUST hold (not silently revert to DefaultServices).
	again, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Services) != 0 {
		t.Fatalf("reload restored services = %v, want [] (explicit empty lost)", again.Services)
	}

	// Save again (e.g. an unrelated `config set`) -> still resolves empty.
	again.SetGogAccount("x@example.com")
	if err := again.Save(); err != nil {
		t.Fatal(err)
	}
	if raw := rawFile(t, path); strings.Contains(raw, "services =") {
		t.Fatalf("second save petrified default-empty services:\n%s", raw)
	}
	final, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(final.Services) != 0 {
		t.Fatalf("final load restored services = %v, want []", final.Services)
	}
}

// A hand-written `services = []` in the file is explicit-empty too.
func TestExplicitEmptyServicesInFileStaysEmpty(t *testing.T) {
	path := tempConfig(t)
	if err := os.WriteFile(path, []byte("services = []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Services) != 0 {
		t.Errorf("services = %v, want [] (explicit empty in file)", got.Services)
	}
}

// A services list that becomes empty ONLY through removed-service filtering
// (stale `services = ["gws"]`) still falls back to defaults — that user never
// chose an empty set.
func TestRemovedServiceOnlyListFallsBackToDefaults(t *testing.T) {
	path := tempConfig(t)
	if err := os.WriteFile(path, []byte("services = [\"gws\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Services, DefaultServices) {
		t.Errorf("services = %v, want DefaultServices %v (stale gws-only list)", got.Services, DefaultServices)
	}
}

// TestSlackOAuthEmptyOmittedFromFile: an untouched (all-zero) Slack config
// never appears in the saved file at all — no bare `[slack]` table, same
// petrification guard as every other defaultable field.
func TestSlackOAuthEmptyOmittedFromFile(t *testing.T) {
	path := tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.SetGogAccount("x@example.com") // force a write, unrelated to slack
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	raw := rawFile(t, path)
	if strings.Contains(raw, "slack") {
		t.Errorf("raw file petrified an empty slack table:\n%s", raw)
	}
}

// TestSlackOAuthClientIDOnlySparseRoundTrip: setting only ClientID (the
// redirect uri resolves to the default in memory) saves WITHOUT redirect_uri
// — the same omit-the-resolved-default tradeoff every other defaultable field
// uses — and reload re-resolves the default.
func TestSlackOAuthClientIDOnlySparseRoundTrip(t *testing.T) {
	path := tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.SetSlackClientID("123.456")
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	raw := rawFile(t, path)
	if !strings.Contains(raw, `client_id = "123.456"`) {
		t.Errorf("raw file missing explicit client_id:\n%s", raw)
	}
	if strings.Contains(raw, "redirect_uri") {
		t.Errorf("raw file petrified the resolved default redirect_uri:\n%s", raw)
	}

	got, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Slack.ClientID != "123.456" {
		t.Errorf("ClientID = %q, want 123.456", got.Slack.ClientID)
	}
	if got.Slack.RedirectURI != DefaultSlackOAuthRedirectURI {
		t.Errorf("RedirectURI = %q, want default %q", got.Slack.RedirectURI, DefaultSlackOAuthRedirectURI)
	}
}

// TestSlackOAuthExplicitRedirectRoundTrips: a custom (non-default) redirect
// uri round-trips through Save/Load unchanged.
func TestSlackOAuthExplicitRedirectRoundTrips(t *testing.T) {
	path := tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.SetSlackClientID("123.456")
	cfg.SetSlackRedirectURI("https://example.com/slack/callback")
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	raw := rawFile(t, path)
	if !strings.Contains(raw, `redirect_uri = "https://example.com/slack/callback"`) {
		t.Errorf("raw file missing the explicit non-default redirect_uri:\n%s", raw)
	}

	got, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Slack.RedirectURI != "https://example.com/slack/callback" {
		t.Errorf("RedirectURI = %q, want the explicit value", got.Slack.RedirectURI)
	}
}

// TestSlackOAuthVaultDocumentAndGrantExpiresAtRoundTrip: the 1Password
// location fields and the cached grant expiry survive a full Save/Load cycle.
func TestSlackOAuthVaultDocumentAndGrantExpiresAtRoundTrip(t *testing.T) {
	path := tempConfig(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.SetSlackOAuthVaultID("Private")
	cfg.SetSlackOAuthDocumentID("itemid123")
	stamp := time.Date(2025, 8, 15, 0, 0, 0, 0, time.UTC)
	cfg.SetSlackOAuthGrantExpiresAt(stamp)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	raw := rawFile(t, path)
	for _, want := range []string{`oauth_vault_id = "Private"`, `oauth_document_id = "itemid123"`, "oauth_grant_expires_at = 2025-08-15T00:00:00Z"} {
		if !strings.Contains(raw, want) {
			t.Errorf("raw file missing %q:\n%s", want, raw)
		}
	}

	got, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Slack.OAuthVaultID != "Private" || got.Slack.OAuthDocumentID != "itemid123" {
		t.Errorf("vault/document = %q/%q", got.Slack.OAuthVaultID, got.Slack.OAuthDocumentID)
	}
	if !got.Slack.OAuthGrantExpiresAt.Equal(stamp) {
		t.Errorf("OAuthGrantExpiresAt = %v, want %v", got.Slack.OAuthGrantExpiresAt, stamp)
	}
}
