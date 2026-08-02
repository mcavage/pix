package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"pix/host/config"
	"pix/host/inference"
)

// ollamaAddEnv is ollamaListEnv plus an OLLAMA_HOST pointing at a fake daemon,
// so reconcileOllamaInference's probe reaches a server this test controls.
func ollamaAddEnv(t *testing.T, tags []string, totalGB float64, endpoint string) shellEnv {
	t.Helper()
	env := ollamaListEnv(tags, "darwin", totalGB)
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	env.getenv = func(k string) string {
		if k == "OLLAMA_HOST" {
			return u.Host
		}
		return ""
	}
	// The REAL probe against the fake daemon. Stubbing this out instead would
	// make the test agree with itself and prove nothing about whether a binding
	// is reachable — and leaving it nil is worse: verifyOllamaInference treats a
	// nil probe seam as "nothing to do" and returns 0 verified, 0 attempted, no
	// failures, which is indistinguishable from a clean run that found nothing.
	env.ollamaInferenceProbe = liveOllamaInferenceProbe
	return env
}

// TestModelsAddAcceptsOllama is the reported bug, verbatim: `pix models add
// ollama` answered "unknown provider \"ollama\" (want one of: anthropic,
// google, openai)". Ollama is a provider you can add and has no key ref, so
// deriving the accepted list from providerKeyRefOrder alone both rejected it
// and told the user it did not exist.
func TestModelsAddAcceptsOllama(t *testing.T) {
	names := providerNames()
	if !containsString(names, "ollama") {
		t.Fatalf("providerNames() = %v, want ollama in the list", names)
	}
	// The keyed lookup must still NOT claim ollama: it has no env var, and a
	// struct with an empty envVar would send `pix secret set "" op://...`.
	if p, ok := providerByName("ollama"); ok {
		t.Fatalf("providerByName(\"ollama\") = %+v, want not-found (it is keyless)", p)
	}
}

