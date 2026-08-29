package inference

import (
	"testing"

	"pix/host/config"
)

func cfgWith(roster []string, backends map[string]config.InferenceBackend, models ...config.InferenceModelBinding) *config.Config {
	return &config.Config{Inference: config.InferenceConfig{
		AllowedModels: roster, Backends: backends, Models: models,
	}}
}

var (
	nativeBackends = map[string]config.InferenceBackend{
		"anthropic": {Driver: "native", Auth: "1password", KeyEnv: "ANTHROPIC_API_KEY"},
	}
	ollamaBackends = map[string]config.InferenceBackend{
		"ollama": {Driver: "ollama", BaseURL: "http://127.0.0.1:11434/v1", Auth: "none"},
	}
)

// TestCallable enumerates every way a binding fails to be callable. Each row is
// a state that HAS shipped as a bug: a binding whose probe never ran, one
// outside the roster, one pointing at a backend that no longer exists, and an
// ollama binding trusted on a listing alone (auth is "none", so without the
// driver check Verified was never required and a gated cloud model 401'd at
// call time).
func TestCallable(t *testing.T) {
	probed := config.InferenceModelBinding{
		Model: "anthropic/claude-opus-5", Backend: "anthropic",
		Upstream: "anthropic/claude-opus-5", Available: true, Verified: true, VerifiedBy: "probe",
	}
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		b    config.InferenceModelBinding
		want bool
	}{
		{"probed, in roster", cfgWith([]string{probed.Model}, nativeBackends, probed), probed, true},
		{"probed, empty roster means no restriction", cfgWith(nil, nativeBackends, probed), probed, true},
		{"outside the roster", cfgWith([]string{"google/gemini-3.7-flash"}, nativeBackends, probed), probed, false},
		{"unprobed 1password binding", cfgWith(nil, nativeBackends, unverified(probed)), unverified(probed), false},
		{"backend no longer configured", cfgWith(nil, map[string]config.InferenceBackend{}, probed), probed, false},
		{"not available", cfgWith(nil, nativeBackends, unavailable(probed)), unavailable(probed), false},
		{"nil config", nil, probed, false},
	} {
		if got := Callable(tc.cfg, tc.b); got != tc.want {
			t.Errorf("%s: Callable = %v, want %v", tc.name, got, tc.want)
		}
	}

	// Ollama: auth is "none", so only the driver check forces host proof.
	listed := config.InferenceModelBinding{Model: "ollama/glm-5.2:cloud", Backend: "ollama", Upstream: "glm-5.2:cloud", Available: true}
	if Callable(cfgWith(nil, ollamaBackends, listed), listed) {
		t.Error("an ollama binding with no probe must not be callable: a listing proves nothing about entitlement")
	}
	proven := listed
	proven.Verified, proven.VerifiedBy = true, "probe"
	if !Callable(cfgWith(nil, ollamaBackends, proven), proven) {
		t.Error("a probe-verified ollama binding must be callable")
	}
}

func unverified(b config.InferenceModelBinding) config.InferenceModelBinding {
	b.Verified, b.VerifiedBy = false, ""
	return b
}

func unavailable(b config.InferenceModelBinding) config.InferenceModelBinding {
	b.Available = false
	return b
}

