// env_inference_credentials_test.go — an environment repository has to be
// able to describe its OWN credentialed inference gateway. Before this, the
// three credential_* fields existed only in machine config.toml, so a
// pix.toml that declared `auth = "sbx-session"` reached kit synthesis with an
// empty identity and failed the launch: the environment contract advertised
// "your environment carries its own auth" and could not express it.
package launch

import (
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/envinfo"
	"pix/host/inference"
)

func gatewaySidecar(exclusive bool) *envinfo.Sidecar {
	return &envinfo.Sidecar{
		Models: envinfo.ModelsSection{Main: "vendor/model-a", Exclusive: exclusive},
		Inference: envinfo.InferenceSection{
			Backends: map[string]envinfo.InferenceBackend{
				"gw-vendor": {
					Driver: "openai-compatible", Protocol: "openai-responses",
					BaseURL: "https://gateway.example.com/inference/vendor/v1",
					Auth:    "sbx-session", KeyEnv: "GW_TOKEN",
					CredentialService: "gw-login",
					CredentialHeader:  "X-Auth",
					CredentialFormat:  "Token %s",
				},
			},
			Models: []envinfo.InferenceModel{
				{ID: "vendor/model-a", Backend: "gw-vendor", UpstreamID: "model-a"},
			},
		},
	}
}

func TestEffectiveInferenceConfig_CarriesSidecarCredentialWiring(t *testing.T) {
	eff, err := EffectiveInferenceConfig(&config.Config{}, gatewaySidecar(false))
	if err != nil {
		t.Fatalf("EffectiveInferenceConfig: %v", err)
	}
	b := eff.Inference.Backends["gw-vendor"]
	if b.CredentialService != "gw-login" || b.CredentialHeader != "X-Auth" || b.CredentialFormat != "Token %s" {
		t.Fatalf("credential wiring = %+v, want it carried through from pix.toml verbatim", b)
	}
	if b.Source != envInferenceSource {
		t.Fatalf("backend source = %q, want %q so an exclusivity boundary can tell it from machine config", b.Source, envInferenceSource)
	}
	if len(eff.Inference.Models) != 1 || eff.Inference.Models[0].Source != envInferenceSource {
		t.Fatalf("binding source = %+v, want the environment tag", eff.Inference.Models)
	}
	// The whole point: the synthesized kit can now actually wire the
	// injection, which is what used to fail the launch.
	if _, err := inference.InferenceKitSpec(eff); err != nil {
		t.Fatalf("InferenceKitSpec on an env-declared gateway: %v", err)
	}
}

func TestEffectiveInferenceConfig_RefusesSbxSessionWithNoCredentialIdentity(t *testing.T) {
	sc := gatewaySidecar(false)
	b := sc.Inference.Backends["gw-vendor"]
	b.CredentialService = ""
	sc.Inference.Backends["gw-vendor"] = b

	_, err := EffectiveInferenceConfig(&config.Config{}, sc)
	if err == nil {
		t.Fatal("an sbx-session backend with no credential_service must refuse at the merge step")
	}
	// It must name pix.toml and the authored key, not machine config.toml:
	// the environment author is the one who can fix it.
	if !strings.Contains(err.Error(), "pix.toml") || !strings.Contains(err.Error(), "gw-vendor") {
		t.Fatalf("error = %q, want it to name pix.toml and the backend key", err)
	}
}

// TestEffectiveInferenceConfig_ExclusiveNarrowsTheCallableSet is upstream ask
// F: `[models].exclusive` used to narrow only roster REFERENCE VALIDATION, so
// a machine-wide public-vendor binding stayed callable underneath an
// environment that declared a compliance boundary. ExclusiveSource is the
// existing runtime narrowing and had no writer left after the pack loader was
// deleted.
func TestEffectiveInferenceConfig_ExclusiveNarrowsTheCallableSet(t *testing.T) {
	machine := &config.Config{}
	machine.Inference.Backends = map[string]config.InferenceBackend{
		"public": {Driver: "native", Auth: "1password", KeyEnv: "PUBLIC_KEY"},
	}
	machine.Inference.Models = []config.InferenceModelBinding{
		{Model: "public/big", Backend: "public", Available: true, Verified: true},
	}

	eff, err := EffectiveInferenceConfig(machine, gatewaySidecar(true))
	if err != nil {
		t.Fatalf("EffectiveInferenceConfig: %v", err)
	}
	if eff.Inference.ExclusiveSource != envInferenceSource {
		t.Fatalf("ExclusiveSource = %q, want %q", eff.Inference.ExclusiveSource, envInferenceSource)
	}
	for _, b := range eff.Inference.Models {
		want := b.Source == envInferenceSource
		if got := inference.TopologyAllowed(eff, b); got != want {
			t.Fatalf("TopologyAllowed(%s from %q) = %v, want %v", b.Model, b.Source, got, want)
		}
	}
	if inference.BackendAllowed(eff, machine.Inference.Backends["public"], "public") {
		t.Fatal("a machine-wide backend must not stay callable under an exclusive environment")
	}
	// The machine's own on-disk config is never mutated by the merge.
	if machine.Inference.ExclusiveSource != "" {
		t.Fatal("EffectiveInferenceConfig wrote through to the caller's config")
	}
}

// An `exclusive = true` with nothing to be exclusive TO leaves zero callable
// models. That is a configuration mistake to name, not a boundary to enforce
// silently into an unlaunchable environment.
func TestEffectiveInferenceConfig_ExclusiveWithNoBackendsRefuses(t *testing.T) {
	sc := &envinfo.Sidecar{Models: envinfo.ModelsSection{Exclusive: true}}
	if _, err := EffectiveInferenceConfig(&config.Config{}, sc); err == nil {
		t.Fatal("exclusive with no environment backends must refuse rather than produce an empty callable set")
	}
}
