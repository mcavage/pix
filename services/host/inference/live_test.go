package inference

import (
	"testing"
	"time"

	"pix/host/config"
	"pix/host/routing"
)

// TestCallableRuntimeModelsIncludesUnknownOllamaBinding pins BLOCKER 1: an
// Ollama tag the user pulled that models.json has never heard of used to be
// bound in config.toml (workflow/models.ConfigureOllamaInference's second
// pass) and then silently dropped right here, in manifestModels, because
// reg.Get(b.Model) never finds a catalog row for it. That made the binding a
// dead letter: `pix run --model ollama/<tag>` reads AllowsModel, which reads
// CallableRuntimeModels, which is exactly this function — so a fully bound,
// fully probed model was still refused with "model is not available through
// the configured inference backends". Reintroducing a bare `if !ok {
// continue }` here reproduces that refusal and fails this test.
func TestCallableRuntimeModelsIncludesUnknownOllamaBinding(t *testing.T) {
	cfg := &config.Config{Inference: config.InferenceConfig{
		Backends: map[string]config.InferenceBackend{
			"ollama": {Driver: "ollama", BaseURL: "http://127.0.0.1:11434/v1", Auth: "none"},
		},
		Models: []config.InferenceModelBinding{
			// Not in defaults/models.json: an unknown pulled tag, bound and
			// probe-verified exactly as ConfigureOllamaInference's unknown-tag
			// pass + a live probe would leave it.
			{Model: "ollama/llama5.1:70b-instruct", Backend: "ollama", Upstream: "llama5.1:70b-instruct", Available: true, Verified: true},
		},
	}}

	ids, err := CallableRuntimeModels(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(ids, "ollama/llama5.1:70b-instruct") {
		t.Fatalf("CallableRuntimeModels() = %v, want it to include the unknown-but-bound tag", ids)
	}
	if !AllowsModel(cfg, "ollama/llama5.1:70b-instruct") {
		t.Fatal("AllowsModel refused a bound, probe-verified model with no catalog row — this is the `pix run` refusal the bug reproduces")
	}

	// The manifest itself must carry the entry too — the bridge reads THIS,
	// not just the id list — with no invented price or context window (both
	// zero/omitted is correct: there is no catalog row to draw them from).
	reg, err := routing.LoadRegistry()
	if err != nil {
		t.Fatal(err)
	}
	models := manifestModels(cfg, reg)
	var found *runtimeModel
	for i := range models {
		if models[i].ID == "ollama/llama5.1:70b-instruct" {
			found = &models[i]
		}
	}
	if found == nil {
		t.Fatalf("manifestModels() = %+v, missing the unknown-tag entry", models)
	}
	if found.CatalogModel != "" {
		t.Fatalf("CatalogModel = %q, want empty: there is no catalog row, and a synthesized one must never look like there is", found.CatalogModel)
	}
	if found.InputCost != 0 || found.OutputCost != 0 {
		t.Fatalf("a model with no catalog row must never be priced: %+v", found)
	}
	if found.ContextWindow != 0 || found.MaxTokens != 0 {
		t.Fatalf("a model with no catalog row must never have an invented context window/max tokens: %+v", found)
	}
}

// TestCallableRuntimeModelsUnknownBindingNeverEntersCatalogRouting pins the
// other half of the fix: making the tag callable by name must never let it
// leak into the routing registry (an unscored, uncataloged model must never
// win an intent — see routing/resolve_test.go's overlord-fallback precedent).
func TestCallableRuntimeModelsUnknownBindingNeverEntersCatalogRouting(t *testing.T) {
	cfg := &config.Config{Inference: config.InferenceConfig{
		Backends: map[string]config.InferenceBackend{
			"ollama": {Driver: "ollama", BaseURL: "http://127.0.0.1:11434/v1", Auth: "none"},
		},
		Models: []config.InferenceModelBinding{
			{Model: "ollama/llama5.1:70b-instruct", Backend: "ollama", Upstream: "llama5.1:70b-instruct", Available: true, Verified: true},
		},
	}}
	reg, err := routing.LoadRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Get("ollama/llama5.1:70b-instruct"); ok {
		t.Fatal("the base catalog must never contain the unknown tag before compile")
	}
	_, manifest, err := CompileInferenceRuntime(cfg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Models) != 1 || manifest.Models[0].ID != "ollama/llama5.1:70b-instruct" {
		t.Fatalf("manifest.Models = %+v, want exactly the one unknown binding", manifest.Models)
	}
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
