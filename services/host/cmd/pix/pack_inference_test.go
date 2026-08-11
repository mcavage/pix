package main

import (
	"io"
	"os"
	"path/filepath"
	"pix/host/packinfo"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/inference"
	"pix/host/sys/systest"
	"pix/host/workflow/launch"
	"pix/host/workflow/pack"
)

// TestPackInferenceValidationIsGenericAndFailClosed: a sbx-session backend has
// no host-reachable endpoint to probe, so Available is asserted the moment the
// (already Tier-1-gated) projection runs — that IS what "structurally
// injectable" means, not a health observation. Verified stays false: no probe
// ran, and none ever will for this auth mode.
func TestPackInferenceValidationIsGenericAndFailClosed(t *testing.T) {
	root := t.TempDir()
	m := packinfo.Manifest{Name: "team", Schema: 1, Inference: &packinfo.Inference{
		Exclusive: true, RequiredBackend: "gateway",
		Backends: map[string]packinfo.InferenceBack{"gateway": {Driver: "openai-compatible", Auth: "sbx-session", BaseURL: "https://models.example.test/v1", CredentialService: "sbx-login", KeyEnv: "SESSION_TOKEN"}},
		Models:   []packinfo.InferenceModel{{Model: "openai/gpt-5.6-sol", Backend: "gateway", Upstream: "alpha-prod"}},
	}}
	if err := pack.WriteManifest(root, m); err != nil {
		t.Fatal(err)
	}
	p, err := packinfo.LoadPack(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	if err := pack.ApplyPackInference(cfg, p.Manifest.Inference, root); err != nil {
		t.Fatal(err)
	}
	if cfg.Inference.ExclusiveSource != root || cfg.Inference.Backends["gateway"].Auth != "sbx-session" {
		t.Fatalf("inference projection = %+v", cfg.Inference)
	}
	if len(cfg.Inference.Models) != 1 || !cfg.Inference.Models[0].Available || cfg.Inference.Models[0].Verified {
		t.Fatalf("sbx-session pack binding must be available-but-unverified: %+v", cfg.Inference.Models)
	}
	if !inference.Callable(cfg, cfg.Inference.Models[0]) {
		t.Fatalf("an accepted sbx-session binding must be callable: %+v", cfg.Inference.Models[0])
	}
}

// TestPackInferenceReapplyPreservesOnlyMatchingEvidence covers the ONE auth
// mode where Available really is probe evidence: 1Password. sbx-session has no
// evidence to preserve or drop — it is recomputed structurally on every apply
// (see TestPackInferenceSbxSessionAcceptedPackYieldsCallableBindingAndRoute).
func TestPackInferenceReapplyPreservesOnlyMatchingEvidence(t *testing.T) {
	source := "/packs/work"
	inf := &packinfo.Inference{
		Backends: map[string]packinfo.InferenceBack{"gateway": {Driver: "native", Auth: "1password", KeyEnv: "GATEWAY_KEY"}},
		Models:   []packinfo.InferenceModel{{Model: "openai/gpt-5.6-sol", Backend: "gateway", Upstream: "prod"}},
	}
	cfg := &config.Config{}
	if err := pack.ApplyPackInference(cfg, inf, source); err != nil {
		t.Fatal(err)
	}
	if cfg.Inference.Models[0].Available {
		t.Fatal("a 1password pack binding must begin unavailable pending an actual probe")
	}
	// Simulate a probe earning the evidence.
	cfg.Inference.Models[0].Available, cfg.Inference.Models[0].Verified = true, true
	if err := pack.ApplyPackInference(cfg, inf, source); err != nil {
		t.Fatal(err)
	}
	if !cfg.Inference.Models[0].Available || !cfg.Inference.Models[0].Verified {
		t.Fatal("unchanged pack reapply erased probe evidence")
	}
	changed := *inf
	changed.Backends = map[string]packinfo.InferenceBack{"gateway": {Driver: "native", Auth: "1password", KeyEnv: "GATEWAY_KEY_V2"}}
	if err := pack.ApplyPackInference(cfg, &changed, source); err != nil {
		t.Fatal(err)
	}
	if cfg.Inference.Models[0].Available || cfg.Inference.Models[0].Verified {
		t.Fatal("changed backend retained stale probe evidence")
	}
}

// TestPackInferenceSbxSessionAcceptedPackYieldsCallableBindingAndRoute is the
// regression for the reported bug: an accepted sbx-session pack binding used to
// stay Available:false forever (needsHostProof's exemption was never reached
// because Callable rejects on !Available first), so `pix models ls` showed 0
// callable models and `pix run` refused to launch. It must now be callable AND
// survive into the compiled runtime route.
func TestPackInferenceSbxSessionAcceptedPackYieldsCallableBindingAndRoute(t *testing.T) {
	source := "/packs/team"
	inf := &packinfo.Inference{
		Backends: map[string]packinfo.InferenceBack{"gateway": {
			Driver: "openai-compatible", Protocol: "openai-responses", Auth: "sbx-session", BaseURL: "https://models.example.test/v1",
			CredentialService: "sbx-login", KeyEnv: "SESSION_TOKEN", CredentialHeader: "Authorization", CredentialFormat: "Bearer %s",
		}},
		Models: []packinfo.InferenceModel{{Model: "openai/gpt-5.6-sol", Backend: "gateway", Upstream: "reasoner"}},
	}
	cfg := &config.Config{}
	if err := pack.ApplyPackInference(cfg, inf, source); err != nil {
		t.Fatal(err)
	}
	binding := cfg.Inference.Models[0]
	if !binding.Available || binding.Verified {
		t.Fatalf("binding = %+v, want available-but-unverified", binding)
	}
	if !inference.Callable(cfg, binding) {
		t.Fatalf("binding must be callable: %+v", binding)
	}
	bound := inference.Bindings(cfg)
	if len(bound) != 1 || bound[0].Model != binding.Model {
		t.Fatalf("inference.Bindings = %+v, want the sbx-session binding", bound)
	}
	ids, err := inference.CallableRuntimeModels(cfg)
	if err != nil {
		t.Fatal(err)
	}
	wantID := inference.RuntimeID(bound[0])
	found := false
	for _, id := range ids {
		if id == wantID {
			found = true
		}
	}
	if !found {
		t.Fatalf("CallableRuntimeModels = %v, want %q in the create-time route", ids, wantID)
	}
}

// TestPackInference1PasswordBindingRemainsUnavailableUntilProbe: unlike
// sbx-session, a pack's 1Password backend IS a host-reachable endpoint Pix can
// dispatch a probe against, so trust acceptance alone must never make it
// callable — only a real, later-run probe (never exercised here) may.
func TestPackInference1PasswordBindingRemainsUnavailableUntilProbe(t *testing.T) {
	source := "/packs/direct"
	inf := &packinfo.Inference{
		Backends: map[string]packinfo.InferenceBack{"direct": {Driver: "native", Auth: "1password", KeyEnv: "GATEWAY_KEY"}},
		Models:   []packinfo.InferenceModel{{Model: "anthropic/claude-sonnet-5", Backend: "direct", Upstream: "anthropic/claude-sonnet-5"}},
	}
	cfg := &config.Config{}
	if err := pack.ApplyPackInference(cfg, inf, source); err != nil {
		t.Fatal(err)
	}
	binding := cfg.Inference.Models[0]
	if binding.Available || binding.Verified {
		t.Fatalf("binding = %+v, want unavailable and unverified pending a probe", binding)
	}
	if inference.Callable(cfg, binding) {
		t.Fatalf("an unprobed 1password pack binding must not be callable: %+v", binding)
	}
}

func TestPackInferenceCannotReplaceBackendFromAnotherSource(t *testing.T) {
	cfg := &config.Config{Inference: config.InferenceConfig{Backends: map[string]config.InferenceBackend{
		"openai": {Driver: "native", Auth: "1password"},
	}}}
	inf := &packinfo.Inference{Backends: map[string]packinfo.InferenceBack{
		"openai": {Driver: "openai-compatible", Auth: "none", BaseURL: "https://models.example.test/v1"},
	}}
	if err := pack.ApplyPackInference(cfg, inf, "/packs/untrusted"); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("collision error = %v", err)
	}
	if got := cfg.Inference.Backends["openai"]; got.Driver != "native" || got.Auth != "1password" {
		t.Fatalf("direct backend was mutated: %+v", got)
	}
}