// TestCatalogForBindings_NarrowsOnlyWhatWasBound is the guard on the other
// direction of the availability bug. Bindings are the authority ONLY once they
// exist: filtering an empty set marks every catalog model unavailable and
// describes nothing useful, which is why Configured() gates the narrowing.
func TestCatalogForBindings_NarrowsOnlyWhatWasBound(t *testing.T) {
	catalog := &Catalog{Models: []Model{
		{ID: "anthropic/claude-opus-5", Provider: "anthropic", Available: true},
		{ID: "openai/gpt-5.6-sol", Provider: "openai", Available: true},
	}}

	empty := &config.Config{}
	if Configured(empty) {
		t.Error("a config with no bindings must not claim to be the availability authority")
	}
	for _, m := range catalog.Models {
		if !m.Available {
			t.Errorf("%s was narrowed away on an unconfigured host", m.ID)
		}
	}

	probed := config.InferenceModelBinding{
		Model: "anthropic/claude-opus-5", Backend: "anthropic",
		Upstream: "anthropic/claude-opus-5", Available: true, Verified: true, VerifiedBy: "probe",
	}
	cfg := cfgWith(nil, nativeBackends, probed)
	if !Configured(cfg) {
		t.Fatal("a config with bindings IS the availability authority")
	}
	narrowed := CatalogForBindings(catalog, Bindings(cfg), "")
	got := map[string]bool{}
	for _, m := range narrowed.Models {
		got[m.ID] = m.Available
	}
	if !got["anthropic/claude-opus-5"] {
		t.Error("the probed model must stay available")
	}
	if got["openai/gpt-5.6-sol"] {
		t.Error("openai has no backend here; leaving it available is the bug that told a keyless host it could call it")
	}
	// The catalog the caller passed in must not be mutated: a second reader
	// still has to see the shipped truth, not one host's narrowing.
	if !catalog.Models[1].Available {
		t.Error("CatalogForBindings mutated the caller's catalog in place")
	}
}

// TestRuntimeID: the upstream id is qualified by its backend, without
// double-qualifying one that already is.
func TestRuntimeID(t *testing.T) {
	for _, tc := range []struct{ backend, upstream, want string }{
		{"ollama", "glm-5.2:cloud", "ollama/glm-5.2:cloud"},
		{"anthropic", "anthropic/claude-opus-5", "anthropic/claude-opus-5"},
		// Qualified, but by a DIFFERENT provider: a gateway serving anthropic ids
		// must still be addressed through the gateway.
		{"gateway", "anthropic/claude-opus-5", "gateway/anthropic/claude-opus-5"},
	} {
		if got := RuntimeID(Binding{Backend: tc.backend, UpstreamID: tc.upstream}); got != tc.want {
			t.Errorf("RuntimeID(%s, %s) = %q, want %q", tc.backend, tc.upstream, got, tc.want)
		}
	}
}

// TestBindings_DropsUncallableEntirely: consumers read PRESENCE. Emitting an
// uncallable binding with Available:false has been misread as "it is there"
// before, so it is omitted instead.
func TestBindings_DropsUncallableEntirely(t *testing.T) {
	probed := config.InferenceModelBinding{
		Model: "anthropic/claude-opus-5", Backend: "anthropic",
		Upstream: "anthropic/claude-opus-5", Available: true, Verified: true, VerifiedBy: "probe",
	}
	cfg := cfgWith(nil, nativeBackends, probed, unverified(probed))
	got := Bindings(cfg)
	if len(got) != 1 || got[0].Model != probed.Model || !got[0].Available {
		t.Fatalf("Bindings = %+v, want exactly the one probed binding", got)
	}
	if len(Bindings(nil)) != 0 {
		t.Error("Bindings(nil) must be empty")
	}
}

// TestTopologyAllowed_IgnoresTheRoster: a caller BUILDING the roster must not
// have its candidate list filtered by the answer it is about to compute.
func TestTopologyAllowed_IgnoresTheRoster(t *testing.T) {
	b := config.InferenceModelBinding{Model: "ollama/qwen3.5:9b", Backend: "ollama", Available: true}
	cfg := cfgWith([]string{"anthropic/claude-opus-5"}, ollamaBackends, b)
	if Allowed(cfg, b) {
		t.Error("Allowed must honor the roster")
	}
	if !TopologyAllowed(cfg, b) {
		t.Error("TopologyAllowed must ignore the roster")
	}

	// An exclusive pack owns the whole surface: a user binding is out regardless.
	packed := cfgWith(nil, ollamaBackends, b)
	packed.Inference.ExclusiveSource = "/packs/corp"
	if TopologyAllowed(packed, b) {
		t.Error("a pack-exclusive host must exclude a non-pack binding")
	}
}
