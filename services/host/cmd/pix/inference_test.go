package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"pix/host/config"
	"pix/host/inference"
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
	if manifest.Models[0].ContextWindow != 1050000 || manifest.Models[0].MaxTokens != 128000 {
		t.Fatalf("GPT-5.6 Sol limits = %d/%d, want 1050000/128000", manifest.Models[0].ContextWindow, manifest.Models[0].MaxTokens)
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

func TestCompileInferenceRuntimeCarriesAdaptiveThinkingFromCatalog(t *testing.T) {
	cfg := &config.Config{Inference: config.InferenceConfig{
		Backends: map[string]config.InferenceBackend{
			"gateway": {Driver: "openai-compatible", Protocol: "anthropic-messages", Auth: "sbx-session", BaseURL: "https://models.example.test"},
		},
		Models: []config.InferenceModelBinding{
			{Model: "anthropic/claude-opus-5", Backend: "gateway", Upstream: "claude-opus-5", Available: true},
		},
	}}
	_, manifest, err := compileInferenceRuntime(cfg, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Models) != 1 || !manifest.Models[0].AdaptiveThinking {
		t.Fatalf("adaptive thinking metadata missing: %+v", manifest.Models)
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

func TestConfigureModelRosterRestrictsRuntimeAndRoutes(t *testing.T) {
	cfg := &config.Config{}
	if err := configureDirectInference(cfg, []string{"anthropic", "openai"}); err != nil {
		t.Fatal(err)
	}
	// The roster runs AFTER verification now, and only offers what answered a
	// request — so a probed roster is the only shape it ever sees in setup.
	for i := range cfg.Inference.Models {
		cfg.Inference.Models[i].Verified = true
		cfg.Inference.Models[i].VerifiedBy = config.VerifiedByProbe
	}
	if err := configureModelRoster(cfg, strings.NewReader(""), &bytes.Buffer{}, false, "openai/gpt-5.6-sol"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.Inference.AllowedModels, ","); got != "openai/gpt-5.6-sol" {
		t.Fatalf("allowed models = %q", got)
	}
	routes, manifest, err := compileInferenceRuntime(cfg, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Models) != 1 || manifest.Models[0].CatalogModel != "openai/gpt-5.6-sol" {
		t.Fatalf("runtime roster leaked another model: %+v", manifest.Models)
	}
	for intent, route := range routes.Routes {
		if route.Model != "openai/gpt-5.6-sol" {
			t.Fatalf("route %s escaped roster: %s", intent, route.Model)
		}
	}
}

func TestConfigureModelRosterPreservesPersonalChoiceUnderExclusivePack(t *testing.T) {
	cfg := &config.Config{Inference: config.InferenceConfig{
		AllowedModels:   []string{"ollama/kimi-k3:cloud"},
		ExclusiveSource: "/packs/work",
	}}
	if err := configureModelRoster(cfg, strings.NewReader(""), &bytes.Buffer{}, true, ""); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.Inference.AllowedModels, ","); got != "ollama/kimi-k3:cloud" {
		t.Fatalf("exclusive pack erased personal roster: %q", got)
	}
}

func TestConfigureModelRosterRejectsUnavailableChoice(t *testing.T) {
	cfg := &config.Config{}
	if err := configureDirectInference(cfg, []string{"openai"}); err != nil {
		t.Fatal(err)
	}
	for i := range cfg.Inference.Models {
		cfg.Inference.Models[i].Verified = true
		cfg.Inference.Models[i].VerifiedBy = config.VerifiedByProbe
	}
	err := configureModelRoster(cfg, strings.NewReader(""), &bytes.Buffer{}, false, "ollama/kimi-k3:cloud")
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("error = %v", err)
	}
}

func TestDirectInferenceProbeDoesNotVerifyRejectedOrUnavailableKey(t *testing.T) {
	const key = "sk-valid-looking-but-unauthorized"
	for _, probeFailure := range []string{"provider rejected model request (HTTP 401)", "probe unavailable"} {
		t.Run(probeFailure, func(t *testing.T) {
			cfg := &config.Config{}
			if err := configureDirectInference(cfg, []string{"openai"}); err != nil {
				t.Fatal(err)
			}
			env := shellEnv{
				readFile: func(string) (string, error) { return "OPENAI_API_KEY=op://vault/openai/key\n", nil },
				run: func(name string, args ...string) (string, error) {
					if name == "op" && len(args) == 2 && args[0] == "read" {
						return key + "\n", nil
					}
					return "", fmt.Errorf("unexpected command")
				},
				directInferenceProbe: func(provider, model, gotKey string) error {
					if provider != "openai" || model == "" || gotKey != key {
						return fmt.Errorf("bad probe args provider=%q model=%q key-match=%v", provider, model, gotKey == key)
					}
					return fmt.Errorf("%s", probeFailure)
				},
			}
			probe, probeErr := verifyDirectInference(cfg, env)
			mustNoProbeErr(t, probeErr)
			attempted, verified, failures := probe.Attempted, probe.Verified, probe.Failures
			if attempted != len(cfg.Inference.Models) || verified != 0 || len(failures) != attempted {
				t.Fatalf("attempted=%d verified=%d failures=%v", attempted, verified, failures)
			}
			if strings.Contains(strings.Join(failures, " "), key) {
				t.Fatal("probe failure leaked the resolved key")
			}
			for _, binding := range cfg.Inference.Models {
				if binding.Verified || inference.Callable(cfg, binding) {
					t.Fatalf("unverified binding became callable: %+v", binding)
				}
			}
			models, err := callableRuntimeModels(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if len(models) != 0 {
				t.Fatalf("unverified models advertised as callable: %v", models)
			}
		})
	}
}

func TestDirectInferenceProbeVerifiesOnlySuccessfulModel(t *testing.T) {
	cfg := &config.Config{}
	if err := configureDirectInference(cfg, []string{"openai"}); err != nil {
		t.Fatal(err)
	}
	env := shellEnv{
		readFile: func(string) (string, error) { return "OPENAI_API_KEY=op://vault/openai/key\n", nil },
		run:      func(string, ...string) (string, error) { return "secret\n", nil },
		directInferenceProbe: func(provider, model, key string) error {
			return nil
		},
	}
	probe, probeErr := verifyDirectInference(cfg, env)
	mustNoProbeErr(t, probeErr)
	attempted, verified, failures := probe.Attempted, probe.Verified, probe.Failures
	if attempted != len(cfg.Inference.Models) || verified != attempted || len(failures) != 0 {
		t.Fatalf("attempted=%d verified=%d failures=%v", attempted, verified, failures)
	}
	models, err := callableRuntimeModels(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != attempted {
		t.Fatalf("callable models = %v, want all %d successfully probed models", models, attempted)
	}
}

func TestDirectInferenceProbePromotesBindingsIndependently(t *testing.T) {
	cfg := &config.Config{}
	if err := configureDirectInference(cfg, []string{"openai"}); err != nil {
		t.Fatal(err)
	}
	env := shellEnv{
		readFile: func(string) (string, error) { return "OPENAI_API_KEY=op://vault/openai/key\n", nil },
		run:      func(string, ...string) (string, error) { return "secret\n", nil },
		directInferenceProbe: func(provider, model, key string) error {
			if strings.Contains(model, "sol") {
				return fmt.Errorf("provider rejected model request (HTTP 403)")
			}
			return nil
		},
	}
	probe, probeErr := verifyDirectInference(cfg, env)
	mustNoProbeErr(t, probeErr)
	attempted, verified, failures := probe.Attempted, probe.Verified, probe.Failures
	if attempted != len(cfg.Inference.Models) || verified != attempted-1 || len(failures) != 1 {
		t.Fatalf("attempted=%d verified=%d failures=%v", attempted, verified, failures)
	}
	for _, binding := range cfg.Inference.Models {
		want := !strings.Contains(binding.Upstream, "sol")
		if binding.Verified != want || inference.Callable(cfg, binding) != want {
			t.Fatalf("binding verification was not independent: %+v want-callable=%v", binding, want)
		}
	}
	checks := setupProvidersAxis(cfg, shellEnv{})
	if len(checks) != 1 || checks[0].verdict != verdictReady || !strings.Contains(checks[0].detail, "did not pass live verification") {
		t.Fatalf("partial verification summary = %+v", checks)
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
				// Every AVAILABLE Ollama catalog model is present in this listing, so a
				// catalog id that no real `ollama list` could ever print (the class of
				// bug that shipped `qwen3.5:397b:cloud`, a two-colon reference) shows up
				// here as a missing binding rather than as a model that silently never
				// binds on a user's machine.
				return "NAME ID SIZE MODIFIED\n" +
					"qwen3.5:9b abc 6GB now\n" +
					"glm-5.2:cloud def - now\n" +
					"kimi-k3:cloud ghi - now\n" +
					"deepseek-v4-flash:cloud jkl - now\n" +
					"deepseek-v4-pro:cloud mno - now\n" +
					"kimi-k2.7-code:cloud pqr - now\n" +
					"qwen3.5:397b-cloud stu - now\n", nil
			}
			return "", fmt.Errorf("unexpected command")
		},
	}
	cfg := &config.Config{}
	var out bytes.Buffer
	// "2,4" is local AND cloud. Token 2 alone now means Ollama LOCAL only: the
	// un-asked-for cloud binding was the gated-model delivery mechanism.
	selected, err := setupChooseInference(cfg, env, strings.NewReader("2,4\n"), &out, true)
	if err != nil {
		t.Fatal(err)
	}
	if !selected || inferenceNeedsOnePassword(cfg) {
		t.Fatalf("selected=%v inference=%+v", selected, cfg.Inference)
	}
	if !strings.Contains(out.String(), "2. Ollama local") || len(cfg.Inference.Models) != 6 {
		t.Fatalf("output=%q models=%+v", out.String(), cfg.Inference.Models)
	}
	got := map[string]bool{}
	for _, b := range cfg.Inference.Models {
		got[b.Model] = true
	}
	// kimi-k3:cloud is still listed by `ollama list` above, but the catalog
	// retires it (available=false: "extra usage only", 401s on default plans),
	// so configureOllamaInference must skip it and bind only the callable six.
	for _, want := range []string{
		"ollama/qwen3.5:9b",
		"ollama/glm-5.2:cloud",
		"ollama/deepseek-v4-flash:cloud",
		"ollama/deepseek-v4-pro:cloud",
		"ollama/kimi-k2.7-code:cloud",
		"ollama/qwen3.5:397b-cloud",
	} {
		if !got[want] {
			t.Fatalf("missing Ollama binding %s: %+v", want, cfg.Inference.Models)
		}
	}
	if got["ollama/kimi-k3:cloud"] {
		t.Fatalf("retired kimi-k3:cloud should not be bound: %+v", cfg.Inference.Models)
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

func TestExclusiveBackendKeylessInferenceIgnoresDormantOnePasswordBackend(t *testing.T) {
	cfg := &config.Config{Inference: config.InferenceConfig{
		Backends: map[string]config.InferenceBackend{
			"direct":  {Driver: "native", Auth: "1password"},
			"gateway": {Driver: "openai-compatible", Auth: "none", BaseURL: "http://127.0.0.1:9000/v1"},
		},
		Models: []config.InferenceModelBinding{
			{Model: "anthropic/claude-sonnet-5", Backend: "direct", Upstream: "anthropic/claude-sonnet-5", Available: true},
			{Model: "openai/gpt-5.6-sol", Backend: "gateway", Upstream: "reasoner", Available: true},
		},
		ExclusiveBackend: "gateway",
	}}
	if inferenceNeedsOnePassword(cfg) {
		t.Fatal("a dormant direct backend outside the exclusive backend must not force 1Password")
	}
}

func TestActiveUnverifiedOnePasswordBindingStillNeedsOnePassword(t *testing.T) {
	cfg := &config.Config{Inference: config.InferenceConfig{
		Backends: map[string]config.InferenceBackend{
			"direct": {Driver: "native", Auth: "1password"},
		},
		Models: []config.InferenceModelBinding{
			{Model: "anthropic/claude-sonnet-5", Backend: "direct", Upstream: "anthropic/claude-sonnet-5", Available: false},
		},
	}}
	if !inferenceNeedsOnePassword(cfg) {
		t.Fatal("an allowed direct binding needs 1Password before availability is verified")
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
