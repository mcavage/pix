package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/sys/systest"
	"pix/host/workflow/launch"
	"pix/host/workflow/pack"
)

func TestPackInferenceValidationIsGenericAndFailClosed(t *testing.T) {
	root := t.TempDir()
	m := pack.Manifest{Name: "team", Schema: 1, Inference: &pack.Inference{
		Exclusive: true, RequiredBackend: "gateway",
		Backends: map[string]pack.InferenceBack{"gateway": {Driver: "openai-compatible", Auth: "sbx-session", BaseURL: "https://models.example.test/v1", CredentialService: "sbx-login", KeyEnv: "SESSION_TOKEN"}},
		Models:   []pack.InferenceModel{{Model: "openai/gpt-5.6-sol", Backend: "gateway", Upstream: "alpha-prod"}},
	}}
	if err := pack.WriteManifest(root, m); err != nil {
		t.Fatal(err)
	}
	p, err := pack.LoadPack(root)
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
	if len(cfg.Inference.Models) != 1 || cfg.Inference.Models[0].Available {
		t.Fatalf("pack binding must begin unverified: %+v", cfg.Inference.Models)
	}
}

func TestPackInferenceReapplyPreservesOnlyMatchingEvidence(t *testing.T) {
	source := "/packs/work"
	inf := &pack.Inference{
		Backends: map[string]pack.InferenceBack{"gateway": {Driver: "openai-compatible", Protocol: "openai-responses", Auth: "sbx-session", BaseURL: "https://models.example.test/v1"}},
		Models:   []pack.InferenceModel{{Model: "openai/gpt-5.6-sol", Backend: "gateway", Upstream: "prod"}},
	}
	cfg := &config.Config{}
	if err := pack.ApplyPackInference(cfg, inf, source); err != nil {
		t.Fatal(err)
	}
	cfg.Inference.Models[0].Available = true
	if err := pack.ApplyPackInference(cfg, inf, source); err != nil {
		t.Fatal(err)
	}
	if !cfg.Inference.Models[0].Available {
		t.Fatal("unchanged pack reapply erased availability evidence")
	}
	changed := *inf
	changed.Backends = map[string]pack.InferenceBack{"gateway": {Driver: "openai-compatible", Protocol: "openai-responses", Auth: "sbx-session", BaseURL: "https://new.example.test/v1"}}
	if err := pack.ApplyPackInference(cfg, &changed, source); err != nil {
		t.Fatal(err)
	}
	if cfg.Inference.Models[0].Available {
		t.Fatal("changed backend retained stale availability evidence")
	}
}

