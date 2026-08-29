package main

import (
	"slices"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/workflow/provision"
)

// TestApplyConfigChange_GoogleWorkspaceAccountRetired: the per-vendor account
// key is gone — an integration's extra environment now comes from the pack's
// `env_keys`, resolved through op-refs.env, and core names no vendor. Both set
// and unset must REFUSE rather than silently no-op: a caller who is told
// nothing walks away believing they configured an account somebody reads.
func TestApplyConfigChange_GoogleWorkspaceAccountRetired(t *testing.T) {
	cfg := defaultCfg()
	if _, err := provision.ApplyConfigChange(cfg, false, "google_workspace_account", []string{"me@x.com"}); err == nil {
		t.Error("expected config set google_workspace_account to refuse (retired key)")
	}
	if _, err := provision.ApplyConfigChange(cfg, true, "google_workspace_account", nil); err == nil {
		t.Error("expected config unset google_workspace_account to refuse (retired key)")
	}
}

// TestApplyConfigChange_MCP: set adds (idempotent), unset removes.
func TestApplyConfigChange_MCP(t *testing.T) {
	cfg := defaultCfg()
	if _, err := provision.ApplyConfigChange(cfg, false, "mcp", []string{testMCPServer}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cfg.MCP, testMCPServer) {
		t.Errorf("MCP = %v, want %s added", cfg.MCP, testMCPServer)
	}
	// Adding again is a no-op (no duplicate).
	_, _ = provision.ApplyConfigChange(cfg, false, "mcp", []string{testMCPServer})
	if n := countStr(cfg.MCP, testMCPServer); n != 1 {
		t.Errorf("MCP should contain %s exactly once, got %d in %v", testMCPServer, n, cfg.MCP)
	}
	if _, err := provision.ApplyConfigChange(cfg, true, "mcp", []string{testMCPServer}); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(cfg.MCP, testMCPServer) {
		t.Errorf("MCP = %v, want %s removed", cfg.MCP, testMCPServer)
	}
	if _, err := provision.ApplyConfigChange(cfg, false, "mcp", nil); err == nil {
		t.Error("expected an error for mcp with no server name")
	}
}

// TestApplyConfigChange_Services: set adds, unset removes.
func TestApplyConfigChange_Services(t *testing.T) {
	cfg := defaultCfg()
	if _, err := provision.ApplyConfigChange(cfg, false, "services", []string{"knowledge"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cfg.Services, "knowledge") {
		t.Errorf("Services = %v, want knowledge added", cfg.Services)
	}
	if _, err := provision.ApplyConfigChange(cfg, true, "services", []string{"knowledge"}); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(cfg.Services, "knowledge") {
		t.Errorf("Services = %v, want knowledge removed", cfg.Services)
	}
}

// TestApplyConfigChange_KnowledgeBundlesRetired: knowledge_bundles (the
// built-in OKF knowledge service, retired W2 U03A) must REFUSE both set and
// unset — there is no replacement config field, so a caller must be told
// plainly rather than silently no-op'd.
func TestApplyConfigChange_KnowledgeBundlesRetired(t *testing.T) {
	cfg := defaultCfg()
	if _, err := provision.ApplyConfigChange(cfg, false, "knowledge_bundles", []string{"bundles/okf"}); err == nil {
		t.Error("expected config set knowledge_bundles to refuse (retired key)")
	}
	if _, err := provision.ApplyConfigChange(cfg, true, "knowledge_bundles", []string{"bundles/okf"}); err == nil {
		t.Error("expected config unset knowledge_bundles to refuse (retired key)")
	}
}

// TestApplyConfigChange_Models: set overrides, unset resets to the default.
func TestApplyConfigChange_Models(t *testing.T) {
	cfg := defaultCfg()
	if _, err := provision.ApplyConfigChange(cfg, false, "memory_watcher_model", []string{"llama3"}); err != nil {
		t.Fatal(err)
	}
	if cfg.MemoryWatcherModel != "llama3" {
		t.Errorf("watcher = %q, want llama3", cfg.MemoryWatcherModel)
	}
	if _, err := provision.ApplyConfigChange(cfg, true, "memory_watcher_model", nil); err != nil {
		t.Fatal(err)
	}
	if cfg.MemoryWatcherModel != config.DefaultMemoryWatcherModel {
		t.Errorf("watcher after unset = %q, want default", cfg.MemoryWatcherModel)
	}
	if _, err := provision.ApplyConfigChange(cfg, false, "memory_embed_model", []string{"embed-x"}); err != nil {
		t.Fatal(err)
	}
	if cfg.MemoryEmbedModel != "embed-x" {
		t.Errorf("embed = %q, want embed-x", cfg.MemoryEmbedModel)
	}
}

// TestApplyConfigChange_MemoryCapture: set validates the enum, unset resets
// to the explicit default, and a bad value is refused as a usage error rather
// than silently accepted.
func TestApplyConfigChange_MemoryCapture(t *testing.T) {
	cfg := defaultCfg()
	if _, err := provision.ApplyConfigChange(cfg, false, "memory_capture", []string{"experimental-auto"}); err != nil {
		t.Fatal(err)
	}
	if cfg.MemoryCapture != "experimental-auto" {
		t.Errorf("memory_capture = %q, want experimental-auto", cfg.MemoryCapture)
	}
	if _, err := provision.ApplyConfigChange(cfg, true, "memory_capture", nil); err != nil {
		t.Fatal(err)
	}
	if cfg.MemoryCapture != config.DefaultMemoryCapture {
		t.Errorf("memory_capture after unset = %q, want default", cfg.MemoryCapture)
	}
	if _, err := provision.ApplyConfigChange(cfg, false, "memory_capture", []string{"review"}); err == nil {
		t.Error("expected an invalid memory_capture value to be refused")
	}
}

// TestApplyConfigChange_UnknownKey errors and lists the supported keys.
func TestApplyConfigChange_UnknownKey(t *testing.T) {
	_, err := provision.ApplyConfigChange(defaultCfg(), false, "nope", []string{"x"})
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
	// One scalar key and one list key, so both write paths are covered.
	if _, err := provision.ApplyConfigChange(cfg, false, "ollama_bridge_model", []string{"llama3"}); err != nil {
		t.Fatal(err)
	}
	if _, err := provision.ApplyConfigChange(cfg, false, "mcp", []string{testMCPServer}); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.OllamaBridgeModel != "llama3" || !slices.Contains(got.MCP, testMCPServer) {
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
