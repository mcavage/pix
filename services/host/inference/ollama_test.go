package inference

import (
	"testing"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/sys/systest"
)

// TestOllamaEndpointFor pins the ONE resolver every surface names an Ollama
// address through. A non-default OLLAMA_HOST must never produce a verdict, a
// probe or an offer about an endpoint the daemon does not use.
func TestOllamaEndpointFor(t *testing.T) {
	for _, tc := range []struct {
		env      string
		wantURL  string
		wantPort int
		wantSrc  string
	}{
		{"", "http://127.0.0.1:11434", 11434, "default"},
		{"0.0.0.0:11434", "http://0.0.0.0:11434", 11434, "OLLAMA_HOST"},
		{"box.local", "http://box.local:11434", 11434, "OLLAMA_HOST"},
		{"box.local:9999", "http://box.local:9999", 9999, "OLLAMA_HOST"},
		{"http://10.0.0.2:1234", "http://10.0.0.2:1234", 1234, "OLLAMA_HOST"},
		{"https://ollama.internal:443", "https://ollama.internal:443", 443, "OLLAMA_HOST"},
	} {
		ep := OllamaEndpointFor(hostenv.Env{System: &systest.Fake{GetenvFn: func(string) string { return tc.env }}})
		if ep.URL != tc.wantURL || ep.Port != tc.wantPort || ep.Source != tc.wantSrc {
			t.Errorf("OLLAMA_HOST=%q -> %+v, want url=%s port=%d source=%s", tc.env, ep, tc.wantURL, tc.wantPort, tc.wantSrc)
		}
	}
	// The zero value still names an address: an evidence line may never trail
	// off into an empty endpoint.
	if got := (OllamaEndpoint{}).String(); got != "http://127.0.0.1:11434" {
		t.Errorf("zero endpoint reads as %q", got)
	}
}

// TestOllamaTagFor: the catalog id carries a provider prefix `ollama pull` and
// `ollama list` do not speak.
func TestOllamaTagFor(t *testing.T) {
	if got := OllamaTagFor("ollama/qwen3.5:9b"); got != "qwen3.5:9b" {
		t.Errorf("OllamaTagFor = %q", got)
	}
	if got := OllamaTagFor("qwen3.5:9b"); got != "qwen3.5:9b" {
		t.Errorf("an untagged id must pass through, got %q", got)
	}
}

// TestOllamaBindingDriver keys off the BACKEND's driver, not the model name: a
// binding named "ollama/..." on a non-ollama backend is not an ollama binding,
// and a nil config can never claim one.
func TestOllamaBindingDriver(t *testing.T) {
	cfg := &config.Config{Inference: config.InferenceConfig{Backends: map[string]config.InferenceBackend{
		"local":  {Driver: "ollama"},
		"remote": {Driver: "openai"},
	}}}
	if !OllamaBindingDriver(cfg, config.InferenceModelBinding{Backend: "local"}) {
		t.Error("an ollama-driver backend must read as an ollama binding")
	}
	if OllamaBindingDriver(cfg, config.InferenceModelBinding{Model: "ollama/x", Backend: "remote"}) {
		t.Error("the model name must not decide the driver")
	}
	if OllamaBindingDriver(cfg, config.InferenceModelBinding{Backend: "missing"}) {
		t.Error("an unknown backend must not read as ollama")
	}
	if OllamaBindingDriver(nil, config.InferenceModelBinding{Backend: "local"}) {
		t.Error("a nil config must claim nothing")
	}
}

// TestConfiguredSummaryCountsOnlyCallable: a bound-but-unproven model is not a
// callable one, and the summary is what setup reads to decide whether this
// host needs inference wired at all.
func TestConfiguredSummaryCountsOnlyCallable(t *testing.T) {
	cfg := &config.Config{Inference: config.InferenceConfig{
		Backends: map[string]config.InferenceBackend{"local": {Driver: "ollama", Auth: "none"}},
		Models: []config.InferenceModelBinding{
			{Model: "ollama/a", Backend: "local", Available: true, Verified: true, VerifiedBy: config.VerifiedByProbe},
			{Model: "ollama/b", Backend: "local", Available: true},
		},
	}}
	count, backends := ConfiguredSummary(cfg)
	if count != 1 || len(backends) != 1 || backends[0] != "local" {
		t.Errorf("ConfiguredSummary = %d, %v; want 1 callable on [local]", count, backends)
	}
	if n, _ := ConfiguredSummary(nil); n != 0 {
		t.Errorf("a nil config must summarize to nothing, got %d", n)
	}
}
