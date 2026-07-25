package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pi-stack/host/config"
)

// captureSave returns a save func that records the last saved config.
func captureSave(dst **config.Config) func(*config.Config) error {
	return func(c *config.Config) error { *dst = c; return nil }
}

// noHostResolver makes localMCPNames report the local set as UNKNOWN, so
// validation fails closed on any non-gog/non-catalog mcp name.
func noHostResolver() (string, error) { return "", fmt.Errorf("no host binary in test") }

func TestValidateOnboarding_Allowlist(t *testing.T) {
	cfg := defaultCfg()
	env := fakeEnv{present: map[string]bool{}}.env()

	ok := []*onboardingResult{
		{Version: 1, GogAccount: "me@x.com", MCP: []string{"gog"}},
		{Version: 1, MCP: []string{"notion", "atlassian", "granola"}},
		{Version: 1, Knowledge: &onboardKnowledge{Action: "skip"}},
	}
	for i, r := range ok {
		if err := validateOnboardingResult(r, cfg, env, noHostResolver); err != nil {
			t.Errorf("ok[%d] rejected: %v", i, err)
		}
	}

	bad := map[string]*onboardingResult{
		"bad version": {Version: 2},
		"unknown mcp": {Version: 1, MCP: []string{"evil-server"}},
		// "linear" was the drift the derived allowlist removes: it looks like a
		// plausible catalog name but `pi-stack mcp bundle` cannot register it.
		"unshipped catalog-looking mcp": {Version: 1, MCP: []string{"linear"}},
		"bad kb action":                 {Version: 1, Knowledge: &onboardKnowledge{Action: "nuke", Source: "/x"}},
		"kb missing source":             {Version: 1, Knowledge: &onboardKnowledge{Action: "use"}},
		"model whitespace":              {Version: 1, OllamaBridgeModel: "bad model"},
	}
	for name, r := range bad {
		if err := validateOnboardingResult(r, cfg, env, noHostResolver); err == nil {
			t.Errorf("%s: expected rejection, got nil", name)
		}
	}
}

// TestApplyOnboarding_Idempotent is the fitness function: re-applying identical
// input yields no further changes.
func TestApplyOnboarding_Idempotent(t *testing.T) {
	cfg := defaultCfg()
	env := fakeEnv{present: map[string]bool{}}.env()
	r := &onboardingResult{Version: 1, GogAccount: "me@x.com", MCP: []string{"gog"}, OllamaBridgeModel: "qwen3.5:9b"}

	var saved *config.Config
	first, err := applyOnboardingResult(r, cfg, env, &bytes.Buffer{}, captureSave(&saved))
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if len(first) == 0 {
		t.Fatal("first apply made no changes")
	}
	second, err := applyOnboardingResult(r, cfg, env, &bytes.Buffer{}, captureSave(&saved))
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("second apply not idempotent, changed: %v", second)
	}
}

func TestApplyOnboarding_AppliesFields(t *testing.T) {
	cfg := defaultCfg()
	env := fakeEnv{present: map[string]bool{}}.env()
	r := &onboardingResult{
		Version: 1, GogAccount: "me@x.com", MCP: []string{"gog", "notion"},
		OllamaBridgeModel: "qwen3.5:9b", MemoryWatcherModel: "qwen3.5:9b",
	}
	var saved *config.Config
	if _, err := applyOnboardingResult(r, cfg, env, &bytes.Buffer{}, captureSave(&saved)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if cfg.GogAccount != "me@x.com" {
		t.Errorf("gog_account = %q", cfg.GogAccount)
	}
	if !containsStr(cfg.MCP, "gog") || !containsStr(cfg.MCP, "notion") {
		t.Errorf("mcp = %v", cfg.MCP)
	}
	if !containsStr(cfg.Services, "memory") {
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
	o, err := parseOnboardArgs([]string{"--account", "a@b.com", "--mcp", "gog", "--mcp=notion", "--model", "m", "--yes"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if o.account != "a@b.com" || o.model != "m" || !o.assumeYes {
		t.Errorf("parsed = %+v", o)
	}
	if !containsStr(o.mcp, "gog") || !containsStr(o.mcp, "notion") {
		t.Errorf("mcp = %v", o.mcp)
	}
	if _, err := parseOnboardArgs([]string{"--account"}); err == nil {
		t.Error("--account without value should error")
	}
}

// TestOnboardingFileNameConst guards the reconcile file name (a rename must be
// deliberate).
func TestOnboardingFileNameConst(t *testing.T) {
	if onboardingFileName != "onboarding.json" || !strings.HasSuffix(onboardingFileName, ".json") {
		t.Errorf("onboarding file name changed: %q", onboardingFileName)
	}
}

// TestFlagTakesValue guards the onboard-flag arity setup uses to split DIR from
// value-bearing flags.
func TestFlagTakesValue(t *testing.T) {
	for _, f := range []string{"--account", "--knowledge", "--mcp", "--model"} {
		if !flagTakesValue(f) {
			t.Errorf("%s should take a value", f)
		}
	}
	for _, f := range []string{"--help", "-h", "--yes", "--account=x"} {
		if flagTakesValue(f) {
			t.Errorf("%s should NOT consume a following token", f)
		}
	}
}

// TestReconcileOnboarding_AppliesFromFile writes a valid proposal, reconciles it
// with assumeYes (CI path), and asserts the config was applied and the file
// removed.
func TestReconcileOnboarding_AppliesFromFile(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	t.Setenv("PI_STACK_PROFILE", "")
	dir := filepath.Join(ws, ".pi-stack")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fp := filepath.Join(dir, "onboarding.json")
	if err := os.WriteFile(fp, []byte(`{"version":1,"gog_account":"me@x.com","mcp":["gog"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	env := fakeEnv{present: map[string]bool{}}.env()

	var out bytes.Buffer
	reconcileOnboarding(ws, env, strings.NewReader(""), &out, true, false)

	if _, err := os.Stat(fp); !os.IsNotExist(err) {
		t.Errorf("onboarding.json should be removed after apply, err=%v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GogAccount != "me@x.com" || !containsStr(cfg.MCP, "gog") {
		t.Errorf("config not applied: gog=%q mcp=%v", cfg.GogAccount, cfg.MCP)
	}
}

// TestReconcileOnboarding_NonTTYLeavesFile: without assumeYes and no TTY, the
// proposal is left in place (never silently applied).
func TestReconcileOnboarding_NonTTYLeavesFile(t *testing.T) {
	ws := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	t.Setenv("PI_STACK_PROFILE", "")
	dir := filepath.Join(ws, ".pi-stack")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fp := filepath.Join(dir, "onboarding.json")
	if err := os.WriteFile(fp, []byte(`{"version":1,"gog_account":"me@x.com"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	env := fakeEnv{present: map[string]bool{}}.env()
	var out bytes.Buffer
	reconcileOnboarding(ws, env, strings.NewReader(""), &out, false, false)
	if _, err := os.Stat(fp); err != nil {
		t.Errorf("non-tty reconcile must leave the file for review, err=%v", err)
	}
}
