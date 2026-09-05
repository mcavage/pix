package models

// roster_facts_test.go pins E3.3's L3 composition: ResolveEnvironmentRoster
// (config + envinfo, no workflow/env, no launch-time trust/containment
// machinery) and ValidateRoster ([models].exclusive plus the general
// declared-somewhere check), independent of the cmd/pix golden tests that
// exercise the same contract end to end.

import (
	"os"
	"path/filepath"
	"testing"

	"pix/host/config"
	"pix/host/inference"
)

func inferenceRosterInput(main string, agents map[string]string) inference.RosterInput {
	return inference.RosterInput{Main: main, Agents: agents}
}

func TestResolveEnvironmentRoster_NoEnvironmentSelected(t *testing.T) {
	facts, err := ResolveEnvironmentRoster(&config.Config{}, nil)
	if err != nil {
		t.Fatalf("ResolveEnvironmentRoster() error = %v, want nil", err)
	}
	if facts.Name != "" {
		t.Fatalf("facts = %+v, want a zero-Name EnvironmentRosterFacts (no environment selected)", facts)
	}
}

func TestResolveEnvironmentRoster_SelectedWithNoSidecar(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{Environment: "work", Environments: map[string]string{"work": root}}
	facts, err := ResolveEnvironmentRoster(cfg, nil)
	if err != nil {
		t.Fatalf("ResolveEnvironmentRoster() error = %v, want nil (pix.toml is optional)", err)
	}
	if facts.Name != "work" || facts.Roster.Main != "" || len(facts.LocalModels) != 0 {
		t.Fatalf("facts = %+v, want an empty roster with no sidecar present", facts)
	}
}

func writeSidecarFor(t *testing.T, content string) *config.Config {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "pix.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return &config.Config{Environment: "work", Environments: map[string]string{"work": root}}
}

func TestResolveEnvironmentRoster_ParsesSidecarRosterAndLocalModels(t *testing.T) {
	cfg := writeSidecarFor(t, `
[models]
main = "zai/glm-5"
exclusive = true

[agents]
engineer = "zai/glm-5"

[[inference.models]]
id = "zai/glm-5"
backend = "zai"
upstream_id = "glm-5"
`)
	facts, err := ResolveEnvironmentRoster(cfg, []string{"engineer", "reviewer"})
	if err != nil {
		t.Fatalf("ResolveEnvironmentRoster() error = %v", err)
	}
	if !facts.Exclusive {
		t.Error("Exclusive = false, want true")
	}
	if facts.Roster.Main != "zai/glm-5" {
		t.Errorf("Roster.Main = %q, want zai/glm-5", facts.Roster.Main)
	}
	if facts.Roster.Agents["engineer"] != "zai/glm-5" {
		t.Errorf("Roster.Agents[engineer] = %q, want zai/glm-5", facts.Roster.Agents["engineer"])
	}
	if facts.LocalModels["zai/glm-5"] != "zai" {
		t.Errorf("LocalModels[zai/glm-5] = %q, want zai", facts.LocalModels["zai/glm-5"])
	}
}

func TestValidateRoster_NoEnvironmentAlwaysPasses(t *testing.T) {
	if err := ValidateRoster(&config.Config{}, EnvironmentRosterFacts{}); err != nil {
		t.Fatalf("ValidateRoster() error = %v, want nil (no environment selected)", err)
	}
}

func TestValidateRoster_NonExclusiveAcceptsEitherMachineOrLocalModel(t *testing.T) {
	cfg := &config.Config{Inference: config.InferenceConfig{
		Models: []config.InferenceModelBinding{{Model: "anthropic/opus", Backend: "anthropic"}},
	}}
	facts := EnvironmentRosterFacts{
		Name:        "work",
		Roster:      inferenceRosterInput("anthropic/opus", nil),
		LocalModels: map[string]string{},
	}
	if err := ValidateRoster(cfg, facts); err != nil {
		t.Fatalf("machine-config model must validate: %v", err)
	}
	facts.Roster = inferenceRosterInput("zai/glm-5", nil)
	facts.LocalModels = map[string]string{"zai/glm-5": "zai"}
	if err := ValidateRoster(cfg, facts); err != nil {
		t.Fatalf("environment-local model must validate: %v", err)
	}
}

func TestValidateRoster_NonExclusiveRefusesUndeclaredModel_ExactSourceKey(t *testing.T) {
	cfg := &config.Config{}
	facts := EnvironmentRosterFacts{Name: "work", Roster: inferenceRosterInput("ghost/model", nil)}
	err := ValidateRoster(cfg, facts)
	if err == nil {
		t.Fatal("want a refusal for a model declared nowhere")
	}
	want := `pix.toml: [models].main: "ghost/model" is not declared by machine config or this environment's own [inference.models]`
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

func TestValidateRoster_ExclusiveRefusesMachineOnlyModel_ExactSourceKey(t *testing.T) {
	cfg := &config.Config{Inference: config.InferenceConfig{
		Models: []config.InferenceModelBinding{{Model: "anthropic/opus", Backend: "anthropic"}},
	}}
	facts := EnvironmentRosterFacts{
		Name: "work", Exclusive: true,
		Roster:      inferenceRosterInput("anthropic/opus", nil),
		LocalModels: map[string]string{"zai/glm-5": "zai"},
	}
	err := ValidateRoster(cfg, facts)
	if err == nil {
		t.Fatal("exclusive = true must refuse a machine-only model")
	}
	want := `pix.toml: [models].main: "anthropic/opus" is not defined in this environment's own [inference.models] (exclusive = true)`
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

func TestValidateRoster_AgentsEntryNamesItsOwnKey(t *testing.T) {
	cfg := &config.Config{}
	facts := EnvironmentRosterFacts{
		Name:   "work",
		Roster: inferenceRosterInput("", map[string]string{"engineer": "ghost/model"}),
	}
	err := ValidateRoster(cfg, facts)
	if err == nil {
		t.Fatal("want a refusal")
	}
	if got := err.Error(); got != `pix.toml: [agents].engineer: "ghost/model" is not declared by machine config or this environment's own [inference.models]` {
		t.Fatalf("error = %q", got)
	}
}