func TestPackInferenceCredentialRoutingIsTrustGatedAndValidated(t *testing.T) {
	root := t.TempDir()
	manifest := packinfo.Manifest{Name: "team", Schema: 1, Inference: &packinfo.Inference{
		Backends: map[string]packinfo.InferenceBack{"gateway": {
			Driver: "openai-compatible", Auth: "sbx-session", BaseURL: "https://models.example.test/v1",
			CredentialService: "sbx-login", KeyEnv: "SESSION_TOKEN", CredentialHeader: "Authorization", CredentialFormat: "Bearer %s",
		}},
		Models: []packinfo.InferenceModel{{Model: "openai/gpt-5.6-sol", Backend: "gateway", Upstream: "reasoner"}},
	}}
	if err := pack.WriteManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	p, err := packinfo.LoadPack(root)
	if err != nil {
		t.Fatal(err)
	}
	bom := pack.ComputeHostBoM(p)
	if !bom.Tier1() || len(bom.Inference) != 1 {
		t.Fatalf("inference credential routing must be trust-gated: %+v", bom)
	}
	manifest.Inference.Backends["gateway"] = packinfo.InferenceBack{
		Driver: "openai-compatible", Auth: "sbx-session", BaseURL: "http://models.example.test/v1",
		CredentialService: "sbx-login", KeyEnv: "SESSION_TOKEN",
	}
	if err := pack.WriteManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := packinfo.LoadPack(root); err == nil || !strings.Contains(err.Error(), "must use https") {
		t.Fatalf("unsafe endpoint error = %v", err)
	}
}

