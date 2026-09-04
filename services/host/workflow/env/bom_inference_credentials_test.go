// bom_inference_credentials_test.go — the sbx-session credential wiring an
// environment may now declare in pix.toml is host-affecting: it names a host
// credential service whose session token the proxy spends against a declared
// endpoint. So it has to be BOTH fingerprinted (an edit re-gates) and shown
// as a credential target (a reviewer can see which host session is spent).
package env

import (
	"testing"

	"pix/host/envinfo"
)

func credentialedInferenceSidecar(service string) *envinfo.Sidecar {
	return &envinfo.Sidecar{
		Inference: envinfo.InferenceSection{
			Backends: map[string]envinfo.InferenceBackend{
				"gw": {
					Driver: "openai-compatible", BaseURL: "https://gateway.example.com/v1",
					Auth: "sbx-session", KeyEnv: "GW_TOKEN",
					CredentialService: service, CredentialHeader: "X-Auth", CredentialFormat: "Token %s",
				},
			},
		},
	}
}

func TestBoM_InferenceCredentialWiringIsFingerprintedAndReviewable(t *testing.T) {
	var a, b BillOfMaterials
	computeInference(credentialedInferenceSidecar("gw-login"), &a)
	computeInference(credentialedInferenceSidecar("other-login"), &b)

	if len(a.Inference) != 1 || a.Inference[0].CredentialService != "gw-login" {
		t.Fatalf("inference facts = %+v, want the declared credential service", a.Inference)
	}
	if a.Inference[0].CredentialHeader != "X-Auth" || a.Inference[0].CredentialFormat != "Token %s" {
		t.Fatalf("inference facts = %+v, want the declared header/format", a.Inference)
	}

	// The credential SERVICE is a spend destination, exactly like key_env.
	var sawService bool
	for _, ct := range a.CredentialTargets {
		if ct.Destination == "https://gateway.example.com/v1" && ct.Source == "gw-login (sbx credential service)" {
			sawService = true
		}
	}
	if !sawService {
		t.Fatalf("credential targets = %+v, want the sbx credential service rendered as a spend destination", a.CredentialTargets)
	}

	fa, err := Fingerprint(a)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	fb, err := Fingerprint(b)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if fa == fb {
		t.Fatal("changing credential_service must re-gate the environment; the fingerprint did not move")
	}
}
