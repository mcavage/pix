package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
)

func TestPackInferenceValidationIsGenericAndFailClosed(t *testing.T) {
	root := t.TempDir()
	m := packManifest{Name: "team", Schema: 1, Inference: &packInference{
		Exclusive: true, RequiredBackend: "gateway",
		Backends: map[string]packInferenceBack{"gateway": {Driver: "openai-compatible", Auth: "sbx-session", BaseURL: "https://models.example.test/v1", CredentialService: "session", KeyEnv: "SESSION_TOKEN"}},
		Models:   []packInferenceModel{{Model: "openai/gpt-5.6-sol", Backend: "gateway", Upstream: "alpha-prod"}},
	}}
	if err := writePackManifest(root, m); err != nil {
		t.Fatal(err)
	}
	p, err := loadPack(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	applyPackInference(cfg, p.Manifest.Inference, root)
	if cfg.Inference.ExclusiveSource != root || cfg.Inference.Backends["gateway"].Auth != "sbx-session" {
		t.Fatalf("inference projection = %+v", cfg.Inference)
	}
	if len(cfg.Inference.Models) != 1 || cfg.Inference.Models[0].Available {
		t.Fatalf("pack binding must begin unverified: %+v", cfg.Inference.Models)
	}
}

func TestPackInferenceRejectsModelOutsideCatalog(t *testing.T) {
	root := t.TempDir()
	m := packManifest{Name: "team", Schema: 1, Inference: &packInference{
		Backends: map[string]packInferenceBack{"gateway": {Driver: "openai-compatible", Auth: "none", BaseURL: "http://127.0.0.1:9000/v1"}},
		Models:   []packInferenceModel{{Model: "private/unknown", Backend: "gateway", Upstream: "unknown"}},
	}}
	if err := writePackManifest(root, m); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPack(root); err == nil || !strings.Contains(err.Error(), "not in the Pix model catalog") {
		t.Fatalf("error = %v", err)
	}
}

func TestPackInferenceRejectsUnknownBackend(t *testing.T) {
	root := t.TempDir()
	text := `name = "team"
schema = 1

[inference]
required_backend = "missing"
`
	if err := os.WriteFile(filepath.Join(root, packManifestName), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadPack(root)
	if err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("error = %v", err)
	}
}

func TestPersistPackStackComposesInferenceInOrder(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	first, second := t.TempDir(), t.TempDir()
	write := func(root, name, backend, model string, exclusive bool) {
		t.Helper()
		m := packManifest{Name: name, Schema: 1, Inference: &packInference{
			Exclusive: exclusive, RequiredBackend: backend,
			Backends: map[string]packInferenceBack{backend: {Driver: "openai-compatible", Auth: "none", BaseURL: "http://127.0.0.1:9000/v1"}},
			Models:   []packInferenceModel{{Model: model, Backend: backend, Upstream: name + "-model"}},
		}}
		if err := writePackManifest(root, m); err != nil {
			t.Fatal(err)
		}
	}
	write(first, "one", "first", "anthropic/claude-sonnet-5", false)
	write(second, "two", "second", "openai/gpt-5.6-sol", true)
	if err := persistPackStack([]string{first, second}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Packs) != 2 || cfg.Packs[0] != first || cfg.Packs[1] != second {
		t.Fatalf("packs = %v", cfg.Packs)
	}
	if cfg.Inference.ExclusiveSource != second || len(cfg.Inference.Backends) != 2 || len(cfg.Inference.Models) != 2 {
		t.Fatalf("inference = %+v", cfg.Inference)
	}
}

func TestClearPackInferencePreservesUserBackends(t *testing.T) {
	cfg := &config.Config{Inference: config.InferenceConfig{
		Backends: map[string]config.InferenceBackend{
			"personal": {Driver: "native", Auth: "1password"},
			"work":     {Driver: "openai-compatible", Auth: "none", Source: "/packs/work"},
		},
		Models: []config.InferenceModelBinding{
			{Model: "openai/gpt-5.6-sol", Backend: "personal", Available: true},
			{Model: "anthropic/claude-sonnet-5", Backend: "work", Available: true, Source: "/packs/work"},
		},
		ExclusiveSource: "/packs/work",
	}}
	clearPackInference(cfg, "")
	if len(cfg.Inference.Backends) != 1 || cfg.Inference.Backends["personal"].Driver != "native" || len(cfg.Inference.Models) != 1 {
		t.Fatalf("user inference was not preserved: %+v", cfg.Inference)
	}
	if cfg.Inference.ExclusiveSource != "" {
		t.Fatalf("stale exclusivity survived: %q", cfg.Inference.ExclusiveSource)
	}
}
