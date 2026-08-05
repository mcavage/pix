package main

// modelsadd_test.go pins the fix for "setup told me I could add the others
// later, but I could not find where" — and, more importantly, for the reason
// finding it would not have helped: a second key was inert even after `pix
// setup` re-ran, because the roster only ever pruned.

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/inference"
	"pix/host/secret"
	"pix/host/sys/systest"
	"pix/host/workflow/doctor"
	"pix/host/workflow/setup"
)

// reconcileEnv fakes a host where the named providers have resolvable 1Password
// refs and every model answers its probe.
func modelsAddEnv(t *testing.T, providers ...string) hostenv.Env {
	t.Helper()
	var lines []string
	for _, p := range providers {
		for _, r := range secret.ProviderKeyRefOrder {
			if r.Name == p {
				lines = append(lines, r.EnvVar+"=op://v/i/f")
			}
		}
	}
	body := strings.Join(lines, "\n") + "\n"
	return hostenv.Env{System: &systest.Fake{LookPathFn: func(n string) (string, error) { return "/usr/bin/" + n, nil }, ReadFileFn: func(path string) (string, error) {
		if strings.HasSuffix(path, "hostmode.env") || strings.HasSuffix(path, "op-refs.env") {
			return body, nil
		}
		return "", os.ErrNotExist
	}, RunFn: func(name string, args ...string) (string, error) {
		if name == "op" {
			return "sk-test\n", nil
		}
		return "", nil
	}}, DirectInference: func(provider, model, key string) error { return nil }}
}

func rosterProviders(cfg *config.Config) string {
	return strings.Join(cfg.Inference.RosterProviders, ",")
}

func modelProvidersIn(ids []string) map[string]bool {
	out := map[string]bool{}
	for _, id := range ids {
		if p, _, ok := strings.Cut(id, "/"); ok {
			out[p] = true
		}
	}
	return out
}

// TestReconcileWidensRosterForNewProviderOnLegacyConfig is THE regression test
// for this whole feature, and for the defect the design review caught in the
// first draft of it.
//
// The user this exists for: set up with anthropic, later add google. Their
// config has allowed_models full of anthropic ids and NO roster_providers key,
// because it predates that field.
//
// The first design computed "which providers have I seen" from the live
// bindings AFTER setup.ConfigureDirectInference had already bound google. Google
// therefore counted as seen, widening skipped it, the roster stayed
// anthropic-only, setup.VerifyDirectInference skipped its bindings (they were not in
// the roster), and the command printed success — reproducing the dead end
// behind a green message. The baseline has to be captured PRE-mutation.
func TestReconcileWidensRosterForNewProviderOnLegacyConfig(t *testing.T) {
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	cfg := &config.Config{Inference: config.InferenceConfig{
		Backends: map[string]config.InferenceBackend{
			"anthropic": {Driver: "native", Auth: "1password", KeyEnv: "ANTHROPIC_API_KEY"},
		},
		Models: []config.InferenceModelBinding{
			{Model: "anthropic/claude-sonnet-5", Backend: "anthropic", Upstream: "anthropic/claude-sonnet-5", Available: true, Verified: true},
		},
		AllowedModels: []string{"anthropic/claude-sonnet-5"},
		// RosterProviders deliberately absent: this is a pre-feature config.
	}}

	res, err := setup.ReconcileDirectInference(cfg, modelsAddEnv(t, "anthropic", "google"), strings.NewReader(""), io.Discard, false, "", "")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(res.Added) != 1 || res.Added[0] != "google" {
		t.Fatalf("google must be reported as newly added, got %+v", res.Added)
	}

	got := modelProvidersIn(cfg.Inference.AllowedModels)
	if !got["google"] {
		t.Fatalf("the roster must widen to the newly added provider, got %v", cfg.Inference.AllowedModels)
	}
	if !got["anthropic"] {
		t.Fatalf("widening must not drop the provider the user already chose, got %v", cfg.Inference.AllowedModels)
	}
	// The real proof: a google model is CALLABLE, which is what "the key is
	// wired" actually means. A roster entry that never got probed is the bug.
	callable := false
	for _, b := range cfg.Inference.Models {
		if b.Backend == "google" && inference.Callable(cfg, b) {
			callable = true
		}
	}
	if !callable {
		t.Fatalf("no google binding is callable after adding the key: %+v", cfg.Inference.Models)
	}
	if rosterProviders(cfg) != "anthropic,google" {
		t.Fatalf("roster_providers must record both providers, got %q", rosterProviders(cfg))
	}
}

