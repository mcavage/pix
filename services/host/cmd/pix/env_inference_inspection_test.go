// env_inference_inspection_test.go — the P0 inspection-surface acceptance
// criteria for "Explicit Inference Setup": `pix env show` must report the
// configured provider REFS (names only, never values, never resolved) and
// where the model catalog it consulted actually lives on disk, not just the
// model/rule/agents it already printed. Folded into the existing `env show`
// verb rather than a new `--inference` flag: the nine-group surface stays
// frozen and this command was already the home for "what NAME is."
package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/cli"
)

func writeEnvInspectionFixture(t *testing.T, home, refs string) string {
	t.Helper()
	envRoot := filepath.Join(home, "envs", "work")
	if err := os.MkdirAll(envRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(envRoot, ".sbxenv.yaml"), []byte("schemaVersion: \"1\"\nagent: pix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if refs != "" {
		if err := os.WriteFile(filepath.Join(home, "secrets.env"), []byte(refs), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return envRoot
}

func TestEnvShow_ReportsConfiguredProviderRefsByNameOnly(t *testing.T) {
	home := t.TempDir()
	writeEnvInspectionFixture(t, home, "ANTHROPIC_API_KEY=op://Vault/Anthropic/key\nOPENAI_API_KEY=op://Vault/OpenAI/key\n")
	t.Setenv("PIX_HOME", home)

	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb}
	code := dispatch([]string{"env", "show", "work"}, d)
	if code != 0 {
		t.Fatalf("dispatch exit = %d, stderr=%s", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "anthropic") || !strings.Contains(got, "openai") {
		t.Fatalf("output missing configured provider names; got:\n%s", got)
	}
	if strings.Contains(got, "op://") {
		t.Fatalf("output leaked an op:// ref, not just the provider name; got:\n%s", got)
	}
}

func TestEnvShow_JSON_ReportsProvidersAndCatalogSource(t *testing.T) {
	home := t.TempDir()
	writeEnvInspectionFixture(t, home, "ANTHROPIC_API_KEY=op://Vault/Anthropic/key\n")
	t.Setenv("PIX_HOME", home)

	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb}
	code := dispatch([]string{"env", "show", "work", "--json"}, d)
	if code != 0 {
		t.Fatalf("dispatch exit = %d, stderr=%s", code, errb.String())
	}

	var fields map[string]any
	if err := json.Unmarshal(out.Bytes(), &fields); err != nil {
		t.Fatalf("output did not parse as JSON: %v\n%s", err, out.String())
	}
	providers, ok := fields["providers"].([]any)
	if !ok || len(providers) != 1 || providers[0] != "anthropic" {
		t.Fatalf(`fields["providers"] = %#v, want ["anthropic"]`, fields["providers"])
	}
	catalog, ok := fields["catalog"].(map[string]any)
	if !ok {
		t.Fatalf(`fields["catalog"] missing or not an object: %#v`, fields["catalog"])
	}
	if catalog["source"] != "embedded" {
		t.Fatalf(`catalog["source"] = %v, want "embedded"`, catalog["source"])
	}
	path, _ := catalog["runtime_path"].(string)
	if path == "" || !strings.Contains(path, "models.json") {
		t.Fatalf(`catalog["runtime_path"] = %v, want a models.json path`, catalog["runtime_path"])
	}
}

func TestEnvShow_NoProviderConfiguredReportsNoneRatherThanOmitting(t *testing.T) {
	home := t.TempDir()
	writeEnvInspectionFixture(t, home, "# no refs configured\n")
	t.Setenv("PIX_HOME", home)

	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb}
	code := dispatch([]string{"env", "show", "work"}, d)
	if code != 0 {
		t.Fatalf("dispatch exit = %d, stderr=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "providers:") {
		t.Fatalf("output must still print a providers line when none are configured; got:\n%s", out.String())
	}
}
