package main

// models_golden_test.go pins E3.3's own contract for `pix models` and
// `pix agent ls`: FACTS ONLY (MODEL/BACKEND/SOURCE, AGENT/MODEL/SOURCE), no
// WHY column, no score, no price, no wired/unwired/retired status taxonomy,
// and no hidden routing pick — a read command never invokes intent-based
// resolution to fill a gap these commands themselves report as absent.
//
// Two scenarios per PRD: no environment selected, and a selected environment
// with its own pix.toml roster/model declarations — plus the
// [models].exclusive refusal.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/cli"
	"pix/host/config"
)

// forbiddenScoredTaxonomy is every word/phrase that would leak the retired
// scored-routing display (WHY, score, price, wired/unwired/retired) back
// into a facts-only surface.
var forbiddenScoredTaxonomy = []string{"WHY", "why explains", "score", "$0.", "wired", "unwired", "retired"}

func assertNoScoredTaxonomy(t *testing.T, label, got string) {
	t.Helper()
	lower := strings.ToLower(got)
	for _, banned := range forbiddenScoredTaxonomy {
		if strings.Contains(lower, strings.ToLower(banned)) {
			t.Errorf("%s must be facts-only, found banned %q in:\n%s", label, banned, got)
		}
	}
}

// TestModelsGolden_NoEnvironment: with no environment selected, `pix models`
// lists exactly the machine-config-bound models, SOURCE "machine config",
// and nothing scored.
func TestModelsGolden_NoEnvironment(t *testing.T) {
	cfg := &config.Config{Inference: config.InferenceConfig{
		Backends: map[string]config.InferenceBackend{
			"anthropic": {Driver: "native", Auth: "1password", KeyEnv: "ANTHROPIC_API_KEY"},
		},
		Models: []config.InferenceModelBinding{{
			Model: "anthropic/claude-opus-5", Backend: "anthropic",
			Upstream: "anthropic/claude-opus-5", Available: true, Verified: true,
		}},
	}}
	d, out, _ := testDeps(cfg)
	if err := runRootParse([]string{"models"}, d); err != nil {
		t.Fatalf("bare `models` must succeed with no environment selected: %v", err)
	}
	got := out.String()
	want := "MODEL\tBACKEND\tSOURCE\n"
	if !strings.Contains(strings.ReplaceAll(got, "  ", "\t"), "MODEL") {
		t.Fatalf("missing header, got:\n%s", got)
	}
	for _, line := range []string{"MODEL", "BACKEND", "SOURCE", "anthropic/claude-opus-5", "anthropic", "machine config"} {
		if !strings.Contains(got, line) {
			t.Errorf("missing %q in:\n%s", line, got)
		}
	}
	if strings.Contains(got, "environment:") {
		t.Errorf("no environment selected must never print an environment SOURCE, got:\n%s", got)
	}
	assertNoScoredTaxonomy(t, "pix models (no environment)", got)
	_ = want
}

// writeSelectedEnvironment scaffolds a registered, selected environment with
// a pix.toml sidecar declaring its own [models]/[agents]/[inference.*], and
// returns the ready-to-use *config.Config.
func writeSelectedEnvironment(t *testing.T, sidecar string) *config.Config {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".sbxenv.yaml"), []byte("schemaVersion: \"2\"\nkind: environment\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pix.toml"), []byte(sidecar), 0o644); err != nil {
		t.Fatal(err)
	}
	return &config.Config{
		Environment:  "work",
		Environments: map[string]string{"work": root},
	}
}

// TestModelsGolden_SelectedEnvironment: an environment's own
// [[inference.models]] declarations show up too, SOURCE "environment: work".
func TestModelsGolden_SelectedEnvironment(t *testing.T) {
	cfg := writeSelectedEnvironment(t, `
[models]
main = "zai/glm-5"

[agents]
engineer = "zai/glm-5"

[inference.backends.zai]
driver = "openai-compatible"
auth = "none"
base_url = "https://api.z.ai/api/paas/v4"

[[inference.models]]
id = "zai/glm-5"
backend = "zai"
upstream_id = "glm-5"
`)
	d, out, _ := testDeps(cfg)
	if err := runRootParse([]string{"models"}, d); err != nil {
		t.Fatalf("bare `models` must succeed with a selected environment: %v", err)
	}
	got := out.String()
	for _, line := range []string{"MODEL", "BACKEND", "SOURCE", "zai/glm-5", "zai", "environment: work"} {
		if !strings.Contains(got, line) {
			t.Errorf("missing %q in:\n%s", line, got)
		}
	}
	assertNoScoredTaxonomy(t, "pix models (selected environment)", got)
}

