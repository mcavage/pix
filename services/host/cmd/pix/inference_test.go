package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"pix/host/config"
)

func TestCompileInferenceRuntimeNoModelAndExclusiveFiltering(t *testing.T) {
	cfg := &config.Config{Inference: config.InferenceConfig{
		Backends: map[string]config.InferenceBackend{
			"direct":  {Driver: "native", Auth: "1password"},
			"gateway": {Driver: "openai-compatible", Auth: "sbx-session", BaseURL: "https://models.example.test/v1"},
		},
		ExclusiveBackend: "gateway",
		Models: []config.InferenceModelBinding{
			{Model: "anthropic/claude-sonnet-5", Backend: "direct", Upstream: "anthropic/claude-sonnet-5", Available: true},
			{Model: "openai/gpt-5.6-sol", Backend: "gateway", Upstream: "reasoner", Available: true},
		},
	}}
	routes, manifest, err := compileInferenceRuntime(cfg, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Models) != 1 || manifest.Models[0].ID != "gateway/reasoner" {
		t.Fatalf("models = %+v", manifest.Models)
	}
	if _, ok := manifest.Backends["direct"]; ok {
		t.Fatal("exclusive runtime leaked a disallowed backend")
	}
	if len(routes.Routes) == 0 {
		t.Fatal("one callable model should serve scored intents")
	}
	for name, route := range routes.Routes {
		if route.Model != "gateway/reasoner" {
			t.Fatalf("route %s = %s", name, route.Model)
		}
	}
}

func TestConfigureDirectInferenceUsesCatalog(t *testing.T) {
	cfg := &config.Config{}
	if err := configureDirectInference(cfg, []string{"anthropic"}); err != nil {
		t.Fatal(err)
	}
	if cfg.Inference.Backends["anthropic"].KeyEnv != "ANTHROPIC_API_KEY" {
		t.Fatalf("backend = %+v", cfg.Inference.Backends["anthropic"])
	}
	if len(cfg.Inference.Models) == 0 {
		t.Fatal("catalog models were not bound")
	}
	for _, b := range cfg.Inference.Models {
		if b.Backend != "anthropic" || !b.Available {
			t.Fatalf("binding = %+v", b)
		}
	}
}

func TestSetupChooseInferenceOffersDetectedOllamaAndNeedsNoOnePassword(t *testing.T) {
	env := shellEnv{
		lookPath: func(name string) (string, error) {
			if name == "ollama" {
				return "/usr/local/bin/ollama", nil
			}
			return "", fmt.Errorf("missing")
		},
		run: func(name string, args ...string) (string, error) {
			if name == "ollama" && len(args) == 1 && args[0] == "list" {
				return "NAME ID SIZE MODIFIED\nqwen3.5:9b abc 6GB now\n", nil
			}
			return "", fmt.Errorf("unexpected command")
		},
	}
	cfg := &config.Config{}
	var out bytes.Buffer
	selected, err := setupChooseInference(cfg, env, strings.NewReader("2\n"), &out, true)
	if err != nil {
		t.Fatal(err)
	}
	if !selected || inferenceNeedsOnePassword(cfg) {
		t.Fatalf("selected=%v inference=%+v", selected, cfg.Inference)
	}
	if !strings.Contains(out.String(), "2. Ollama") || len(cfg.Inference.Models) != 1 {
		t.Fatalf("output=%q models=%+v", out.String(), cfg.Inference.Models)
	}
}

func TestSetupChooseInferenceConfiguresKeylessGateway(t *testing.T) {
	cfg := &config.Config{}
	var out bytes.Buffer
	input := "3\nhttps://models.example.test/v1\n\nanthropic/claude-sonnet-5=sonnet-prod\n"
	selected, err := setupChooseInference(cfg, shellEnv{}, strings.NewReader(input), &out, true)
	if err != nil {
		t.Fatal(err)
	}
	if !selected || inferenceNeedsOnePassword(cfg) || len(cfg.Inference.Models) != 1 {
		t.Fatalf("selected=%v inference=%+v", selected, cfg.Inference)
	}
	if got := cfg.Inference.Backends["gateway"]; got.Auth != "sbx-session" || got.BaseURL != "https://models.example.test/v1" || got.CredentialService != "sbx-login" || got.KeyEnv != "DOCKER_TOKEN" || got.CredentialHeader != "Authorization" || got.CredentialFormat != "Bearer %s" {
		t.Fatalf("gateway = %+v", got)
	}
}

func TestExclusiveKeylessInferenceIgnoresDormantOnePasswordBackend(t *testing.T) {
	cfg := &config.Config{Inference: config.InferenceConfig{
		Backends: map[string]config.InferenceBackend{
			"direct":  {Driver: "native", Auth: "1password"},
			"gateway": {Driver: "openai-compatible", Auth: "sbx-session", BaseURL: "https://models.example.test/v1", Source: "/packs/work"},
		},
		Models: []config.InferenceModelBinding{
			{Model: "anthropic/claude-sonnet-5", Backend: "direct", Upstream: "anthropic/claude-sonnet-5", Available: true},
			{Model: "openai/gpt-5.6-sol", Backend: "gateway", Upstream: "reasoner", Available: true, Source: "/packs/work"},
		},
		ExclusiveSource: "/packs/work",
	}}
	if inferenceNeedsOnePassword(cfg) {
		t.Fatal("a dormant direct backend outside the exclusive runtime must not force 1Password")
	}
}

