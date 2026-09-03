package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pix/host/cli"
	"pix/host/pixhome"
	"pix/host/secret"
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

// TestSetupEnv_MissingDeclaredSecretRefuses proves surface.md §3.6 step
// 7's whole point: an env_keys name with no recorded op:// ref is caught
// generically, BEFORE any `[[setup]]` hook runs — replacing the pattern of
// an environment shipping its own hook whose only job is failing on
// purpose to print `pix secret set` commands.
func TestSetupEnv_MissingDeclaredSecretRefuses(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	p := writeSetupEnvFixture(t, home, "work", `schema = 1

[host.mcp.google-workspace]
env_keys = ["GOG_KEYRING_PASSWORD"]
`)
	preTrustSetupEnv(t, p, "work")

	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb}
	err := setupSelectedEnvironment(d, p, "work")
	if err == nil {
		t.Fatal("a declared secret with no recorded op:// ref must refuse setup")
	}
	if !strings.Contains(err.Error(), "requirement(s) not recorded") {
		t.Errorf("expected a concise requirements refusal, got: %v", err)
	}
	if !strings.Contains(out.String(), "pix secret set GOG_KEYRING_PASSWORD op://") {
		t.Errorf("expected the exact pix secret set remedy in output:\n%s", out.String())
	}
}

// TestSetupEnv_DeclaredSecretRefRecordedPasses proves the same check
// passes once the op:// ref is recorded, without needing a
// `[[setup]]`-hook round trip to prove it.
func TestSetupEnv_DeclaredSecretRefRecordedPasses(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	p := writeSetupEnvFixture(t, home, "work", `schema = 1

[host.mcp.google-workspace]
env_keys = ["GOG_KEYRING_PASSWORD"]
`)
	preTrustSetupEnv(t, p, "work")
	if err := secret.SetRef(p, "GOG_KEYRING_PASSWORD", "op://Private/gog/password"); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb}
	if err := setupSelectedEnvironment(d, p, "work"); err != nil {
		t.Fatalf("setupSelectedEnvironment: %v\n%s%s", err, out.String(), errb.String())
	}
}

// TestSetupEnv_PlainKeyPromptedInteractivelyPasses proves a plain_keys
// name (a non-secret account string) is collected right here on a TTY and
// recorded WITHOUT an op:// reference — never through `pix secret set`.
func TestSetupEnv_PlainKeyPromptedInteractivelyPasses(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	p := writeSetupEnvFixture(t, home, "work", `schema = 1

[host.mcp.google-workspace]
plain_keys = ["GOG_ACCOUNT"]
`)
	preTrustSetupEnv(t, p, "work")

	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb, In: strings.NewReader("you@docker.com\n"), Interactive: true}
	if err := setupSelectedEnvironment(d, p, "work"); err != nil {
		t.Fatalf("setupSelectedEnvironment: %v\n%s%s", err, out.String(), errb.String())
	}
	val, present := secret.PlainValue(p, "GOG_ACCOUNT")
	if !present || val != "you@docker.com" {
		t.Fatalf("expected GOG_ACCOUNT recorded as a plain value, got %q present=%v", val, present)
	}
	content, err := os.ReadFile(p.SecretsEnv)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "op://") {
		t.Errorf("a plain_keys value must never be stored as an op:// reference:\n%s", content)
	}
}

// TestSetupEnv_MissingPlainKeyNonInteractiveRefusesConcisely proves a
// missing plain value on a non-interactive terminal is a concise,
// actionable refusal — never a hook that fails on purpose.
func TestSetupEnv_MissingPlainKeyNonInteractiveRefusesConcisely(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	p := writeSetupEnvFixture(t, home, "work", `schema = 1

[host.mcp.google-workspace]
plain_keys = ["GOG_ACCOUNT"]
`)
	preTrustSetupEnv(t, p, "work")

	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb}
	err := setupSelectedEnvironment(d, p, "work")
	if err == nil {
		t.Fatal("a missing declared non-secret value must refuse on a non-interactive terminal")
	}
	if !strings.Contains(out.String(), "GOG_ACCOUNT is a non-secret value") {
		t.Errorf("expected a concise, actionable remedy naming GOG_ACCOUNT, got:\n%s", out.String())
	}
}

// TestSetup_EnvFlagSuppressesBaseParallelPrompt proves `--env NAME` skips
// the base-install Parallel-search offer/report entirely (never called at
// all, not merely gated on interactivity) — a named environment's own
// declared roster makes that base-install noise irrelevant.
func TestSetup_EnvFlagSuppressesBaseParallelPrompt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	p := pixhome.New(home)
	if err := os.MkdirAll(p.Home, 0o700); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb}
	setupCredentials(d, false)

	if strings.Contains(out.String(), "Parallel web search") {
		t.Errorf("a named --env setup must not print the base Parallel-search prompt/report:\n%s", out.String())
	}
}

// TestSetup_EnvFlagSuppressesBaseModelPrompt proves a named `--env NAME`
// setup never reaches the base default-model picker at all, even when a
// bare `pix setup` under the identical conditions would.
func TestSetup_EnvFlagSuppressesBaseModelPrompt(t *testing.T) {
	runSetup := func(t *testing.T, env string) string {
		t.Helper()
		home := t.TempDir()
		t.Setenv("PIX_HOME", home)
		// A configured provider ref: setupModelSelection's own early-return
		// gate ("no provider configured yet") would otherwise mask this
		// test's question, which is whether --env skips the picker, not
		// whether a provider is configured.
		if err := os.WriteFile(filepath.Join(home, "secrets.env"), []byte("ANTHROPIC_API_KEY=op://Vault/Anthropic/key\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		dir, _ := fakeInstallDir(t, "2.0.0")
		docker := &setupFakeDocker{}
		mcp := &setupFakeMCP{}
		if env != "" {
			// A zero-Tier1 environment: no host command/service/mount/
			// credential, so trust is satisfied with no acceptance record
			// and no prompt of its own — this test is about the base
			// model/Parallel prompts, not the trust screen.
			writeSetupEnvFixture(t, home, env, "schema = 1\n")
		}
		var out, errb bytes.Buffer
		d := &cli.Deps{Out: &out, Err: &errb, In: strings.NewReader("y\n\n"), Interactive: true}
		if err := (&setupCmd{Env: env}).run(d, setupSeamsFor(t, dir, docker, mcp)); err != nil {
			t.Fatalf("pix setup --env %q: %v\n%s%s", env, err, out.String(), errb.String())
		}
		return out.String()
	}

	baseOut := runSetup(t, "")
	if !strings.Contains(baseOut, "Default model:") {
		t.Fatalf("a bare `pix setup` must still show the base default-model prompt:\n%s", baseOut)
	}

	envOut := runSetup(t, "work")
	if strings.Contains(envOut, "Default model:") {
		t.Errorf("`pix setup --env work` must not print the base default-model prompt:\n%s", envOut)
	}
	if strings.Contains(envOut, "Parallel web search") {
		t.Errorf("`pix setup --env work` must not print the base Parallel-search prompt/report:\n%s", envOut)
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