// TestModelsGolden_ExclusiveRefusesMachineOnlyModel is E3.3's own red test:
// [models].exclusive = true refuses a roster reference to a model this
// environment did not declare itself — even one this MACHINE can otherwise
// call — exit 2, naming the exact source file and key.
func TestModelsGolden_ExclusiveRefusesMachineOnlyModel(t *testing.T) {
	cfg := writeSelectedEnvironment(t, `
[models]
main = "anthropic/claude-opus-5"
exclusive = true
`)
	cfg.Inference.Backends = map[string]config.InferenceBackend{
		"anthropic": {Driver: "native", Auth: "1password", KeyEnv: "ANTHROPIC_API_KEY"},
	}
	cfg.Inference.Models = []config.InferenceModelBinding{{
		Model: "anthropic/claude-opus-5", Backend: "anthropic",
		Upstream: "anthropic/claude-opus-5", Available: true, Verified: true,
	}}
	d, out, _ := testDeps(cfg)
	err := runRootParse([]string{"models"}, d)
	if err == nil {
		t.Fatalf("exclusive = true must refuse a machine-only model, got success with output:\n%s", out.String())
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("exit code = %d, want 2 (a usage refusal), err = %v", got, err)
	}
	for _, want := range []string{"pix.toml", "[models].main", "anthropic/claude-opus-5"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal must name the exact source/key, missing %q in %q", want, err.Error())
		}
	}
	if out.String() != "" {
		t.Errorf("a refused command must print nothing on Out, got:\n%s", out.String())
	}
}

// TestAgentLsGolden_SourceColumn proves the three named SOURCE cases plus the
// no-roster neutral case, end to end through the real `pix agent ls` verb.
func TestAgentLsGolden_SourceColumn(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.MkdirAll("agents", 0o755); err != nil {
		t.Fatal(err)
	}
	// explicit: its own frontmatter model: wins outright.
	writeAgentFile(t, "explicit-agent", "---\nmodel: openai/gpt-5.6-sol\n---\n\nBody.\n")
	// roster: no explicit model:, but the environment names it in [agents].
	writeAgentFile(t, "engineer", "---\ndescription: go engineer\n---\n\nBody.\n")
	// main: no explicit model:, no [agents] entry -> falls back to [models].main.
	writeAgentFile(t, "reviewer", "---\ndescription: reviewer\n---\n\nBody.\n")

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".sbxenv.yaml"), []byte("schemaVersion: \"2\"\nkind: environment\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const sidecar = `
[models]
main = "zai/glm-5"

[agents]
engineer = "openai/gpt-5.6-sol"

[inference.backends.zai]
driver = "openai-compatible"
auth = "none"
base_url = "https://api.z.ai/api/paas/v4"

[[inference.models]]
id = "zai/glm-5"
backend = "zai"
upstream_id = "glm-5"

[[inference.models]]
id = "openai/gpt-5.6-sol"
backend = "zai"
upstream_id = "gpt-5.6-sol"
`
	if err := os.WriteFile(filepath.Join(root, "pix.toml"), []byte(sidecar), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Environment: "work", Environments: map[string]string{"work": root}}

	d, out, _ := testDeps(cfg)
	if err := runRootParse([]string{"agent", "ls"}, d); err != nil {
		t.Fatalf("agent ls: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"explicit-agent", "openai/gpt-5.6-sol", agentSourceExplicit,
		"engineer", agentSourceRoster,
		"reviewer", "zai/glm-5", agentSourceMain,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	assertNoScoredTaxonomy(t, "agent ls (selected environment)", got)
}

// TestAgentLsGolden_NoEnvironment: no environment selected, no explicit
// model: — every row is "(inherit parent)" with the neutral no-roster SOURCE.
func TestAgentLsGolden_NoEnvironment(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.MkdirAll("agents", 0o755); err != nil {
		t.Fatal(err)
	}
	writeAgentFile(t, "engineer", "---\ndescription: go engineer\n---\n\nBody.\n")

	d, out, _ := testDeps(&config.Config{})
	if err := runRootParse([]string{"agent", "ls"}, d); err != nil {
		t.Fatalf("agent ls: %v", err)
	}
	got := out.String()
	for _, want := range []string{"AGENT", "MODEL", "SOURCE", "engineer", "(inherit parent)", agentSourceNone} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	assertNoScoredTaxonomy(t, "agent ls (no environment)", got)
}

func writeAgentFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join("agents", name+".md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
