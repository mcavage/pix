package main

import (
	"os"
	"path/filepath"
	"testing"

	"pix/host/envinfo"
)

func TestResolveRunModel_ConfiguredAnthropicUsesCurrentCatalogDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "secrets.env"), []byte("ANTHROPIC_API_KEY=op://Vault/Anthropic/key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	model, source, err := resolveRunModel("", nil, defaultShellEnv())
	if err != nil {
		t.Fatal(err)
	}
	if model != "anthropic/claude-opus-5" || source != "configured provider default" {
		t.Fatalf("resolveRunModel = (%q, %q), want current Anthropic default", model, source)
	}
}

func TestResolveRunModel_AllCloudProvidersUsesShippedSessionDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	refs := "ANTHROPIC_API_KEY=op://Vault/Anthropic/key\nOPENAI_API_KEY=op://Vault/OpenAI/key\nGEMINI_API_KEY=op://Vault/Google/key\n"
	if err := os.WriteFile(filepath.Join(home, "secrets.env"), []byte(refs), 0o600); err != nil {
		t.Fatal(err)
	}
	model, _, err := resolveRunModel("", nil, defaultShellEnv())
	if err != nil {
		t.Fatal(err)
	}
	if model != "openai/gpt-5.6-sol" {
		t.Fatalf("resolveRunModel with all cloud providers = %q, want shipped session default", model)
	}
}

func TestResolveRunModel_NoChoiceAndNoProviderRefusesStalePiFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "secrets.env"), []byte("# no provider refs\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveRunModel("", nil, defaultShellEnv()); err == nil {
		t.Fatal("expected no selected model and no configured provider to refuse")
	}
}

func TestResolveRunModel_ExplicitAndEnvironmentChoicesStillWin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	sc := &envinfo.Sidecar{}
	sc.Models.Main = "google/gemini-3.1-pro-preview"

	model, source, err := resolveRunModel("openai/gpt-5.6-sol", sc, defaultShellEnv())
	if err != nil || model != "openai/gpt-5.6-sol" || source != "--model" {
		t.Fatalf("explicit resolveRunModel = (%q, %q, %v)", model, source, err)
	}
	model, source, err = resolveRunModel("", sc, defaultShellEnv())
	if err != nil || model != "google/gemini-3.1-pro-preview" || source != "[models].main" {
		t.Fatalf("environment resolveRunModel = (%q, %q, %v)", model, source, err)
	}
}