func TestPackInferenceCredentialRoutingIsReverifiedAtLaunch(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	root := filepath.Join(dir, "pack")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := packinfo.Manifest{Name: "team", Schema: 1, Inference: &packinfo.Inference{
		Backends: map[string]packinfo.InferenceBack{"gateway": {
			Driver: "openai-compatible", Auth: "sbx-session", BaseURL: "https://models.example.test/v1",
			CredentialService: "sbx-login", KeyEnv: "DOCKER_TOKEN", CredentialHeader: "Authorization", CredentialFormat: "Bearer %s",
		}},
		Models: []packinfo.InferenceModel{{Model: "openai/gpt-5.6-sol", Backend: "gateway", Upstream: "reasoner"}},
	}}
	if err := pack.WriteManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	p, err := packinfo.LoadPack(root)
	if err != nil {
		t.Fatal(err)
	}
	bom := pack.ComputeHostBoM(p)
	fp, _, err := pack.ComputeHostExecFingerprint(root, bom)
	if err != nil {
		t.Fatal(err)
	}
	store := &pack.PackTrustStore{Version: 1}
	store.RecordAcceptance(store.TrustKey(root), pack.PackTrustRecord{Path: packinfo.CanonicalizePackRoot(root), Fingerprint: fp})
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Inference: config.InferenceConfig{Backends: map[string]config.InferenceBackend{}}}
	if _, err := packApplyForTest(cfg, &launch.RunOpts{Pack: root}, hostenv.Env{System: &systest.Fake{}}, io.Discard); err != nil {
		t.Fatalf("accepted inference launch rejected: %v", err)
	}
	// An accepted sbx-session binding is callable right away — THIS is the fix
	// under test — but the mutation guard below must still catch a pack that
	// changes its endpoint out from under a stale trust acceptance; Available
	// being structural must never be read as "mutation-proof."
	if len(cfg.Inference.Models) != 1 || !cfg.Inference.Models[0].Available || cfg.Inference.Models[0].Verified {
		t.Fatalf("accepted sbx-session binding = %+v, want available-but-unverified", cfg.Inference.Models)
	}
	if !inference.Callable(cfg, cfg.Inference.Models[0]) {
		t.Fatalf("accepted sbx-session binding must be callable: %+v", cfg.Inference.Models[0])
	}
	acceptedBackend := cfg.Inference.Backends["gateway"]

	backend := manifest.Inference.Backends["gateway"]
	backend.BaseURL = "https://attacker.example.test/v1"
	manifest.Inference.Backends["gateway"] = backend
	if err := pack.WriteManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := packApplyForTest(cfg, &launch.RunOpts{Pack: root}, hostenv.Env{System: &systest.Fake{}}, io.Discard); err == nil || !strings.Contains(err.Error(), "changed since acceptance") {
		t.Fatalf("mutated credential endpoint was not rejected: %v", err)
	}
	// The rejected re-gate must never have reached ApplyPackInference: the
	// config still carries the ORIGINALLY accepted backend, not the attacker's.
	if got := cfg.Inference.Backends["gateway"]; got != acceptedBackend {
		t.Fatalf("rejected mutation leaked into config: %+v, want %+v", got, acceptedBackend)
	}
}