// TestReconcileWithNoNewProviderLeavesRosterAlone is the other half of the
// contract, and the reason widening cannot simply be unconditional: a user who
// deliberately narrowed to one model must not silently get their roster
// re-expanded every time anything reconciles.
func TestReconcileWithNoNewProviderLeavesRosterAlone(t *testing.T) {
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	cfg := &config.Config{Inference: config.InferenceConfig{
		Backends: map[string]config.InferenceBackend{
			"anthropic": {Driver: "native", Auth: "1password", KeyEnv: "ANTHROPIC_API_KEY"},
		},
		Models: []config.InferenceModelBinding{
			{Model: "anthropic/claude-sonnet-5", Backend: "anthropic", Upstream: "anthropic/claude-sonnet-5", Available: true, Verified: true},
			{Model: "anthropic/claude-opus-5", Backend: "anthropic", Upstream: "anthropic/claude-opus-5", Available: true, Verified: true},
		},
		AllowedModels:   []string{"anthropic/claude-sonnet-5"}, // narrowed on purpose
		RosterProviders: []string{"anthropic"},
	}}

	if _, err := setup.ReconcileDirectInference(cfg, modelsAddEnv(t, "anthropic"), strings.NewReader(""), io.Discard, false, "", ""); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(cfg.Inference.AllowedModels) != 1 || cfg.Inference.AllowedModels[0] != "anthropic/claude-sonnet-5" {
		t.Fatalf("a deliberate narrowing within an already-seen provider must survive, got %v", cfg.Inference.AllowedModels)
	}
}

// TestReconcileRefusesUnderMandatoryPack: writing bindings a pack's topology
// filter then silently drops would make "added" a success word with nothing
// behind it.
func TestReconcileRefusesUnderMandatoryPack(t *testing.T) {
	cfg := &config.Config{Inference: config.InferenceConfig{ExclusiveSource: "/packs/work"}}
	_, err := setup.ReconcileDirectInference(cfg, modelsAddEnv(t, "anthropic"), strings.NewReader(""), io.Discard, false, "", "")
	if err != setup.ErrInferenceExclusive {
		t.Fatalf("want setup.ErrInferenceExclusive, got %v", err)
	}
	if len(cfg.Inference.Models) != 0 || len(cfg.Inference.Backends) != 0 {
		t.Fatalf("the refusal must happen before any mutation, got %+v", cfg.Inference)
	}
}

// TestUnwiredProviderKeysReportsOnlyTheGap: the check that sends a user to
// `models add` must fire on a key with NO bindings, and must NOT fire on a key
// whose bindings exist but failed their probe — that is a different problem
// with a different fix, and conflating them sends people to the wrong command.
func TestUnwiredProviderKeysReportsOnlyTheGap(t *testing.T) {
	env := modelsAddEnv(t, "anthropic", "google")
	base := &config.Config{Inference: config.InferenceConfig{
		Backends: map[string]config.InferenceBackend{
			"anthropic": {Driver: "native", Auth: "1password", KeyEnv: "ANTHROPIC_API_KEY"},
		},
		Models: []config.InferenceModelBinding{
			{Model: "anthropic/claude-sonnet-5", Backend: "anthropic", Upstream: "anthropic/claude-sonnet-5", Available: true, Verified: true},
		},
	}}
	if gaps := doctor.UnwiredProviderKeys(base, env); len(gaps) != 1 || gaps[0] != "google" {
		t.Fatalf("google's key is set with no bindings; want [google], got %v", gaps)
	}

	// Same host, but google IS bound and merely unverified: not a wiring gap.
	bound := *base
	bound.Inference.Backends = map[string]config.InferenceBackend{
		"anthropic": {Driver: "native", Auth: "1password", KeyEnv: "ANTHROPIC_API_KEY"},
		"google":    {Driver: "native", Auth: "1password", KeyEnv: "GEMINI_API_KEY"},
	}
	bound.Inference.Models = append(append([]config.InferenceModelBinding{}, base.Inference.Models...),
		config.InferenceModelBinding{Model: "google/gemini-3.6-flash", Backend: "google", Upstream: "google/gemini-3.6-flash", Available: true})
	if gaps := doctor.UnwiredProviderKeys(&bound, env); len(gaps) != 0 {
		t.Fatalf("a bound-but-unverified provider is not a wiring gap, got %v", gaps)
	}

	// A pack owning inference makes the whole question the pack's business.
	packed := bound
	packed.Inference = base.Inference
	packed.Inference.ExclusiveSource = "/packs/work"
	if gaps := doctor.UnwiredProviderKeys(&packed, env); len(gaps) != 0 {
		t.Fatalf("no gap should be reported while a pack owns inference, got %v", gaps)
	}
}

// TestSecretSetNudgesTowardWiring: `secret set` deliberately does not probe, so
// the ONE thing standing between the user and a wired key is knowing the next
// command. Say it where they are.
func TestSecretSetNudgesTowardWiring(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "op-refs.env")
	env := hostenv.Env{System: &systest.Fake{ReadFileFn: func(string) (string, error) { return "", nil }, WriteFileFn: func(string, []byte, os.FileMode) error { return nil }}}
	systest.Of(env.System).ReadFileFn = func(p string) (string, error) {
		if p == path {
			return "", nil
		}
		return "", os.ErrNotExist
	}
	var out bytes.Buffer
	_ = secret.RunSecretSetLocked(env, &out, "ANTHROPIC_API_KEY", "op://v/i/f")
	if !strings.Contains(out.String(), "pix models add anthropic") {
		t.Errorf("setting a provider key must name the command that wires it, got:\n%s", out.String())
	}

	// A non-provider key has nothing to wire, so it must stay quiet.
	out.Reset()
	_ = secret.RunSecretSetLocked(env, &out, "SLACK_BOT_TOKEN", "op://v/i/f")
	if strings.Contains(out.String(), "pix models add") {
		t.Errorf("a non-provider secret must not suggest wiring a model provider, got:\n%s", out.String())
	}
}
