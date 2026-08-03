package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"pix/host/hostenv"
	"pix/host/readiness"
	"pix/host/readiness/axis"
	"pix/host/workflow/doctor"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pix/host/config"
	"pix/host/inference"
	"pix/host/sys/systest"
)

// ollamaListEnv fakes a healthy daemon whose `ollama list` prints tags, plus a
// memory reading. BOTH platform seams are wired (sysctl and /proc/meminfo) so
// the fixture works whatever GOOS the test binary runs on; every RAM figure the
// tests use resolves to the same rung under the darwin AND linux fractions.
func ollamaListEnv(tags []string, _ string, totalGB float64) hostenv.Env {
	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(string) (string, error) { return "/usr/local/bin/ollama", nil }}}
	rows := "NAME ID SIZE MODIFIED\n"
	for _, tag := range tags {
		rows += tag + " abc 1GB - now\n"
	}
	systest.Of(env.System).RunTimedFn = func(name string, args ...string) (string, bool, error) {
		switch name {
		case "ollama":
			return rows, false, nil
		case "sysctl":
			return fmt.Sprintf("%d\n", int64(totalGB*axis.BytesPerGB)), false, nil
		}
		return "", false, fmt.Errorf("unexpected command %s", name)
	}
	systest.Of(env.System).ReadFileFn = func(path string) (string, error) {
		if path == "/proc/meminfo" {
			return fmt.Sprintf("MemTotal: %d kB\n", int64(totalGB*axis.BytesPerGB/1024)), nil
		}
		return "", fmt.Errorf("unexpected file %s", path)
	}
	return env
}

func ollamaCfgWith(bindings ...config.InferenceModelBinding) *config.Config {
	return &config.Config{Inference: config.InferenceConfig{
		Backends: map[string]config.InferenceBackend{
			"ollama": {Driver: "ollama", BaseURL: "http://127.0.0.1:11434/v1", Auth: "none"},
		},
		Models: bindings,
	}}
}

func binding(model string) config.InferenceModelBinding {
	return config.InferenceModelBinding{Model: model, Backend: "ollama", Upstream: axis.OllamaTagFor(model), Available: true}
}

// TestConfigureOllamaInferenceBindsUnverifiedCandidates: a listing is not
// evidence. Re-introducing `Verified: true` at the bind site fails here.
func TestConfigureOllamaInferenceBindsUnverifiedCandidates(t *testing.T) {
	cfg := &config.Config{}
	env := ollamaListEnv([]string{"qwen3.5:9b", "glm-5.2:cloud"}, "darwin", 32)
	plan, err := configureOllamaInference(cfg, env, ollamaSelection{Local: true, Cloud: true}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Inference.Models) == 0 {
		t.Fatal("nothing was bound")
	}
	for _, b := range cfg.Inference.Models {
		if b.Verified || b.VerifiedBy != "" {
			t.Fatalf("a listing must never produce a verified binding: %+v", b)
		}
		if !b.Available {
			t.Fatalf("a listed model is a candidate: %+v", b)
		}
	}
	if len(plan.LocalBound) != 1 || plan.LocalBound[0] != "ollama/qwen3.5:9b" {
		t.Fatalf("local bound = %v", plan.LocalBound)
	}
	if len(plan.CloudBound) != 1 {
		t.Fatalf("cloud bound = %v", plan.CloudBound)
	}
}

