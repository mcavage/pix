package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/inference"
	"pix/host/sys/systest"
	"pix/host/workflow/launch"
	"pix/host/workflow/models"
	"pix/host/workflow/provision"
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
	routes, manifest, err := inference.CompileInferenceRuntime(cfg, time.Unix(1, 0), inference.RosterInput{})
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
	_, manifest, err := inference.CompileInferenceRuntime(cfg, time.Unix(1, 0), inference.RosterInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Models) != 1 || !manifest.Models[0].AdaptiveThinking {
		t.Fatalf("adaptive thinking metadata missing: %+v", manifest.Models)
	}
}

func TestConfigureDirectInferenceUsesCatalog(t *testing.T) {
	cfg := &config.Config{}
	if err := models.ConfigureDirectInference(cfg, []string{"anthropic"}); err != nil {
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
	if err := models.ConfigureDirectInference(cfg, []string{"anthropic", "openai"}); err != nil {
		t.Fatal(err)
	}
	// The roster runs AFTER verification now, and only offers what answered a
	// request — so a probed roster is the only shape it ever sees in models.
	for i := range cfg.Inference.Models {
		cfg.Inference.Models[i].Verified = true
		cfg.Inference.Models[i].VerifiedBy = config.VerifiedByProbe
	}
	if err := models.ConfigureModelRoster(cfg, strings.NewReader(""), &bytes.Buffer{}, false, "openai/gpt-5.6-sol"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.Inference.AllowedModels, ","); got != "openai/gpt-5.6-sol" {
		t.Fatalf("allowed models = %q", got)
	}
	routes, manifest, err := inference.CompileInferenceRuntime(cfg, time.Unix(1, 0), inference.RosterInput{})
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
	if err := models.ConfigureModelRoster(cfg, strings.NewReader(""), &bytes.Buffer{}, true, ""); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.Inference.AllowedModels, ","); got != "ollama/kimi-k3:cloud" {
		t.Fatalf("exclusive pack erased personal roster: %q", got)
	}
}

func TestConfigureModelRosterRejectsUnavailableChoice(t *testing.T) {
	cfg := &config.Config{}
	if err := models.ConfigureDirectInference(cfg, []string{"openai"}); err != nil {
		t.Fatal(err)
	}
	for i := range cfg.Inference.Models {
		cfg.Inference.Models[i].Verified = true
		cfg.Inference.Models[i].VerifiedBy = config.VerifiedByProbe
	}
	err := models.ConfigureModelRoster(cfg, strings.NewReader(""), &bytes.Buffer{}, false, "ollama/kimi-k3:cloud")
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("error = %v", err)
	}
}