func TestBuildSbxArgsConstrainsModelCycleToCallableBindings(t *testing.T) {
	args := buildSbxArgs(&config.Config{}, runOpts{
		Workspace: ".",
		Model:     "gateway/reasoner",
		Models:    []string{"gateway/fast", "gateway/reasoner"},
	}, "0.1.0")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--models gateway/fast,gateway/reasoner") {
		t.Fatalf("args do not constrain model cycle: %v", args)
	}
}

func TestInferenceAllowsOnlyMaterializedRuntimeID(t *testing.T) {
	cfg := &config.Config{Inference: config.InferenceConfig{
		Backends: map[string]config.InferenceBackend{"gateway": {Driver: "openai-compatible", Auth: "none", BaseURL: "http://127.0.0.1:9000/v1"}},
		Models:   []config.InferenceModelBinding{{Model: "openai/gpt-5.6-sol", Backend: "gateway", Upstream: "reasoner", Available: true}},
	}}
	if !inferenceAllowsModel(cfg, "gateway/reasoner") {
		t.Fatal("materialized id should be allowed")
	}
	if inferenceAllowsModel(cfg, "openai/gpt-5.6-sol") {
		t.Fatal("canonical catalog id must not bypass its configured backend")
	}
}

func TestSetupPackFlagIsRepeatableAndOrdered(t *testing.T) {
	o, err := parseOnboardArgs([]string{"--pack", "one", "--pack=two", "--with", "optional"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(o.packs, ","); got != "one,two" {
		t.Fatalf("packs = %q", got)
	}
}

func TestInferenceKitSpecGeneratesSessionCredentialAndEgress(t *testing.T) {
	cfg := &config.Config{Inference: config.InferenceConfig{
		Backends: map[string]config.InferenceBackend{
			"work-openai": {
				Driver: "openai-compatible", Protocol: "openai-responses",
				BaseURL: "https://gateway.example.test/inference/openai/v1", Auth: "sbx-session", KeyEnv: "SESSION_TOKEN",
				CredentialService: "session-login", CredentialHeader: "Authorization", CredentialFormat: "Bearer %s", Source: "/packs/work",
			},
		},
		Models:          []config.InferenceModelBinding{{Model: "openai/gpt-5.6-sol", Backend: "work-openai", Upstream: "reasoner", Available: true, Source: "/packs/work"}},
		ExclusiveSource: "/packs/work",
	}}
	spec, err := inferenceKitSpec(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"gateway.example.test:443", `service: "session-login"`, `name: "SESSION_TOKEN"`, `domain: "gateway.example.test"`, `format: "Bearer %s"`} {
		if !strings.Contains(spec, want) {
			t.Fatalf("spec missing %q:\n%s", want, spec)
		}
	}
}

func TestInferenceKitSpecPreservesAPIKeyHeaderWithoutBearerPrefix(t *testing.T) {
	cfg := &config.Config{Inference: config.InferenceConfig{
		Backends: map[string]config.InferenceBackend{
			"work": {
				Driver: "openai-compatible", Protocol: "openai-responses",
				BaseURL: "https://gateway.example.test/v1", Auth: "sbx-session", KeyEnv: "SESSION_TOKEN",
				CredentialService: "session-login", CredentialHeader: "x-api-key", CredentialFormat: "%s",
			},
		},
		Models: []config.InferenceModelBinding{{Model: "openai/gpt-5.6-sol", Backend: "work", Upstream: "reasoner", Available: true}},
	}}
	spec, err := inferenceKitSpec(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`header: "x-api-key"`, `format: "%s"`} {
		if !strings.Contains(spec, want) {
			t.Fatalf("spec missing %q:\n%s", want, spec)
		}
	}
	if strings.Contains(spec, "Bearer") {
		t.Fatalf("API-key credential unexpectedly gained a bearer prefix:\n%s", spec)
	}
}

func TestInferenceKitSpecUsesAmbientDockerSessionContract(t *testing.T) {
	cfg := &config.Config{Inference: config.InferenceConfig{
		Backends: map[string]config.InferenceBackend{
			"gateway": {
				Driver: "openai-compatible", Protocol: "openai-responses",
				BaseURL: "https://models.example.test/v1", Auth: "sbx-session", KeyEnv: "DOCKER_TOKEN",
				CredentialService: "sbx-login", CredentialHeader: "Authorization", CredentialFormat: "Bearer %s",
			},
		},
		Models: []config.InferenceModelBinding{{Model: "openai/gpt-5.6-sol", Backend: "gateway", Upstream: "reasoner", Available: true}},
	}}
	spec, err := inferenceKitSpec(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`service: "sbx-login"`, `name: "DOCKER_TOKEN"`, `proxyManaged: true`, `header: "Authorization"`, `format: "Bearer %s"`} {
		if !strings.Contains(spec, want) {
			t.Fatalf("spec missing %q:\n%s", want, spec)
		}
	}
}
