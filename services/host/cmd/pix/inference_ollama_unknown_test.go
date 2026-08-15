package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/sys/systest"
	"pix/host/workflow/models"
)

// TestConfigureOllamaInferenceBindsUnknownPulledLocalModel is the confirmed
// bug, fixed: a model the user pulled that is NOT in the shipped catalog used
// to be installed, listed by the daemon, and completely invisible to Pix.
// Reintroducing the old `for _, m := range reg.Models` -only loop (no second
// pass over the listing) fails here.
func TestConfigureOllamaInferenceBindsUnknownPulledLocalModel(t *testing.T) {
	cfg := &config.Config{}
	env := ollamaListEnv(t, []string{"qwen3.5:9b", "llama5.1:70b-instruct"}, "darwin", 64)
	plan, err := models.ConfigureOllamaInference(cfg, env, models.OllamaSelection{Local: true}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.UnknownLocal) != 1 || plan.UnknownLocal[0] != "llama5.1:70b-instruct" {
		t.Fatalf("plan.UnknownLocal = %v, want [llama5.1:70b-instruct]", plan.UnknownLocal)
	}
	var found bool
	for _, b := range cfg.Inference.Models {
		if b.Model == "ollama/llama5.1:70b-instruct" {
			found = true
			if b.Backend != "ollama" || b.Upstream != "llama5.1:70b-instruct" || !b.Available {
				t.Fatalf("unknown-tag binding = %+v", b)
			}
			// pix run --model ollama/<tag> must be able to name it.
			if !strings.HasPrefix(b.Model, "ollama/") {
				t.Fatalf("unknown-tag binding is not a valid --model id: %+v", b)
			}
		}
	}
	if !found {
		t.Fatal("a pulled model absent from models.json was never bound")
	}
}

// TestConfigureOllamaInferenceClassifiesUnknownCloudTagAndSkipsRAMGate: a
// pulled tag the user has never registered in the catalog, but whose name
// follows Ollama's own Cloud naming convention, must be classified cloud —
// never charged against the RAM gate or offered as a free local rung.
func TestConfigureOllamaInferenceClassifiesUnknownCloudTagAndSkipsRAMGate(t *testing.T) {
	cfg := &config.Config{}
	// 16GB total is under LocalFloorTotalGB (24): if this tag were miscounted as
	// LOCAL it would show up in SkippedRAM instead of CloudBound.
	env := ollamaListEnv(t, []string{"minimax-m3:cloud"}, "darwin", 16)
	plan, err := models.ConfigureOllamaInference(cfg, env, models.OllamaSelection{Cloud: true}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.UnknownCloud) != 1 || plan.UnknownCloud[0] != "minimax-m3:cloud" {
		t.Fatalf("plan.UnknownCloud = %v, want [minimax-m3:cloud]", plan.UnknownCloud)
	}
	if len(plan.SkippedRAM) != 0 {
		t.Fatalf("a cloud tag must never be charged against the RAM gate: %v", plan.SkippedRAM)
	}
	var found bool
	for _, b := range cfg.Inference.Models {
		if b.Model == "ollama/minimax-m3:cloud" {
			found = true
		}
	}
	if !found {
		t.Fatal("the cloud tag was classified but never bound")
	}
}

// TestConfigureOllamaInferenceUnclassifiedUnknownTagFailsClosed: a tag with
// NEITHER Ollama's own remote_host marker, NOR the ":cloud"/"-cloud" naming
// convention, NOR an on-disk size past the manifest-stub floor gives this
// package nothing to classify it by. The safe default is to refuse the bind,
// never to assume local (which would present a possible cloud model as free).
func TestConfigureOllamaInferenceUnclassifiedUnknownTagFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// No remote_host, no cloud-suffixed name, and a manifest-sized (not a
		// real weight file's) footprint: neither signal fires.
		_, _ = w.Write([]byte(`{"models":[{"name":"mystery:latest","size":128}]}`))
	}))
	defer srv.Close()
	env := ollamaListEnv(t, nil, "darwin", 64)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	// Route OLLAMA_HOST at our custom listing instead of ollamaListEnv's own.
	systest.Of(env.System).GetenvFn = func(name string) string {
		if name == "OLLAMA_HOST" {
			return u.Host
		}
		return ""
	}

	var out strings.Builder
	plan, err := models.ConfigureOllamaInference(&config.Config{}, env, models.OllamaSelection{Local: true}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.UnknownLocal) != 0 || len(plan.UnknownCloud) != 0 {
		t.Fatalf("an unclassifiable tag must never be bound either way: local=%v cloud=%v", plan.UnknownLocal, plan.UnknownCloud)
	}
	if len(plan.UnknownUnclassified) != 1 || plan.UnknownUnclassified[0] != "mystery:latest" {
		t.Fatalf("plan.UnknownUnclassified = %v, want [mystery:latest]", plan.UnknownUnclassified)
	}
	if !strings.Contains(out.String(), "mystery:latest") || !strings.Contains(out.String(), "not bound") {
		t.Fatalf("the ambiguity must be on the record in the rendered output: %q", out.String())
	}
}

