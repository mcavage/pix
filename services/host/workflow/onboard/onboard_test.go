package onboard

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/hostenv/hostenvtest"
)

// captureSave returns a save func that records the last saved config.
func captureSave(dst **config.Config) func(*config.Config) error {
	return func(c *config.Config) error { *dst = c; return nil }
}

// noHostResolver makes mcp.LocalMCPNames report the local set as UNKNOWN, so
// validation fails closed on any non-gog/non-catalog mcp name.
func noHostResolver() (string, error) { return "", fmt.Errorf("no host binary in test") }

func TestValidateOnboarding_Allowlist(t *testing.T) {
	cfg := defaultCfg()
	env := hostenvtest.Env{Present: map[string]bool{}}.Build()

	ok := []*OnboardingResult{
		{Version: 1, MCP: []string{config.GWServerName}},
		{Version: 1, MCP: []string{"notion", "atlassian", "granola"}},
	}
	for i, r := range ok {
		if err := ValidateOnboardingResult(r, cfg, env, noHostResolver); err != nil {
			t.Errorf("ok[%d] rejected: %v", i, err)
		}
	}

	bad := map[string]*OnboardingResult{
		"bad version": {Version: 2},
		"unknown mcp": {Version: 1, MCP: []string{"evil-server"}},
		// "linear" was the drift the derived allowlist removes: it looks like a
		// plausible catalog name but `pix mcp bundle` cannot register it.
		"unshipped catalog-looking mcp": {Version: 1, MCP: []string{"linear"}},
		"model whitespace":              {Version: 1, OllamaBridgeModel: "bad model"},
	}
	for name, r := range bad {
		if err := ValidateOnboardingResult(r, cfg, env, noHostResolver); err == nil {
			t.Errorf("%s: expected rejection, got nil", name)
		}
	}
}

// TestApplyOnboarding_Idempotent is the fitness function: re-applying identical
// input yields no further changes.
func TestApplyOnboarding_Idempotent(t *testing.T) {
	cfg := defaultCfg()
	env := hostenvtest.Env{Present: map[string]bool{}}.Build()
	r := &OnboardingResult{Version: 1, MCP: []string{config.GWServerName}, OllamaBridgeModel: "qwen3.5:9b"}

	var saved *config.Config
	first, err := ApplyOnboardingResult(r, cfg, env, &bytes.Buffer{}, captureSave(&saved))
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if len(first) == 0 {
		t.Fatal("first apply made no changes")
	}
	second, err := ApplyOnboardingResult(r, cfg, env, &bytes.Buffer{}, captureSave(&saved))
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("second apply not idempotent, changed: %v", second)
	}
}

func TestApplyOnboarding_AppliesFields(t *testing.T) {
	cfg := defaultCfg()
	env := hostenvtest.Env{Present: map[string]bool{}}.Build()
	r := &OnboardingResult{
		Version: 1, MCP: []string{config.GWServerName, "notion"},
		OllamaBridgeModel: "qwen3.5:9b", MemoryWatcherModel: "qwen3.5:9b",
	}
	var saved *config.Config
	if _, err := ApplyOnboardingResult(r, cfg, env, &bytes.Buffer{}, captureSave(&saved)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// There is deliberately NO account writer in onboarding — Google Workspace
	// authorization needs a browser, so applying an OnboardingResult must never
	// set google_workspace_account (that write is manual: `pix config set
	// google_workspace_account <email>`, then `pix mcp register`).
	if cfg.GogAccount != "" {
		t.Errorf("onboarding must never set google_workspace_account, got %q", cfg.GogAccount)
	}
	if !slices.Contains(cfg.MCP, config.GWServerName) || !slices.Contains(cfg.MCP, "notion") {
		t.Errorf("mcp = %v", cfg.MCP)
	}
	if !slices.Contains(cfg.Services, "memory") {
		t.Errorf("memory service should be ensured: %v", cfg.Services)
	}
	if cfg.OllamaBridgeModel != "qwen3.5:9b" || cfg.MemoryWatcherModel != "qwen3.5:9b" {
		t.Errorf("models not applied: bridge=%q watcher=%q", cfg.OllamaBridgeModel, cfg.MemoryWatcherModel)
	}
	if saved == nil {
		t.Error("apply must Save the config")
	}
}

func TestParseOnboardArgs(t *testing.T) {
	o, err := ParseOnboardArgs([]string{"--account", "a@b.com", "--mcp", config.GWServerName, "--mcp=notion", "--model", "m", "--yes"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if o.Account != "a@b.com" || o.Model != "m" || !o.AssumeYes {
		t.Errorf("parsed = %+v", o)
	}
	if !slices.Contains(o.Mcp, config.GWServerName) || !slices.Contains(o.Mcp, "notion") {
		t.Errorf("mcp = %v", o.Mcp)
	}
	if _, err := ParseOnboardArgs([]string{"--account"}); err == nil {
		t.Error("--account without value should error")
	}
}

// TestOnboardingFileNameConst guards the reconcile file name (a rename must be
// deliberate).
func TestOnboardingFileNameConst(t *testing.T) {
	if FileName != "onboarding.json" || !strings.HasSuffix(FileName, ".json") {
		t.Errorf("onboarding file name changed: %q", FileName)
	}
}

// TestReconcileOnboarding_AppliesFromFile writes a valid proposal, reconciles it
// with assumeYes (CI path), and asserts the config was applied and the file
// removed.
func TestReconcileOnboarding_AppliesFromFile(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	t.Setenv("PIX_PROFILE", "")
	dir := filepath.Join(ws, ".pix")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fp := filepath.Join(dir, "onboarding.json")
	if err := os.WriteFile(fp, []byte(`{"version":1,"google_workspace_account":"me@x.com","mcp":["`+config.GWServerName+`"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	env := hostenvtest.Env{Present: map[string]bool{}}.Build()

	var out bytes.Buffer
	ReconcileOnboarding(ws, env, strings.NewReader(""), &out, true, false, testDeps())

	if _, err := os.Stat(fp); !os.IsNotExist(err) {
		t.Errorf("onboarding.json should be removed after apply, err=%v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	// google_workspace_account in the file is a stray, unrecognized field on
	// OnboardingResult (deliberately absent) — it must be silently ignored,
	// never applied. Only mcp is a real field here.
	if cfg.GogAccount != "" {
		t.Errorf("onboarding must never apply google_workspace_account from the file, got %q", cfg.GogAccount)
	}
	if !slices.Contains(cfg.MCP, config.GWServerName) {
		t.Errorf("config not applied: mcp=%v", cfg.MCP)
	}
}

// TestReconcileOnboarding_NonTTYLeavesFile: without assumeYes and no TTY, the
// proposal is left in place (never silently applied).
func TestReconcileOnboarding_NonTTYLeavesFile(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	t.Setenv("PIX_PROFILE", "")
	dir := filepath.Join(ws, ".pix")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fp := filepath.Join(dir, "onboarding.json")
	// google_workspace_account alone produces no applicable change (onboarding
	// has deliberately no account writer); give the proposal a real change
	// (mcp) so the assumeYes/tty gate below actually gets exercised.
	if err := os.WriteFile(fp, []byte(`{"version":1,"google_workspace_account":"me@x.com","mcp":["`+config.GWServerName+`"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	env := hostenvtest.Env{Present: map[string]bool{}}.Build()
	var out bytes.Buffer
	ReconcileOnboarding(ws, env, strings.NewReader(""), &out, false, false, testDeps())
	if _, err := os.Stat(fp); err != nil {
		t.Errorf("non-tty reconcile must leave the file for review, err=%v", err)
	}
}