// TestOllamaLocalSelectionBindsNoCloudModel is the S4 narrowing: token 2 means
// Ollama LOCAL. The un-asked-for cloud binding was the delivery mechanism for
// the gated model that 401'd.
func TestOllamaLocalSelectionBindsNoCloudModel(t *testing.T) {
	cfg := &config.Config{}
	env := ollamaListEnv([]string{"qwen3.5:9b", "glm-5.2:cloud", "deepseek-v4-pro:cloud"}, "darwin", 32)
	if _, err := configureOllamaInference(cfg, env, ollamaSelection{Local: true}, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, b := range cfg.Inference.Models {
		if strings.Contains(b.Upstream, "cloud") {
			t.Fatalf("\"Ollama local\" bound a cloud model: %+v", b)
		}
	}
}

// TestOllamaLocalWithNoCatalogModelPulledSucceeds: resurrecting the hard error
// fails here. The rung is recorded so the models step's existing consent can
// offer it.
func TestOllamaLocalWithNoCatalogModelPulledSucceeds(t *testing.T) {
	cfg := &config.Config{}
	env := ollamaListEnv(nil, "darwin", 32)
	plan, err := configureOllamaInference(cfg, env, ollamaSelection{Local: true}, io.Discard)
	if err != nil {
		t.Fatalf("a local-only user who has pulled nothing must not be hard-failed: %v", err)
	}
	if plan.WantPull != "qwen3.5:9b" || cfg.OllamaBridgeModel != "qwen3.5:9b" {
		t.Fatalf("plan = %+v, bridge = %q; a 32GB Mac gets the 9b", plan, cfg.OllamaBridgeModel)
	}
	if len(cfg.Inference.Models) != 1 || cfg.Inference.Models[0].Verified {
		t.Fatalf("the rung must be bound as an unverified candidate: %+v", cfg.Inference.Models)
	}
}

// TestOllamaLocalSkipsRungsThisMachineCannotRun guards the offer filter.
func TestOllamaLocalSkipsRungsThisMachineCannotRun(t *testing.T) {
	cfg := &config.Config{}
	env := ollamaListEnv([]string{"qwen3.5:9b", "qwen3.5:35b"}, "darwin", 24)
	plan, err := configureOllamaInference(cfg, env, ollamaSelection{Local: true}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range cfg.Inference.Models {
		if b.Upstream == "qwen3.5:35b" {
			t.Fatal("a 24GB machine (16.1GB usable) was offered the 35b rung")
		}
	}
	if len(plan.SkippedRAM) == 0 {
		t.Fatal("the skipped rung must be reported so setup can explain the absence")
	}
}

// fakeProber records every probe call and can fail chosen tags.
type fakeProber struct {
	mu       sync.Mutex
	order    []string
	ctx      map[string]int
	fail     map[string]error
	delay    time.Duration
	inFlight int32
	maxLocal int32
	localSet map[string]bool
}

func (f *fakeProber) probe(endpoint, model string, numCtx int, timeout time.Duration) error {
	f.mu.Lock()
	f.order = append(f.order, model)
	if f.ctx == nil {
		f.ctx = map[string]int{}
	}
	f.ctx[model] = numCtx
	f.mu.Unlock()
	if f.localSet[model] {
		n := atomic.AddInt32(&f.inFlight, 1)
		for {
			cur := atomic.LoadInt32(&f.maxLocal)
			if n <= cur || atomic.CompareAndSwapInt32(&f.maxLocal, cur, n) {
				break
			}
		}
		defer atomic.AddInt32(&f.inFlight, -1)
	}
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	return f.fail[model]
}

// TestVerifyOllamaInferencePromotesOnlyAnsweringModels: one probe result must
// never be applied to a whole backend.
func TestVerifyOllamaInferencePromotesOnlyAnsweringModels(t *testing.T) {
	cfg := ollamaCfgWith(
		binding("ollama/qwen3.5:9b"),
		binding("ollama/glm-5.2:cloud"),
		binding("ollama/deepseek-v4-pro:cloud"),
	)
	f := &fakeProber{fail: map[string]error{"deepseek-v4-pro:cloud": fmt.Errorf("endpoint rejected the request (HTTP 401)")}}
	env := hostenv.Env{System: &systest.Fake{}, OllamaInference: f.probe}
	probe, probeErr := verifyOllamaInference(cfg, env, io.Discard)
	mustNoProbeErr(t, probeErr)
	attempted, verified, failures, notProbed := probe.Attempted, probe.Verified, probe.Failures, probe.NotProbed
	if attempted != 3 || verified != 2 || len(failures) != 1 || len(notProbed) != 0 {
		t.Fatalf("attempted=%d verified=%d failures=%v notProbed=%v", attempted, verified, failures, notProbed)
	}
	for _, b := range cfg.Inference.Models {
		want := b.Upstream != "deepseek-v4-pro:cloud"
		if b.Verified != want {
			t.Errorf("%s verified=%v, want %v", b.Model, b.Verified, want)
		}
	}
	if !strings.Contains(failures[0], "HTTP 401") {
		t.Fatalf("the failure must carry the refusal: %v", failures)
	}
}

// TestVerifiedBindingRecordsProbeProvenance: provenance is written with the
// claim and cleared with it, so it can never outlive what it describes.
func TestVerifiedBindingRecordsProbeProvenance(t *testing.T) {
	cfg := ollamaCfgWith(binding("ollama/qwen3.5:9b"))
	f := &fakeProber{}
	env := hostenv.Env{System: &systest.Fake{}, OllamaInference: f.probe}
	if probe, _ := verifyOllamaInference(cfg, env, io.Discard); probe.Verified != 1 {
		t.Fatal("probe succeeded but nothing was promoted")
	}
	got := cfg.Inference.Models[0]
	if got.VerifiedBy != config.VerifiedByProbe || got.VerifiedAt == "" {
		t.Fatalf("promotion must record provenance: %+v", got)
	}
	// Now demote: the same binding, a refusing prober.
	f2 := &fakeProber{fail: map[string]error{"qwen3.5:9b": fmt.Errorf("endpoint rejected the request (HTTP 500)")}}
	if probe, _ := verifyOllamaInference(cfg, hostenv.Env{System: &systest.Fake{}, OllamaInference: f2.probe}, io.Discard); probe.Verified != 0 {
		t.Fatal("a refused probe must not verify")
	}
	got = cfg.Inference.Models[0]
	if got.Verified || got.VerifiedBy != "" || got.VerifiedAt != "" {
		t.Fatalf("demotion must clear the claim AND its provenance: %+v", got)
	}
}

// TestNilOllamaProbeSeamLeavesBindingsUnverified guards the thing that must
// never happen with a missing prober: a test-mode default that fabricates
// success, or a real network call leaking out of a hermetic test.
//
// It used to also assert the RETURN was a clean zero. That assertion was the
// bug in miniature — it pinned "no probe configured" as indistinguishable from
// "probed nothing" — so the return contract is now an error, and what remains
// here is the part that was always right: nothing gets marked verified.
func TestNilOllamaProbeSeamLeavesBindingsUnverified(t *testing.T) {
	cfg := ollamaCfgWith(binding("ollama/qwen3.5:9b"))
	probe, err := verifyOllamaInference(cfg, hostenv.Env{System: &systest.Fake{}}, io.Discard)
	if err == nil {
		t.Fatal("a nil prober must be reported, not silently treated as nothing to do")
	}
	if probe.Attempted != 0 || probe.Verified != 0 {
		t.Fatalf("a nil prober must probe nothing: %+v", probe)
	}
	if cfg.Inference.Models[0].Verified {
		t.Fatal("a nil prober must never produce a verified binding")
	}
}

// TestVerifyOllamaInferenceUsesResolvedEndpoint pairs with
// check-endpoint-literals.sh: a hardcoded loopback fails here.
func TestVerifyOllamaInferenceUsesResolvedEndpoint(t *testing.T) {
	cfg := ollamaCfgWith(binding("ollama/qwen3.5:9b"))
	var seen string
	env := hostenv.Env{System: &systest.Fake{GetenvFn: func(name string) string {
		if name == "OLLAMA_HOST" {
			return "10.0.0.5:11500"
		}
		return ""
	}}, OllamaInference: func(endpoint, model string, numCtx int, timeout time.Duration) error {
		seen = endpoint
		return nil
	}}
	_, _ = verifyOllamaInference(cfg, env, io.Discard)
	if seen != "http://10.0.0.5:11500" {
		t.Fatalf("probe endpoint = %q, want the resolved OLLAMA_HOST", seen)
	}
}

// TestLocalOllamaProbesAreSerialized is review blocker B1: a shared errgroup
// across the whole binding set co-loads local weights, so a good model reports
// a timeout it never got a turn to spend and gets un-bound.
func TestLocalOllamaProbesAreSerialized(t *testing.T) {
	cfg := ollamaCfgWith(
		binding("ollama/qwen3.5:4b"),
		binding("ollama/qwen3.5:9b"),
		binding("ollama/qwen3.5:27b"),
		binding("ollama/glm-5.2:cloud"),
		binding("ollama/deepseek-v4-pro:cloud"),
	)
	f := &fakeProber{
		delay:    30 * time.Millisecond,
		localSet: map[string]bool{"qwen3.5:4b": true, "qwen3.5:9b": true, "qwen3.5:27b": true},
	}
	var cloudInFlight, maxCloud int32
	base := f.probe
	env := hostenv.Env{System: &systest.Fake{}, OllamaInference: func(endpoint, model string, numCtx int, timeout time.Duration) error {
		if strings.Contains(model, "cloud") {
			n := atomic.AddInt32(&cloudInFlight, 1)
			if n > atomic.LoadInt32(&maxCloud) {
				atomic.StoreInt32(&maxCloud, n)
			}
			defer atomic.AddInt32(&cloudInFlight, -1)
		}
		return base(endpoint, model, numCtx, timeout)
	}}
	_, _ = verifyOllamaInference(cfg, env, io.Discard)
	if got := atomic.LoadInt32(&f.maxLocal); got != 1 {
		t.Fatalf("max concurrent LOCAL probes = %d, want 1 (two resident models is a budget nobody computed)", got)
	}
	if got := atomic.LoadInt32(&maxCloud); got < 2 {
		t.Fatalf("max concurrent CLOUD probes = %d, want >= 2 (network round trips hold no local resource)", got)
	}
}

// TestLocalProbeOrderIsLargestRungFirst: spending the budget on the 4b and
// never reaching the rung the roster will actually use.
func TestLocalProbeOrderIsLargestRungFirst(t *testing.T) {
	cfg := ollamaCfgWith(
		binding("ollama/qwen3.5:4b"),
		binding("ollama/qwen3.5:27b"),
		binding("ollama/qwen3.5:9b"),
	)
	f := &fakeProber{}
	_, _ = verifyOllamaInference(cfg, hostenv.Env{System: &systest.Fake{}, OllamaInference: f.probe}, io.Discard)
	want := []string{"qwen3.5:27b", "qwen3.5:9b", "qwen3.5:4b"}
	if strings.Join(f.order, ",") != strings.Join(want, ",") {
		t.Fatalf("probe order = %v, want %v", f.order, want)
	}
	if f.ctx["qwen3.5:27b"] != 32768 || f.ctx["qwen3.5:9b"] != 16384 || f.ctx["qwen3.5:4b"] != 8192 {
		t.Fatalf("each rung must be probed at the context its gate priced: %v", f.ctx)
	}
}

// TestLocalProbeBudgetMarksRemainderNotProbedNotFailed is B1's second half: a
// healthy model reported as broken, and a decline turned into a non-zero exit
// by a budget.
func TestLocalProbeBudgetMarksRemainderNotProbedNotFailed(t *testing.T) {
	oldTimeout, oldBudget := ollamaLocalProbeTimeout, ollamaLocalProbeBudget
	ollamaLocalProbeTimeout, ollamaLocalProbeBudget = 50*time.Millisecond, 120*time.Millisecond
	defer func() { ollamaLocalProbeTimeout, ollamaLocalProbeBudget = oldTimeout, oldBudget }()

	cfg := ollamaCfgWith(
		binding("ollama/qwen3.5:35b"),
		binding("ollama/qwen3.5:27b"),
		binding("ollama/qwen3.5:9b"),
		binding("ollama/qwen3.5:4b"),
	)
	f := &fakeProber{delay: 70 * time.Millisecond}
	var out bytes.Buffer
	probe, probeErr := verifyOllamaInference(cfg, hostenv.Env{System: &systest.Fake{}, OllamaInference: f.probe}, &out)
	mustNoProbeErr(t, probeErr)
	attempted, verified, failures, notProbed := probe.Attempted, probe.Verified, probe.Failures, probe.NotProbed
	if len(notProbed) == 0 {
		t.Fatal("the budget must leave a remainder unprobed rather than running forever")
	}
	if attempted != verified {
		t.Fatalf("attempted=%d verified=%d: an unprobed candidate must not count as attempted", attempted, verified)
	}
	if attempted+len(notProbed) != 4 {
		t.Fatalf("every candidate is either probed or not probed: attempted=%d notProbed=%v", attempted, notProbed)
	}
	if len(failures) != 0 {
		t.Fatalf("a budget must never manufacture a failure: %v", failures)
	}
	if !strings.Contains(out.String(), "not probed") {
		t.Fatalf("the pause must be legible: %q", out.String())
	}
	if strings.Contains(out.String(), "failed") {
		t.Fatalf("an unreached candidate must not be rendered as a rejection: %q", out.String())
	}
	// The largest rung — the one the roster will use — is never the one skipped.
	for _, id := range notProbed {
		if id == "ollama/qwen3.5:35b" {
			t.Fatal("the budget ran out on the largest rung; the order is wrong")
		}
	}
}

// TestLocalProbeSendsKeepAliveZeroAndRungContext exercises the REAL probe body:
// dropping the unload stacks probe n+1 on probe n's resident weights.
func TestLocalProbeSendsKeepAliveZeroAndRungContext(t *testing.T) {
	var body map[string]any
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"response":"OK"}`))
	}))
	defer srv.Close()
	if err := inference.LiveOllamaInferenceProbe(srv.URL, "qwen3.5:9b", 16384, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if path != "/api/generate" {
		t.Fatalf("probe path = %q", path)
	}
	if v, ok := body["keep_alive"]; !ok || fmt.Sprint(v) != "0" {
		t.Fatalf("keep_alive = %v; without the unload, the next probe stacks on this one's weights", body["keep_alive"])
	}
	if body["stream"] != false || body["model"] != "qwen3.5:9b" {
		t.Fatalf("probe body = %v", body)
	}
	opts, _ := body["options"].(map[string]any)
	if fmt.Sprint(opts["num_ctx"]) != "16384" {
		t.Fatalf("num_ctx = %v, want the rung's declared context budget", opts["num_ctx"])
	}
	if _, ok := opts["num_ctx"]; ok {
		// A cloud probe carries no num_ctx: its context is not RAM-gated here.
		body = nil
		if err := inference.LiveOllamaInferenceProbe(srv.URL, "glm-5.2:cloud", 0, 5*time.Second); err != nil {
			t.Fatal(err)
		}
		cloudOpts, _ := body["options"].(map[string]any)
		if _, present := cloudOpts["num_ctx"]; present {
			t.Fatal("a cloud probe must not pin num_ctx")
		}
	}
}

// TestLiveOllamaProbeRejectsUnauthorized keeps the refusal text specific.
func TestLiveOllamaProbeRejectsUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()
	err := inference.LiveOllamaInferenceProbe(srv.URL, "kimi-k3:cloud", 0, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("error = %v, want an HTTP 401 refusal", err)
	}
}

// TestUnverifiedOllamaBindingIsNotCallable is the hole that made honest
// verification cosmetic: reverting bindingNeedsHostProof to the
// `Auth != "1password"` shortcut fails here in three places at once.
func TestUnverifiedOllamaBindingIsNotCallable(t *testing.T) {
	cfg := ollamaCfgWith(binding("ollama/qwen3.5:9b"))
	if inference.Callable(cfg, cfg.Inference.Models[0]) {
		t.Fatal("an unverified ollama binding must not be callable")
	}
	if ids, err := callableRuntimeModels(cfg); err != nil || len(ids) != 0 {
		t.Fatalf("callable runtime models = %v (%v)", ids, err)
	}
	_, manifest, err := compileInferenceRuntime(cfg, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Models) != 0 {
		t.Fatalf("an unproven binding reached the compiled manifest: %+v", manifest.Models)
	}
	cfg.Inference.Models[0].Verified = true
	if !inference.Callable(cfg, cfg.Inference.Models[0]) {
		t.Fatal("a probe-verified ollama binding must be callable")
	}
}

// TestPackDeclaredOllamaBindingStaysCallableWithoutHostProof: a pack's
// authority is the sandbox smoke test, which a host probe cannot replay.
func TestPackDeclaredOllamaBindingStaysCallableWithoutHostProof(t *testing.T) {
	cfg := &config.Config{Inference: config.InferenceConfig{
		Backends: map[string]config.InferenceBackend{
			"ollama": {Driver: "ollama", BaseURL: "http://host.docker.internal:11434/v1", Auth: "none", Source: "/packs/work"},
		},
		Models: []config.InferenceModelBinding{
			{Model: "ollama/qwen3.5:9b", Backend: "ollama", Upstream: "qwen3.5:9b", Available: true, Source: "/packs/work"},
		},
	}}
	if !inference.Callable(cfg, cfg.Inference.Models[0]) {
		t.Fatal("a pack-declared ollama binding must stay callable without a host probe")
	}
	// And a host probe must not even try to demote it.
	f := &fakeProber{}
	if probe, _ := verifyOllamaInference(cfg, hostenv.Env{System: &systest.Fake{}, OllamaInference: f.probe}, io.Discard); probe.Attempted != 0 {
		t.Fatalf("a pack binding was probed (%d attempts): %v", probe.Attempted, f.order)
	}
}

// TestNonInteractiveModelsFlagRejectsUnprobedModel (N1): the contract change is
// intended, but it must land with an error that names the probe, not the
// generic "not available through the selected runtime".
func TestNonInteractiveModelsFlagRejectsUnprobedModel(t *testing.T) {
	cfg := ollamaCfgWith(binding("ollama/qwen3.5:27b"), binding("ollama/qwen3.5:9b"))
	cfg.Inference.Models[1].Verified = true
	err := configureModelRoster(cfg, strings.NewReader(""), &bytes.Buffer{}, false, "ollama/qwen3.5:27b")
	if err == nil {
		t.Fatal("a scripted setup naming a model that cannot answer must fail loudly")
	}
	if !strings.Contains(err.Error(), "has not passed a probe") || !strings.Contains(err.Error(), axis.PullModelsFixCmd) {
		t.Fatalf("error = %v; it must name the reason and the fix", err)
	}
}

// TestSynthesizeInferenceKitErrorNamesTheFix (S5): a dead-end refusal.
func TestSynthesizeInferenceKitErrorNamesTheFix(t *testing.T) {
	cfg := ollamaCfgWith(binding("ollama/qwen3.5:9b"))
	_, err := synthesizeInferenceKit(cfg)
	if err == nil {
		t.Fatal("a config with no callable binding must refuse to build a kit")
	}
	if !strings.Contains(err.Error(), axis.PullModelsFixCmd) {
		t.Fatalf("refusal = %v; it must carry the remediation", err)
	}
}

// TestUnverifiedOllamaCandidateRemediatesWithPullNotProviderKey (S3): the
// fall-through to modelKeyCoreCheck told a pure-Ollama user to buy a key.
func TestUnverifiedOllamaCandidateRemediatesWithPullNotProviderKey(t *testing.T) {
	cfg := ollamaCfgWith(binding("ollama/qwen3.5:9b"))
	c := axis.InferenceCoreCheck(cfg, "", true)
	if c.Verdict != readiness.VerdictTodo || c.Todo != axis.PullModelsFixCmd {
		t.Fatalf("core check = %+v, want a todo remediated by %q", c, axis.PullModelsFixCmd)
	}
	if strings.Contains(c.Todo+c.Detail+c.Evidence, "ANTHROPIC_API_KEY") {
		t.Fatalf("a not-pulled model must never be remediated with a cloud key: %+v", c)
	}
	// With no ollama candidates at all, the key fix is still correct.
	empty := &config.Config{}
	if got := axis.InferenceCoreCheck(empty, "", true); got.Todo != axis.ModelKeyFixCmd {
		t.Fatalf("a host with no ollama candidates still needs a key: %+v", got)
	}
}

// TestRunIntentRowNamesThePullForUnverifiedOllamaBinding is S3's second caller.
func TestRunIntentRowNamesThePullForUnverifiedOllamaBinding(t *testing.T) {
	cfg := ollamaCfgWith(binding("ollama/qwen3.5:9b"))
	cfg.RunIntent = "breadth"
	model, err := axis.ResolveSessionModel("breadth")
	if err != nil {
		t.Skipf("breadth does not resolve here: %v", err)
	}
	cfg.Inference.Models[0].Model = model
	cfg.Inference.Models[0].Upstream = axis.OllamaTagFor(model)
	c := axis.RunIntentKeyCheck(cfg, "", true)
	if c.Todo != axis.PullModelsFixCmd {
		t.Fatalf("run_intent row = %+v, want the pull remediation", c)
	}
}

// TestLegacyVerifiedOllamaBindingFlaggedOnceThenClears (B2): a row that fires
// forever, or never.
func TestLegacyVerifiedOllamaBindingFlaggedOnceThenClears(t *testing.T) {
	cfg := ollamaCfgWith(binding("ollama/qwen3.5:9b"))
	cfg.Inference.Models[0].Verified = true // pre-upgrade: claimed from a listing
	if got := doctor.LegacyVerifiedOllamaBindings(cfg); len(got) != 1 {
		t.Fatalf("a listing-derived claim must be flagged: %v", got)
	}
	if !inference.Callable(cfg, cfg.Inference.Models[0]) {
		t.Fatal("legacy bindings are grandfathered as callable, not demoted at load")
	}
	// Promote path: the next setup re-probes and earns it.
	promoted := ollamaCfgWith(binding("ollama/qwen3.5:9b"))
	promoted.Inference.Models[0].Verified = true
	f := &fakeProber{}
	_, _ = verifyOllamaInference(promoted, hostenv.Env{System: &systest.Fake{}, OllamaInference: f.probe}, io.Discard)
	if got := doctor.LegacyVerifiedOllamaBindings(promoted); len(got) != 0 {
		t.Fatalf("the row must clear once a probe earns the claim: %v", got)
	}
	// Demote path: the probe refuses. The row clears there too, replaced by the
	// candidate row that names the real problem.
	demoted := ollamaCfgWith(binding("ollama/qwen3.5:9b"))
	demoted.Inference.Models[0].Verified = true
	f2 := &fakeProber{fail: map[string]error{"qwen3.5:9b": fmt.Errorf("endpoint rejected the request (HTTP 500)")}}
	_, _ = verifyOllamaInference(demoted, hostenv.Env{System: &systest.Fake{}, OllamaInference: f2.probe}, io.Discard)
	if got := doctor.LegacyVerifiedOllamaBindings(demoted); len(got) != 0 {
		t.Fatalf("the row must clear on demotion too: %v", got)
	}
	if len(axis.UnverifiedOllamaCandidates(demoted)) != 1 {
		t.Fatal("a demoted binding becomes the candidate row that names the real problem")
	}
}

// TestInferenceStepPrintsNothingOnFullSuccess (N2/AC-P0-302): a mutation must
// not render a success claim; that is the post-mutation probe's job.
func TestInferenceStepPrintsNothingOnFullSuccess(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg := ollamaCfgWith(binding("ollama/qwen3.5:9b"))
	f := &fakeProber{}
	var out bytes.Buffer
	if err := runSetupInferenceStep(cfg, hostenv.Env{System: &systest.Fake{}, OllamaInference: f.probe}, strings.NewReader(""), &out, false, setupModelsOutcome{consent: "none"}); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if strings.Contains(s, "verified;") || strings.Contains(s, "✓") || strings.Contains(s, "Core ready") {
		t.Fatalf("the mutation printed a success claim:\n%s", s)
	}
	if !cfg.Inference.Models[0].Verified {
		t.Fatal("the step must still promote what answered")
	}
}

// TestDeclinedPullLeavesNoCallableModelAndExitsZero: declining a multi-gigabyte
// download is a decision, not a failure.
func TestDeclinedPullLeavesNoCallableModelAndExitsZero(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg := ollamaCfgWith(binding("ollama/qwen3.5:9b"))
	cfg.OllamaBridgeModel = "qwen3.5:9b"
	var out bytes.Buffer
	// The prober is wired and REFUSES: nothing was pulled, so the probe cannot
	// succeed. That is the honest fixture. Leaving the seam nil to mean the same
	// thing is what this change deletes — "no prober" and "the prober said no"
	// are different facts, and only the second is what a declined pull produces.
	refuses := &fakeProber{fail: map[string]error{"qwen3.5:9b": fmt.Errorf("model not found")}}
	err := runSetupInferenceStep(cfg, hostenv.Env{System: &systest.Fake{}, OllamaInference: refuses.probe}, strings.NewReader(""), &out, false, setupModelsOutcome{consent: "prompt-no"})
	if err != nil {
		t.Fatalf("a declined pull must not fail setup: %v", err)
	}
	if !strings.Contains(out.String(), axis.PullModelsFixCmd) {
		t.Fatalf("the honest note must name the fix:\n%s", out.String())
	}
	if callable, _ := axis.ConfiguredInferenceSummary(cfg); callable != 0 {
		t.Fatal("nothing may be callable after a declined pull")
	}
}

// TestOllamaCloudSelectedWithZeroVerifiedFailsSetup: a silent "configured" for
// an account that can call nothing.
func TestOllamaCloudSelectedWithZeroVerifiedFailsSetup(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg := ollamaCfgWith(binding("ollama/glm-5.2:cloud"), binding("ollama/deepseek-v4-pro:cloud"))
	f := &fakeProber{fail: map[string]error{
		"glm-5.2:cloud":         fmt.Errorf("endpoint rejected the request (HTTP 401)"),
		"deepseek-v4-pro:cloud": fmt.Errorf("endpoint rejected the request (HTTP 401)"),
	}}
	var out bytes.Buffer
	err := runSetupInferenceStep(cfg, hostenv.Env{System: &systest.Fake{}, OllamaInference: f.probe}, strings.NewReader(""), &out, false, setupModelsOutcome{consent: "none"})
	if err == nil {
		t.Fatal("cloud selected with zero verified models must fail setup")
	}
	if !strings.Contains(err.Error(), "ollama signin") || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("error = %v; it must name the refusal and the command the USER runs", err)
	}
}

// TestPullPromptNamesTheBridgeAsRequiredOnPureLocalBox (N3): the static
// "optional" header surviving the change.
func TestPullPromptNamesTheBridgeAsRequiredOnPureLocalBox(t *testing.T) {
	cfg := ollamaCfgWith(binding("ollama/qwen3.5:9b"))
	cfg.OllamaBridgeModel = "qwen3.5:9b"
	cfg.MemoryWatcherModel = "qwen3.5:9b"
	cfg.MemoryEmbedModel = "nomic-embed-text"
	w := &ollamaWorld{}
	env := modelsSetupEnv(t, w)
	var out bytes.Buffer
	o := setupLocalModels(cfg, env, strings.NewReader("n\n"), &out, true, false)
	if o.consent != "prompt-no" {
		t.Fatalf("consent = %q, want prompt-no", o.consent)
	}
	s := out.String()
	header := s[:strings.Index(s, "\n[y/N]")+1]
	if idx := strings.Index(s, "Missing local Ollama models"); idx >= 0 {
		header = s[idx:]
		header = header[:strings.Index(header, "\n")]
	}
	if strings.Contains(header, "optional") {
		t.Fatalf("the header must not call the only callable model optional: %q", header)
	}
	if !strings.Contains(s, "REQUIRED") {
		t.Fatalf("the bridge rung is the only model Pix can call here; say so:\n%s", s)
	}
}

// TestPackOnePasswordBindingStillNeedsHostProof is the counterpart to the
// pack-ollama exemption above, and the regression test for scoping it.
//
// Packs may legally declare an `auth = "1password"` native backend, and
// verifyDirectInference already probes those with no Source check and demotes
// the ones that fail. An exemption keyed on `Source != ""` alone therefore let
// a binding whose probe was DISPATCHED AND REFUSED stay callable, flowing on
// into the compiled manifest, the sandbox kit, and doctor's "N callable
// model(s)" — a success word behind a failed probe. The exemption belongs to
// the auth Pix cannot verify from the host, not to packs as a class.
func TestPackOnePasswordBindingStillNeedsHostProof(t *testing.T) {
	cfg := &config.Config{Inference: config.InferenceConfig{
		Backends: map[string]config.InferenceBackend{
			"anthropic": {Driver: "native", Auth: "1password", KeyEnv: "ANTHROPIC_API_KEY", Source: "/packs/work"},
		},
		Models: []config.InferenceModelBinding{
			{Model: "anthropic/claude-sonnet-5", Backend: "anthropic", Upstream: "anthropic/claude-sonnet-5", Available: true, Source: "/packs/work"},
		},
	}}
	if inference.Callable(cfg, cfg.Inference.Models[0]) {
		t.Fatal("an unverified pack 1password binding must NOT be callable: the host can probe that auth, so it must")
	}
	// Earning it the honest way makes it callable, so the rule gates on proof,
	// not on origin.
	cfg.Inference.Models[0].Verified = true
	if !inference.Callable(cfg, cfg.Inference.Models[0]) {
		t.Fatal("a probe-verified pack 1password binding must be callable")
	}
}

// TestEmptyOllamaSelectionPersistsNothing is the regression test for the dead
// end that deleting the old hard error opened up.
//
// configureOllamaInference writes the ollama backend before it knows whether
// anything will bind. With the hard error gone it returned nil with an empty
// plan, so setup's keys step reached cfg.Save() and persisted a backend with no
// models — and the NEXT `pix setup` early-returns into
// enableDeclaredInferenceBindings ("configured but declares no models"), which
// is fatal. Setup was then bricked until `pix state reset`, with config.toml
// hand-editing forbidden by design.
//
// Reachable two ways, both covered here: choosing Ollama Cloud while signed out
// (no :cloud rows in the listing), and choosing local on a machine under the
// 24 GB floor.
func TestEmptyOllamaSelectionPersistsNothing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sel      ollamaSelection
		listing  string
		totalGB  float64
		wantWord string
	}{
		{
			name:     "cloud selected while signed out",
			sel:      ollamaSelection{Cloud: true},
			listing:  "NAME ID SIZE MODIFIED\n",
			totalGB:  64,
			wantWord: "ollama signin",
		},
		{
			name:     "local selected on a machine under the floor",
			sel:      ollamaSelection{Local: true},
			listing:  "NAME ID SIZE MODIFIED\n",
			totalGB:  16,
			wantWord: "below the 24 GB",
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// runtime.GOOS, not a fixed OS: axis.ProbeHostMemory dispatches on the real
			// one, and the 24 GB floor is a TOTAL-RAM rule, so it fires identically
			// whichever usable-fraction applies.
			env := hwMemEnv(t, runtime.GOOS, tc.totalGB)
			base := systest.Of(env.System).RunFn
			systest.Of(env.System).RunFn = func(name string, args ...string) (string, error) {
				if name == "ollama" && len(args) == 1 && args[0] == "list" {
					return tc.listing, nil
				}
				if base != nil {
					return base(name, args...)
				}
				return "", nil
			}
			cfg := &config.Config{}
			_, err := configureOllamaInference(cfg, env, tc.sel, io.Discard)
			if err == nil {
				t.Fatal("an Ollama selection that binds nothing must fail, not persist an empty backend")
			}
			if !strings.Contains(err.Error(), tc.wantWord) {
				t.Errorf("error must name what would change the answer (%q), got: %v", tc.wantWord, err)
			}
			// The whole point: nothing was left behind for the next run to choke on.
			if _, ok := cfg.Inference.Backends["ollama"]; ok {
				t.Error("the ollama backend was persisted despite binding nothing; the next `pix setup` would hard-fail")
			}
			if len(cfg.Inference.Models) != 0 {
				t.Errorf("bindings were persisted despite the failure: %+v", cfg.Inference.Models)
			}
			// And prove the dead end really was a dead end: a config in the state we
			// just refused to write is exactly what bricks the next run.
			bricked := &config.Config{Inference: config.InferenceConfig{
				Backends: map[string]config.InferenceBackend{"ollama": {Driver: "ollama", BaseURL: "http://x/v1", Auth: "none"}},
			}}
			if err := enableDeclaredInferenceBindings(bricked); err == nil {
				t.Fatal("expected the empty-backend config to be the fatal state; if this stops being true, revisit the rollback")
			}
		})
	}
}