// TestReconcileOllamaInference_BindsProbesAndWidens is the end-to-end contract:
// a listed model becomes a binding, the binding is proven by a real generate
// against the resolved endpoint, and the roster grows to include it. Any one of
// those missing leaves the model exactly as inert as before the command existed.
func TestReconcileOllamaInference_BindsProbesAndWidens(t *testing.T) {
	var probed []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		probed = append(probed, strings.TrimSpace(toStr(body["model"])))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"response":"OK"}`))
	}))
	defer srv.Close()

	// A roster that already names anthropic AND stamps ollama as offered: the
	// state in which widenRosterForNewProviders alone does nothing.
	cfg := &config.Config{Inference: config.InferenceConfig{
		AllowedModels:   []string{"anthropic/claude-opus-5"},
		RosterProviders: []string{"anthropic", "ollama"},
	}}
	env := ollamaAddEnv(t, []string{"qwen3.5:9b"}, 32, srv.URL)

	res, plan, err := reconcileOllamaInference(cfg, env, strings.NewReader(""), io.Discard, false, ollamaSelection{Local: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Verified != 1 {
		t.Fatalf("verified = %d, want 1 (%v)", res.Verified, res.Failures)
	}
	if len(probed) != 1 || probed[0] != "qwen3.5:9b" {
		t.Fatalf("probed = %v, want exactly [qwen3.5:9b] through the resolved endpoint", probed)
	}
	if len(plan.LocalBound) != 1 {
		t.Fatalf("plan.LocalBound = %v", plan.LocalBound)
	}
	if !containsString(cfg.Inference.AllowedModels, "ollama/qwen3.5:9b") {
		t.Fatalf("roster = %v; a stamped provider must still widen for an EXPLICIT add",
			cfg.Inference.AllowedModels)
	}
	// The point of all of it: the model is now callable, which is what the
	// router reads.
	var found bool
	for _, b := range cfg.Inference.Models {
		if b.Model == "ollama/qwen3.5:9b" {
			found = true
			if !b.Verified || b.VerifiedBy != "probe" {
				t.Fatalf("binding is not probe-verified: %+v", b)
			}
			if !inference.Callable(cfg, b) {
				t.Fatalf("probed binding is not callable: %+v", b)
			}
		}
	}
	if !found {
		t.Fatal("no binding was written for the listed local model")
	}
}

// TestReconcileOllamaInference_RefusesUnderExclusivePack: a pack that owns
// inference would have every written binding dropped by the topology filter, so
// reporting success would be a success word with nothing behind it.
func TestReconcileOllamaInference_RefusesUnderExclusivePack(t *testing.T) {
	cfg := &config.Config{Inference: config.InferenceConfig{ExclusiveSource: "/packs/corp"}}
	_, _, err := reconcileOllamaInference(cfg, ollamaListEnv([]string{"qwen3.5:9b"}, "darwin", 32),
		strings.NewReader(""), io.Discard, false, ollamaSelection{Local: true})
	if err != errInferenceExclusive {
		t.Fatalf("err = %v, want errInferenceExclusive", err)
	}
	if len(cfg.Inference.Models) != 0 {
		t.Fatalf("refusal must write nothing, got %+v", cfg.Inference.Models)
	}
}

// TestRequireOllamaReady_NamesTheRightProblem: telling someone to install
// software they already have is its own kind of wrong, so a missing binary and
// a dead daemon must not share a message.
func TestRequireOllamaReady_NamesTheRightProblem(t *testing.T) {
	missing := shellEnv{lookPath: func(string) (string, error) { return "", errNotFoundFixture }}
	err := requireOllamaReady(missing)
	if err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("missing binary: err = %v, want an install message", err)
	}

	installed := ollamaListEnv(nil, "darwin", 32)
	installed.probe = func(string, ...string) (string, bool, error) { return "", true, nil } // timeout
	err = requireOllamaReady(installed)
	if err == nil || !strings.Contains(err.Error(), "daemon") {
		t.Fatalf("dead daemon: err = %v, want a daemon message", err)
	}

	if err := requireOllamaReady(ollamaListEnv([]string{"qwen3.5:9b"}, "darwin", 32)); err != nil {
		t.Fatalf("healthy ollama rejected: %v", err)
	}
}

// TestWidenRosterForProvider_OnlyTouchesTheNamedProvider: widening is scoped to
// what the user asked for. Sweeping in another provider's models would undo a
// deliberate narrowing the roster stamp exists to protect.
func TestWidenRosterForProvider_OnlyTouchesTheNamedProvider(t *testing.T) {
	cfg := &config.Config{Inference: config.InferenceConfig{
		AllowedModels: []string{"anthropic/claude-opus-5"},
		Backends: map[string]config.InferenceBackend{
			"ollama":    {Driver: "ollama", BaseURL: "http://127.0.0.1:11434/v1", Auth: "none"},
			"anthropic": {Driver: "native", Auth: "1password", KeyEnv: "ANTHROPIC_API_KEY"},
		},
		Models: []config.InferenceModelBinding{
			{Model: "ollama/qwen3.5:9b", Backend: "ollama", Upstream: "qwen3.5:9b", Available: true},
			{Model: "anthropic/claude-haiku-4-5", Backend: "anthropic", Upstream: "anthropic/claude-haiku-4-5", Available: true},
		},
	}}
	widenRosterForProvider(cfg, "ollama")
	if !containsString(cfg.Inference.AllowedModels, "ollama/qwen3.5:9b") {
		t.Fatalf("roster = %v, want the ollama model added", cfg.Inference.AllowedModels)
	}
	if containsString(cfg.Inference.AllowedModels, "anthropic/claude-haiku-4-5") {
		t.Fatalf("roster = %v; widening for ollama must not add anthropic models", cfg.Inference.AllowedModels)
	}

	// An empty roster already means "no restriction" — widening it would turn an
	// absence of policy into an explicit list that then freezes.
	open := &config.Config{Inference: config.InferenceConfig{Models: cfg.Inference.Models}}
	widenRosterForProvider(open, "ollama")
	if len(open.Inference.AllowedModels) != 0 {
		t.Fatalf("an unrestricted roster must stay unrestricted, got %v", open.Inference.AllowedModels)
	}
}

func toStr(v any) string {
	s, _ := v.(string)
	return s
}

// TestInferenceManifestCarriesOllamaModels is the HOST half of the "No models
// match pattern" fix. The sandbox is handed a --models cycle built from this
// manifest, and extensions/ollama-bridge.ts registers the provider from the
// same file. If the manifest omits ollama models the bridge cannot register
// them; if it spells their ids differently the cycle cannot match. So this
// pins the exact ids the two sides agree on.
func TestInferenceManifestCarriesOllamaModels(t *testing.T) {
	probed := func(model, tag string) config.InferenceModelBinding {
		return config.InferenceModelBinding{
			Model: model, Backend: "ollama", Upstream: tag,
			Available: true, Verified: true, VerifiedBy: "probe",
		}
	}
	cfg := &config.Config{Inference: config.InferenceConfig{
		Backends: map[string]config.InferenceBackend{
			"ollama": {Driver: "ollama", BaseURL: "http://127.0.0.1:11434/v1", Auth: "none"},
		},
		Models: []config.InferenceModelBinding{
			probed("ollama/glm-5.2:cloud", "glm-5.2:cloud"),
			probed("ollama/qwen3.5:9b", "qwen3.5:9b"),
		},
	}}
	_, manifest, err := compileInferenceRuntime(cfg, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, m := range manifest.Models {
		got[m.ID] = m.Backend
	}
	for _, want := range []string{"ollama/glm-5.2:cloud", "ollama/qwen3.5:9b"} {
		if got[want] != "ollama" {
			t.Errorf("manifest is missing %q under backend \"ollama\"; the bridge cannot register what it cannot see (got %v)", want, got)
		}
	}
	// The ids must be exactly what `pix run` puts in --models, or the cycle
	// warns on every one of them at session start.
	cycle, err := callableRuntimeModels(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range cycle {
		if _, ok := got[id]; !ok {
			t.Errorf("--models advertises %q but the manifest does not declare it", id)
		}
	}
}

// mustNoProbeErr fails the test on a verify error instead of letting it read as
// "nothing to verify". A nil probe seam used to return a clean-looking zero
// outcome, so a test that forgot to wire one asserted against silence and
// passed; that is the exact confusion errNoProbeSeam exists to end, and a
// helper that swallows it here would reintroduce it in the test layer.
func mustNoProbeErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// TestVerify_NilProbeSeamIsLoud pins the fix. Both verify functions used to
// return (0 attempted, 0 verified, no failures) when handed a shellEnv with no
// probe — a value a caller cannot tell apart from a clean pass, which is how
// `0 model(s) answered a live request` got printed next to exit code 0.
func TestVerify_NilProbeSeamIsLoud(t *testing.T) {
	cfg := ollamaCfgWith(binding("ollama/qwen3.5:9b"))
	if _, err := verifyOllamaInference(cfg, shellEnv{}, io.Discard); !errors.Is(err, errNoProbeSeam) {
		t.Fatalf("ollama verify with no probe seam: err = %v, want errNoProbeSeam", err)
	}
	if _, err := verifyDirectInference(&config.Config{}, shellEnv{}); !errors.Is(err, errNoProbeSeam) {
		t.Fatalf("direct verify with no probe seam: err = %v, want errNoProbeSeam", err)
	}
	// A seam that IS wired reports honestly, including the legitimate
	// nothing-to-do case, which must stay distinguishable from the above.
	empty, err := verifyDirectInference(&config.Config{}, shellEnv{directInferenceProbe: func(string, string, string) error { return nil }})
	if err != nil || empty.Attempted != 0 || empty.Verified != 0 {
		t.Fatalf("a wired seam with no bindings = %+v, %v; want a clean zero outcome and no error", empty, err)
	}
}