func TestDirectInferenceProbeDoesNotVerifyRejectedOrUnavailableKey(t *testing.T) {
	const key = "sk-valid-looking-but-unauthorized"
	for _, probeFailure := range []string{"provider rejected model request (HTTP 401)", "probe unavailable"} {
		t.Run(probeFailure, func(t *testing.T) {
			cfg := &config.Config{}
			if err := models.ConfigureDirectInference(cfg, []string{"openai"}); err != nil {
				t.Fatal(err)
			}
			env := hostenv.Env{System: &systest.Fake{ReadFileFn: func(string) (string, error) { return "OPENAI_API_KEY=op://vault/openai/key\n", nil }, RunFn: func(name string, args ...string) (string, error) {
				if name == "op" && len(args) == 2 && args[0] == "read" {
					return key + "\n", nil
				}
				return "", fmt.Errorf("unexpected command")
			}}, DirectInference: func(provider, model, gotKey string) error {
				if provider != "openai" || model == "" || gotKey != key {
					return fmt.Errorf("bad probe args provider=%q model=%q key-match=%v", provider, model, gotKey == key)
				}
				return fmt.Errorf("%s", probeFailure)
			}}
			probe, probeErr := models.VerifyDirectInference(cfg, env)
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
			models, err := inference.CallableRuntimeModels(cfg)
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
	if err := models.ConfigureDirectInference(cfg, []string{"openai"}); err != nil {
		t.Fatal(err)
	}
	env := hostenv.Env{System: &systest.Fake{ReadFileFn: func(string) (string, error) { return "OPENAI_API_KEY=op://vault/openai/key\n", nil }, RunFn: func(string, ...string) (string, error) { return "secret\n", nil }}, DirectInference: func(provider, model, key string) error {
		return nil
	}}
	probe, probeErr := models.VerifyDirectInference(cfg, env)
	mustNoProbeErr(t, probeErr)
	attempted, verified, failures := probe.Attempted, probe.Verified, probe.Failures
	if attempted != len(cfg.Inference.Models) || verified != attempted || len(failures) != 0 {
		t.Fatalf("attempted=%d verified=%d failures=%v", attempted, verified, failures)
	}
	models, err := inference.CallableRuntimeModels(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != attempted {
		t.Fatalf("callable models = %v, want all %d successfully probed models", models, attempted)
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
	if inference.InferenceNeedsOnePassword(cfg) {
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
	if inference.InferenceNeedsOnePassword(cfg) {
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
	if !inference.InferenceNeedsOnePassword(cfg) {
		t.Fatal("an allowed direct binding needs 1Password before availability is verified")
	}
}

func TestBuildSbxArgsConstrainsModelCycleToCallableBindings(t *testing.T) {
	args := launch.BuildSbxArgs(&config.Config{}, launch.RunOpts{
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
	if !inference.AllowsModel(cfg, "gateway/reasoner") {
		t.Fatal("materialized id should be allowed")
	}
	if inference.AllowsModel(cfg, "openai/gpt-5.6-sol") {
		t.Fatal("canonical catalog id must not bypass its configured backend")
	}
}

func TestSetupPackFlagIsRepeatableAndOrdered(t *testing.T) {
	o, err := provision.ParseSetupArgs([]string{"--pack", "one", "--pack=two", "--with", "optional"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(o.Packs, ","); got != "one,two" {
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
	spec, err := inference.InferenceKitSpec(cfg)
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
	spec, err := inference.InferenceKitSpec(cfg)
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
	spec, err := inference.InferenceKitSpec(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`service: "sbx-login"`, `name: "DOCKER_TOKEN"`, `proxyManaged: true`, `header: "Authorization"`, `format: "Bearer %s"`} {
		if !strings.Contains(spec, want) {
			t.Fatalf("spec missing %q:\n%s", want, spec)
		}
	}
}

// TestInferenceKitSpecRejectsSbxSessionBackendMissingBothCredentialFields
// covers a hand-edited ~/.config/pix/config.toml: the pack loader
// (packinfo.Load) rejects a pack that ships this shape, but nothing enforced
// the same rule for a directly-authored config, so InferenceKitSpec used to
// happily emit `service: ""` / `name: ""` into the kit YAML instead of failing.
func TestInferenceKitSpecRejectsSbxSessionBackendMissingBothCredentialFields(t *testing.T) {
	cfg := &config.Config{Inference: config.InferenceConfig{
		Backends: map[string]config.InferenceBackend{
			"gateway": {Driver: "openai-compatible", BaseURL: "https://gateway.example.test/v1", Auth: "sbx-session"},
		},
		Models: []config.InferenceModelBinding{{Model: "openai/gpt-5.6-sol", Backend: "gateway", Upstream: "reasoner", Available: true}},
	}}
	_, err := inference.InferenceKitSpec(cfg)
	if err == nil {
		t.Fatal("want error for sbx-session backend missing credential_service and key_env, got nil")
	}
	if !strings.Contains(err.Error(), "gateway") || !strings.Contains(err.Error(), "credential_service") || !strings.Contains(err.Error(), "key_env") {
		t.Fatalf("error %q does not name the backend and both missing fields", err.Error())
	}
}

func TestInferenceKitSpecRejectsSbxSessionBackendMissingKeyEnvOnly(t *testing.T) {
	cfg := &config.Config{Inference: config.InferenceConfig{
		Backends: map[string]config.InferenceBackend{
			"gateway": {
				Driver: "openai-compatible", BaseURL: "https://gateway.example.test/v1", Auth: "sbx-session",
				CredentialService: "session-login",
			},
		},
		Models: []config.InferenceModelBinding{{Model: "openai/gpt-5.6-sol", Backend: "gateway", Upstream: "reasoner", Available: true}},
	}}
	_, err := inference.InferenceKitSpec(cfg)
	if err == nil {
		t.Fatal("want error for sbx-session backend missing key_env, got nil")
	}
	if !strings.Contains(err.Error(), "gateway") {
		t.Fatalf("error %q does not name the backend", err.Error())
	}
}

func TestInferenceKitSpecRejectsSbxSessionBackendMissingCredentialServiceOnly(t *testing.T) {
	cfg := &config.Config{Inference: config.InferenceConfig{
		Backends: map[string]config.InferenceBackend{
			"gateway": {
				Driver: "openai-compatible", BaseURL: "https://gateway.example.test/v1", Auth: "sbx-session",
				KeyEnv: "SESSION_TOKEN",
			},
		},
		Models: []config.InferenceModelBinding{{Model: "openai/gpt-5.6-sol", Backend: "gateway", Upstream: "reasoner", Available: true}},
	}}
	_, err := inference.InferenceKitSpec(cfg)
	if err == nil {
		t.Fatal("want error for sbx-session backend missing credential_service, got nil")
	}
	if !strings.Contains(err.Error(), "gateway") {
		t.Fatalf("error %q does not name the backend", err.Error())
	}
}

// TestInferenceKitSpecMergesTwoHeadersUnderOneCredentialIdentity covers the
// dedupe bug directly: two backends share one credential identity
// (service+key_env) but disagree on header/format for the SAME domain. The
// old dedupe key was service+key_env+hostname, so the second backend's rule
// was silently dropped instead of surviving as its own inject rule.
func TestInferenceKitSpecMergesTwoHeadersUnderOneCredentialIdentity(t *testing.T) {
	cfg := &config.Config{Inference: config.InferenceConfig{
		Backends: map[string]config.InferenceBackend{
			"one": {
				Driver: "openai-compatible", BaseURL: "https://gateway.example.test/v1", Auth: "sbx-session",
				KeyEnv: "TOKEN", CredentialService: "svc", CredentialHeader: "Authorization", CredentialFormat: "Bearer %s",
			},
			"two": {
				Driver: "openai-compatible", BaseURL: "https://gateway.example.test/v2", Auth: "sbx-session",
				KeyEnv: "TOKEN", CredentialService: "svc", CredentialHeader: "x-api-key", CredentialFormat: "%s",
			},
		},
		Models: []config.InferenceModelBinding{
			{Model: "openai/gpt-5.6-sol", Backend: "one", Upstream: "reasoner-one", Available: true},
			{Model: "anthropic/claude-sonnet-5", Backend: "two", Upstream: "reasoner-two", Available: true},
		},
	}}
	spec, err := inference.InferenceKitSpec(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(spec, "- service:"); n != 1 {
		t.Fatalf("want exactly one credential entry for the shared service+name identity, got %d:\n%s", n, spec)
	}
	want := "credentials:\n" +
		"  - service: \"svc\"\n" +
		"    apiKey:\n" +
		"      name: \"TOKEN\"\n" +
		"      proxyManaged: true\n" +
		"      inject:\n" +
		"        - domain: \"gateway.example.test\"\n" +
		"          header: \"Authorization\"\n" +
		"          format: \"Bearer %s\"\n" +
		"        - domain: \"gateway.example.test\"\n" +
		"          header: \"x-api-key\"\n" +
		"          format: \"%s\"\n"
	if !strings.HasSuffix(spec, want) {
		t.Fatalf("spec =\n%s\nwant suffix:\n%s", spec, want)
	}
}

// TestInferenceKitSpecCollapsesIdenticalInjectRule covers the opposite edge:
// two backends land on the exact same (domain, header, format) for a shared
// identity — that must dedupe to ONE inject rule, not two identical ones.
func TestInferenceKitSpecCollapsesIdenticalInjectRule(t *testing.T) {
	cfg := &config.Config{Inference: config.InferenceConfig{
		Backends: map[string]config.InferenceBackend{
			"one": {
				Driver: "openai-compatible", BaseURL: "https://gateway.example.test/v1", Auth: "sbx-session",
				KeyEnv: "TOKEN", CredentialService: "svc", CredentialHeader: "Authorization", CredentialFormat: "Bearer %s",
			},
			"two": {
				Driver: "openai-compatible", BaseURL: "https://gateway.example.test/v2", Auth: "sbx-session",
				KeyEnv: "TOKEN", CredentialService: "svc", CredentialHeader: "Authorization", CredentialFormat: "Bearer %s",
			},
		},
		Models: []config.InferenceModelBinding{
			{Model: "openai/gpt-5.6-sol", Backend: "one", Upstream: "reasoner-one", Available: true},
			{Model: "anthropic/claude-sonnet-5", Backend: "two", Upstream: "reasoner-two", Available: true},
		},
	}}
	spec, err := inference.InferenceKitSpec(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(spec, "- service:"); n != 1 {
		t.Fatalf("want exactly one credential entry, got %d:\n%s", n, spec)
	}
	if n := strings.Count(spec, "- domain:"); n != 1 {
		t.Fatalf("want exactly one inject rule (identical rule must dedupe), got %d:\n%s", n, spec)
	}
	want := "credentials:\n" +
		"  - service: \"svc\"\n" +
		"    apiKey:\n" +
		"      name: \"TOKEN\"\n" +
		"      proxyManaged: true\n" +
		"      inject:\n" +
		"        - domain: \"gateway.example.test\"\n" +
		"          header: \"Authorization\"\n" +
		"          format: \"Bearer %s\"\n"
	if !strings.HasSuffix(spec, want) {
		t.Fatalf("spec =\n%s\nwant suffix:\n%s", spec, want)
	}
}

// TestInferenceKitSpecCoversTwoDomainsUnderOneCredentialIdentitySorted covers
// the other real shape (see the hand-authored anthropic block in
// pi-kit/spec.yaml): one identity legitimately used against two different
// domains. Both must survive as separate inject rules under ONE credential
// entry, sorted by domain regardless of backend map iteration order.
func TestInferenceKitSpecCoversTwoDomainsUnderOneCredentialIdentitySorted(t *testing.T) {
	cfg := &config.Config{Inference: config.InferenceConfig{
		Backends: map[string]config.InferenceBackend{
			"z-backend": {
				Driver: "openai-compatible", BaseURL: "https://z-gateway.example.test/v1", Auth: "sbx-session",
				KeyEnv: "TOKEN", CredentialService: "svc", CredentialHeader: "Authorization", CredentialFormat: "Bearer %s",
			},
			"a-backend": {
				Driver: "openai-compatible", BaseURL: "https://a-gateway.example.test/v1", Auth: "sbx-session",
				KeyEnv: "TOKEN", CredentialService: "svc", CredentialHeader: "Authorization", CredentialFormat: "Bearer %s",
			},
		},
		Models: []config.InferenceModelBinding{
			{Model: "openai/gpt-5.6-sol", Backend: "z-backend", Upstream: "reasoner-z", Available: true},
			{Model: "anthropic/claude-sonnet-5", Backend: "a-backend", Upstream: "reasoner-a", Available: true},
		},
	}}
	spec, err := inference.InferenceKitSpec(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(spec, "- service:"); n != 1 {
		t.Fatalf("want exactly one credential entry for the shared identity, got %d:\n%s", n, spec)
	}
	want := "credentials:\n" +
		"  - service: \"svc\"\n" +
		"    apiKey:\n" +
		"      name: \"TOKEN\"\n" +
		"      proxyManaged: true\n" +
		"      inject:\n" +
		"        - domain: \"a-gateway.example.test\"\n" +
		"          header: \"Authorization\"\n" +
		"          format: \"Bearer %s\"\n" +
		"        - domain: \"z-gateway.example.test\"\n" +
		"          header: \"Authorization\"\n" +
		"          format: \"Bearer %s\"\n"
	if !strings.HasSuffix(spec, want) {
		t.Fatalf("spec =\n%s\nwant suffix (domains sorted a- before z-):\n%s", spec, want)
	}
}

// TestInferenceKitSpecOrdersCredentialsByServiceThenNameDeterministically
// covers determinism across the whole `credentials:` block: two UNRELATED
// identities (different service+name pairs, so no merge happens) must always
// emit in the same service-then-name order, independent of
// cfg.Inference.Backends map iteration order (Go map ranges are randomized
// per run), so re-synthesizing the kit never produces a spurious diff.
func TestInferenceKitSpecOrdersCredentialsByServiceThenNameDeterministically(t *testing.T) {
	cfg := &config.Config{Inference: config.InferenceConfig{
		Backends: map[string]config.InferenceBackend{
			"zsvc-backend": {
				Driver: "openai-compatible", BaseURL: "https://gateway-z.example.test/v1", Auth: "sbx-session",
				KeyEnv: "ZTOKEN", CredentialService: "zsvc", CredentialHeader: "Authorization", CredentialFormat: "Bearer %s",
			},
			"asvc-backend": {
				Driver: "openai-compatible", BaseURL: "https://gateway-a.example.test/v1", Auth: "sbx-session",
				KeyEnv: "ATOKEN", CredentialService: "asvc", CredentialHeader: "Authorization", CredentialFormat: "Bearer %s",
			},
		},
		Models: []config.InferenceModelBinding{
			{Model: "openai/gpt-5.6-sol", Backend: "zsvc-backend", Upstream: "reasoner-z", Available: true},
			{Model: "anthropic/claude-sonnet-5", Backend: "asvc-backend", Upstream: "reasoner-a", Available: true},
		},
	}}
	var prior string
	for i := 0; i < 20; i++ {
		spec, err := inference.InferenceKitSpec(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if prior != "" && spec != prior {
			t.Fatalf("InferenceKitSpec is not deterministic across runs:\nrun N-1:\n%s\nrun N:\n%s", prior, spec)
		}
		prior = spec
		aSvcIdx, zSvcIdx := strings.Index(spec, `service: "asvc"`), strings.Index(spec, `service: "zsvc"`)
		if aSvcIdx < 0 || zSvcIdx < 0 || aSvcIdx > zSvcIdx {
			t.Fatalf("want asvc before zsvc regardless of map order, got:\n%s", spec)
		}
		prior = spec
	}
}