// TestConfigureOllamaInferenceUnknownTagIdempotent: re-running configure
// against the SAME cfg (as a second `pix setup` / `pix models add ollama`
// would) must never duplicate a binding for an unknown tag.
func TestConfigureOllamaInferenceUnknownTagIdempotent(t *testing.T) {
	cfg := &config.Config{}
	env := ollamaListEnv(t, []string{"qwen3.5:9b", "llama5.1:70b-instruct"}, "darwin", 64)
	if _, err := models.ConfigureOllamaInference(cfg, env, models.OllamaSelection{Local: true}, io.Discard); err != nil {
		t.Fatal(err)
	}
	first := len(cfg.Inference.Models)
	if _, err := models.ConfigureOllamaInference(cfg, env, models.OllamaSelection{Local: true}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := len(cfg.Inference.Models); got != first {
		t.Fatalf("re-running configure changed the binding count: %d -> %d (%+v)", first, got, cfg.Inference.Models)
	}
	count := 0
	for _, b := range cfg.Inference.Models {
		if b.Model == "ollama/llama5.1:70b-instruct" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("the unknown tag was bound %d times, want 1", count)
	}
}

// TestConfigureOllamaInferenceUnreachableDaemonStaysNonFatal: a daemon that is
// simply not there (nothing listening) must return a clean, ordinary error —
// never hang and never panic — exactly like the old `ollama list` failure
// path this replaces.
func TestConfigureOllamaInferenceUnreachableDaemonStaysNonFatal(t *testing.T) {
	env := ollamaListEnv(t, nil, "darwin", 64)
	// Point OLLAMA_HOST at a port nothing listens on: connection refused, fast.
	systest.Of(env.System).GetenvFn = func(name string) string {
		if name == "OLLAMA_HOST" {
			return "127.0.0.1:1"
		}
		return ""
	}
	_, err := models.ConfigureOllamaInference(&config.Config{}, env, models.OllamaSelection{Local: true}, io.Discard)
	if err == nil {
		t.Fatal("an unreachable daemon must be reported, not silently treated as an empty listing")
	}
	if !strings.Contains(err.Error(), "could not list Ollama models") {
		t.Fatalf("error = %v, want the ordinary could-not-list refusal", err)
	}
}

// TestConfigureOllamaInferenceNeverPrintsSuccessWordsForAListing pins AGENTS.md
// invariant 12 (success words are earned by a probe): binding from a listing
// is "listed"/"bound", never "verified"/"configured"/"ready" — those are
// promoted only by an actual probe (models.VerifyOllamaInference).
func TestConfigureOllamaInferenceNeverPrintsSuccessWordsForAListing(t *testing.T) {
	cfg := &config.Config{}
	env := ollamaListEnv(t, []string{"qwen3.5:9b", "llama5.1:70b-instruct", "minimax-m3:cloud"}, "darwin", 64)
	var out strings.Builder
	if _, err := models.ConfigureOllamaInference(cfg, env, models.OllamaSelection{Local: true, Cloud: true}, &out); err != nil {
		t.Fatal(err)
	}
	rendered := out.String()
	for _, forbidden := range []string{"verified", "configured", "ready"} {
		if strings.Contains(strings.ToLower(rendered), forbidden) {
			t.Fatalf("a listing pass must never print %q; a listing is not a probe: %q", forbidden, rendered)
		}
	}
	for _, b := range cfg.Inference.Models {
		if b.Verified {
			t.Fatalf("a listing must never mark a binding verified: %+v", b)
		}
	}
}
