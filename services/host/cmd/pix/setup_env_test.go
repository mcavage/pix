package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"pix/host/cli"
	"pix/host/pixhome"
	nativeenv "pix/host/workflow/env"
)

// setup_env_test.go proves `pix setup --env NAME`'s real job (surface.md
// §3.6 step 7, wired through setupSelectedEnvironment): validate the named
// environment's declared requirements — including any local inference its
// pix.toml authors — and perform the SAME trust review `pix env trust NAME`
// does, never a generic config mutation and never a machine-default change.

// writeSetupEnvFixture creates <home>/envs/<name>/.sbxenv.yaml + pix.toml
// and returns the resolved pixhome.Paths.
func writeSetupEnvFixture(t *testing.T, home, name, sidecar string) pixhome.Paths {
	t.Helper()
	p := pixhome.New(home)
	dir := p.EnvironmentDir(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".sbxenv.yaml"), []byte("schemaVersion: \"1\"\nagent: pix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if sidecar != "" {
		if err := os.WriteFile(filepath.Join(dir, "pix.toml"), []byte(sidecar), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// preTrustSetupEnv writes an acceptance record for name so
// setupSelectedEnvironment's trust review is a no-op (this test proves the
// requirements check, not the trust prompt, which run_trust_test.go already
// covers end to end).
func preTrustSetupEnv(t *testing.T, home pixhome.Paths, name string) {
	t.Helper()
	sel, err := nativeenv.ResolveIn(home, name)
	if err != nil {
		t.Fatalf("resolve %s: %v", name, err)
	}
	bom, fp, err := environmentBoM(sel)
	_ = bom
	if err != nil {
		t.Fatalf("environmentBoM: %v", err)
	}
	if err := os.MkdirAll(home.StateTrustEnvironments, 0o700); err != nil {
		t.Fatal(err)
	}
	rec := envTrustRecord{Root: sel.Root, Fingerprint: fp, AcceptedAt: time.Now().UTC().Format(time.RFC3339)}
	b, _ := json.MarshalIndent(rec, "", "  ")
	if err := os.WriteFile(trustRecordPath(home, name), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSetupEnv_ValidOllamaInferenceAndRoster_Passes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	p := writeSetupEnvFixture(t, home, "work", `schema = 1

[models]
main = "ollama/qwen3.5:9b"

[inference.backends.ollama]
driver = "ollama"
base_url = "http://host.docker.internal:11434/v1"
auth = "none"

[[inference.models]]
id = "ollama/qwen3.5:9b"
backend = "ollama"
upstream_id = "qwen3.5:9b"
`)
	preTrustSetupEnv(t, p, "work")

	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb}
	if err := setupSelectedEnvironment(d, p, "work"); err != nil {
		t.Fatalf("setupSelectedEnvironment: %v\n%s%s", err, out.String(), errb.String())
	}
}

func TestSetupEnv_InvalidBackendDriverRefuses(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	p := writeSetupEnvFixture(t, home, "work", `schema = 1

[inference.backends.weird]
driver = "carrier-pigeon"
base_url = "http://example.invalid"
auth = "none"
`)
	preTrustSetupEnv(t, p, "work")

	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb}
	if err := setupSelectedEnvironment(d, p, "work"); err == nil {
		t.Fatal("an environment naming an unsupported inference driver must refuse")
	}
}

func TestSetupEnv_UnknownEnvironmentNameRefuses(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	p := pixhome.New(home)
	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb}
	if err := setupSelectedEnvironment(d, p, "nope"); err == nil {
		t.Fatal("an unknown --env name must refuse, never guess")
	}
}
