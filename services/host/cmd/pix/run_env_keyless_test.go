// run_env_keyless_test.go — the reported launch bug, at the real command
// boundary: `pix run --env work` on a PIX_HOME with no personal provider
// refs opened the base "Set up model providers from 1Password?" interview
// (and, non-interactively, refused outright) even though the selected
// environment reaches every model through its own sbx-session gateway and
// was never going to use a personal API key.
//
// The cause was ORDER, not policy: the provider-key gate ran against
// machine config.toml alone, BEFORE the selected environment's own
// [inference.*] declarations were merged in. These tests pin the fixed
// order from the outside — dispatch("run", ...), the same entry point a
// user types — and pin the control case, so the fix can never turn into
// "the key gate is gone".
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/cli"
)

// gatewaySidecar is an environment that carries its OWN inference: one
// sbx-session backend (the sandbox sends a sentinel, the sbx proxy swaps in
// the real credential on the way out) plus one model bound to it, and that
// model as [models].main. Nothing here needs, or can use, a 1Password
// provider key.
const gatewaySidecar = `schema = 1

[models]
main = "gateway/big"
# The real work environment sets this: inference is narrowed to the
# environment's OWN backends, so a machine-wide public-vendor binding stays
# configured but is not callable. The keyless answer must hold under it.
exclusive = true

[inference.backends.gateway]
driver = "openai-compatible"
base_url = "https://gateway.example.internal/v1"
auth = "sbx-session"
key_env = "GATEWAY_API_KEY"
credential_service = "gateway"

[[inference.models]]
id = "gateway/big"
backend = "gateway"
upstream_id = "big"
`

// keylessEnvHome writes <PIX_HOME>/envs/<name>/.sbxenv.yaml plus, when
// sidecar is non-empty, that environment's pix.toml. The .sbxenv.yaml is
// deliberately the minimal zero-footprint document (run_trust_test.go's
// trustTestHome shape) so the trust gate is not what this test measures.
func keylessEnvHome(t *testing.T, name, sidecar string) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "pixhome")
	envDir := filepath.Join(home, "envs", name)
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(envDir, ".sbxenv.yaml"), []byte("schemaVersion: \"1\"\nagent: pix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if sidecar != "" {
		if err := os.WriteFile(filepath.Join(envDir, "pix.toml"), []byte(sidecar), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

// keylessRunDeps puts a FAKE sbx on PATH — present (so the provider-key gate
// is reachable at all; it is skipped outright when sbx is absent) and new
// enough to pass gateSbxVersion, but unable to do anything else, so the run
// cannot reach a real create. That is the point: everything asserted here
// happens before any sandbox side effect.
func keylessRunDeps(t *testing.T, home string) (*cli.Deps, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	bin := t.TempDir()
	script := "#!/bin/sh\ncase \"$1\" in\n  --version|version) echo 'sbx version 9.9.9' ;;\n  *) exit 1 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "sbx"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("PIX_HOME", home)
	var out, errb bytes.Buffer
	return &cli.Deps{Out: &out, Err: &errb}, &out, &errb
}

// providerInterviewText is the exact evidence the base credential path ran:
// the refusal secret.ModelKeyMissingMessage prints when no ref is configured.
const providerInterviewText = "no model provider key is set"

func TestRunEnvGatewayInference_SkipsTheBaseProviderKeyGate(t *testing.T) {
	home := keylessEnvHome(t, "work", gatewaySidecar)
	d, out, errb := keylessRunDeps(t, home)
	d.Interactive = false
	// An sbx-session backend names a credential destination, which IS a
	// host-affecting fact, so this environment is legitimately trust-gated.
	// Accept it the way a user would, through the real verb, so what the run
	// below measures is the provider-key gate and nothing else.
	if code := dispatch([]string{"env", "trust", "work", "--yes"}, d); code != 0 {
		t.Fatalf("env trust exited %d:\n%s%s", code, out.String(), errb.String())
	}
	out.Reset()
	errb.Reset()

	dispatch([]string{"run", t.TempDir(), "--env", "work"}, d)

	combined := out.String() + errb.String()
	if strings.Contains(combined, providerInterviewText) {
		t.Fatalf("an environment whose inference is sbx-session must not be gated on a personal provider key; got:\n%s", combined)
	}
	// Positive proof the run got PAST the gate rather than dying before it:
	// model resolution is the very next step, and it reports the
	// environment's own [models].main choice.
	if !strings.Contains(combined, `environment "work" -> model gateway/big`) {
		t.Fatalf("the run never reached model selection, so this proves nothing about the gate; got:\n%s", combined)
	}
}

// The control: the SAME command, the same empty PIX_HOME, an environment
// that declares no inference of its own. A pi session there really does need
// a personal provider key, so the gate must still fire.
func TestRunEnvWithoutOwnInference_StillRefusesWithNoProviderKey(t *testing.T) {
	home := keylessEnvHome(t, "plain", "")
	d, out, errb := keylessRunDeps(t, home)
	d.Interactive = false

	dispatch([]string{"run", t.TempDir(), "--env", "plain"}, d)

	combined := out.String() + errb.String()
	if !strings.Contains(combined, providerInterviewText) {
		t.Fatalf("an environment with no inference of its own still needs a provider key; got:\n%s", combined)
	}
}