func TestPackInferenceCannotReplaceBackendFromAnotherSource(t *testing.T) {
	cfg := &config.Config{Inference: config.InferenceConfig{Backends: map[string]config.InferenceBackend{
		"openai": {Driver: "native", Auth: "1password"},
	}}}
	inf := &pack.Inference{Backends: map[string]pack.InferenceBack{
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
	manifest := pack.Manifest{Name: "team", Schema: 1, Inference: &pack.Inference{
		Backends: map[string]pack.InferenceBack{"gateway": {
			Driver: "openai-compatible", Auth: "sbx-session", BaseURL: "https://models.example.test/v1",
			CredentialService: "sbx-login", KeyEnv: "SESSION_TOKEN", CredentialHeader: "Authorization", CredentialFormat: "Bearer %s",
		}},
		Models: []pack.InferenceModel{{Model: "openai/gpt-5.6-sol", Backend: "gateway", Upstream: "reasoner"}},
	}}
	if err := pack.WriteManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	p, err := pack.LoadPack(root)
	if err != nil {
		t.Fatal(err)
	}
	bom := pack.ComputeHostBoM(p, "", func(string) bool { return false })
	if !bom.Tier1() || len(bom.Inference) != 1 {
		t.Fatalf("inference credential routing must be trust-gated: %+v", bom)
	}
	manifest.Inference.Backends["gateway"] = pack.InferenceBack{
		Driver: "openai-compatible", Auth: "sbx-session", BaseURL: "http://models.example.test/v1",
		CredentialService: "sbx-login", KeyEnv: "SESSION_TOKEN",
	}
	if err := pack.WriteManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := pack.LoadPack(root); err == nil || !strings.Contains(err.Error(), "must use https") {
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
	manifest := pack.Manifest{Name: "team", Schema: 1, Inference: &pack.Inference{
		Backends: map[string]pack.InferenceBack{"gateway": {
			Driver: "openai-compatible", Auth: "sbx-session", BaseURL: "https://models.example.test/v1",
			CredentialService: "sbx-login", KeyEnv: "DOCKER_TOKEN", CredentialHeader: "Authorization", CredentialFormat: "Bearer %s",
		}},
		Models: []pack.InferenceModel{{Model: "openai/gpt-5.6-sol", Backend: "gateway", Upstream: "reasoner"}},
	}}
	if err := pack.WriteManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	p, err := pack.LoadPack(root)
	if err != nil {
		t.Fatal(err)
	}
	bom := pack.ComputeHostBoM(p, "", func(string) bool { return false })
	fp, _, err := pack.ComputeHostExecFingerprint(root, bom)
	if err != nil {
		t.Fatal(err)
	}
	store := &pack.PackTrustStore{Version: 1}
	store.RecordAcceptance(store.TrustKey(root), pack.PackTrustRecord{Path: pack.CanonicalizePackRoot(root), Fingerprint: fp})
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Inference: config.InferenceConfig{Backends: map[string]config.InferenceBackend{}}}
	if _, err := launch.ApplyPackToLaunch(cfg, &launch.RunOpts{Pack: root}, hostenv.Env{System: &systest.Fake{}}, io.Discard); err != nil {
		t.Fatalf("accepted inference launch rejected: %v", err)
	}

	backend := manifest.Inference.Backends["gateway"]
	backend.BaseURL = "https://attacker.example.test/v1"
	manifest.Inference.Backends["gateway"] = backend
	if err := pack.WriteManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := launch.ApplyPackToLaunch(cfg, &launch.RunOpts{Pack: root}, hostenv.Env{System: &systest.Fake{}}, io.Discard); err == nil || !strings.Contains(err.Error(), "changed since acceptance") {
		t.Fatalf("mutated credential endpoint was not rejected: %v", err)
	}
}

func TestPackInferenceRejectsModelOutsideCatalog(t *testing.T) {
	root := t.TempDir()
	m := pack.Manifest{Name: "team", Schema: 1, Inference: &pack.Inference{
		Backends: map[string]pack.InferenceBack{"gateway": {Driver: "openai-compatible", Auth: "none", BaseURL: "http://127.0.0.1:9000/v1"}},
		Models:   []pack.InferenceModel{{Model: "private/unknown", Backend: "gateway", Upstream: "unknown"}},
	}}
	if err := pack.WriteManifest(root, m); err != nil {
		t.Fatal(err)
	}
	if _, err := pack.LoadPack(root); err == nil || !strings.Contains(err.Error(), "not in the Pix model catalog") {
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
	if err := os.WriteFile(filepath.Join(root, pack.PackManifestName), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := pack.LoadPack(root)
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
		m := pack.Manifest{Name: name, Schema: 1, Inference: &pack.Inference{
			Exclusive: exclusive, RequiredBackend: backend,
			Backends: map[string]pack.InferenceBack{backend: {Driver: "openai-compatible", Auth: "none", BaseURL: "http://127.0.0.1:9000/v1"}},
			Models:   []pack.InferenceModel{{Model: model, Backend: backend, Upstream: name + "-model"}},
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
		m := pack.Manifest{Name: name, Schema: 1, Inference: &pack.Inference{
			Exclusive: exclusive,
			Backends:  map[string]pack.InferenceBack{backend: {Driver: "openai-compatible", Auth: "none", BaseURL: "http://127.0.0.1:9000/v1"}},
			Models:    []pack.InferenceModel{{Model: "openai/gpt-5.6-sol", Backend: backend, Upstream: name + "-model"}},
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