func TestPackInferenceRejectsModelOutsideCatalog(t *testing.T) {
	root := t.TempDir()
	m := packinfo.Manifest{Name: "team", Schema: 1, Inference: &packinfo.Inference{
		Backends: map[string]packinfo.InferenceBack{"gateway": {Driver: "openai-compatible", Auth: "none", BaseURL: "http://127.0.0.1:9000/v1"}},
		Models:   []packinfo.InferenceModel{{Model: "private/unknown", Backend: "gateway", Upstream: "unknown"}},
	}}
	if err := pack.WriteManifest(root, m); err != nil {
		t.Fatal(err)
	}
	if _, err := packinfo.LoadPack(root); err == nil || !strings.Contains(err.Error(), "not in the Pix model catalog") {
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
	if err := os.WriteFile(filepath.Join(root, packinfo.PackManifestName), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := packinfo.LoadPack(root)
	if err == nil || !strings.Contains(err.Error(), "not declared") {
		t.Fatalf("error = %v", err)
	}
}

func TestPersistPackStackComposesInferenceInOrder(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	first, second := t.TempDir(), t.TempDir()
	write := func(root, name, backend, model string, exclusive bool) {
		t.Helper()
		m := packinfo.Manifest{Name: name, Schema: 1, Inference: &packinfo.Inference{
			Exclusive: exclusive, RequiredBackend: backend,
			Backends: map[string]packinfo.InferenceBack{backend: {Driver: "openai-compatible", Auth: "none", BaseURL: "http://127.0.0.1:9000/v1"}},
			Models:   []packinfo.InferenceModel{{Model: model, Backend: backend, Upstream: name + "-model"}},
		}}
		if err := pack.WriteManifest(root, m); err != nil {
			t.Fatal(err)
		}
	}
	write(first, "one", "first", "anthropic/claude-sonnet-5", false)
	write(second, "two", "second", "openai/gpt-5.6-sol", true)
	if err := pack.PersistPackStack([]string{first, second}); err != nil {
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

func TestPersistPackStackLaterNonExclusiveClearsExclusivity(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	first, second := t.TempDir(), t.TempDir()
	write := func(root, name, backend string, exclusive bool) {
		t.Helper()
		m := packinfo.Manifest{Name: name, Schema: 1, Inference: &packinfo.Inference{
			Exclusive: exclusive,
			Backends:  map[string]packinfo.InferenceBack{backend: {Driver: "openai-compatible", Auth: "none", BaseURL: "http://127.0.0.1:9000/v1"}},
			Models:    []packinfo.InferenceModel{{Model: "openai/gpt-5.6-sol", Backend: backend, Upstream: name + "-model"}},
		}}
		if err := pack.WriteManifest(root, m); err != nil {
			t.Fatal(err)
		}
	}
	write(first, "exclusive", "first", true)
	write(second, "additive", "second", false)
	if err := pack.PersistPackStack([]string{first, second}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Inference.ExclusiveSource != "" {
		t.Fatalf("later non-exclusive pack did not clear exclusivity: %q", cfg.Inference.ExclusiveSource)
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
	pack.ClearPackInference(cfg, "")
	if len(cfg.Inference.Backends) != 1 || cfg.Inference.Backends["personal"].Driver != "native" || len(cfg.Inference.Models) != 1 {
		t.Fatalf("user inference was not preserved: %+v", cfg.Inference)
	}
	if cfg.Inference.ExclusiveSource != "" {
		t.Fatalf("stale exclusivity survived: %q", cfg.Inference.ExclusiveSource)
	}
}
