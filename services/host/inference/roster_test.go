package inference

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"pix/host/config"
)

// testCfg is the one-backend, one-model config every roster test composes
// against: a single callable binding "zai/glm-5", so buildRoster's "known
// models" set is exactly {"zai/glm-5"}.
func testCfg() *config.Config {
	return &config.Config{Inference: config.InferenceConfig{
		Backends: map[string]config.InferenceBackend{
			"zai": {Driver: "openai-compatible", Auth: "none", BaseURL: "https://api.z.ai/api/paas/v4"},
		},
		Models: []config.InferenceModelBinding{
			{Model: "zai/glm-5", Backend: "zai", Upstream: "glm-5", Available: true},
		},
	}}
}

// TestBuildRoster_EmptyMainIsNoRoster pins the "no environment roster
// selected" case: a zero-value RosterInput (Main == "") must build no
// roster at all, not an error and not an empty-but-present one. This is
// exactly what an unmodified caller (today, every one of them) passes, and
// it is what keeps a v1 manifest byte-for-byte unchanged for a caller that
// never named an environment.
func TestBuildRoster_EmptyMainIsNoRoster(t *testing.T) {
	models := manifestModels(testCfg(), mustRegistry(t))
	roster, err := buildRoster(RosterInput{}, models)
	if err != nil {
		t.Fatalf("buildRoster() error = %v, want nil", err)
	}
	if roster != nil {
		t.Fatalf("buildRoster() = %+v, want nil (no environment roster selected)", roster)
	}
}

// TestBuildRoster_UndefinedMainModel_ExactError pins the exact copy PRD §5.7
// requires for an undefined `[models].main` reference: the sidecar file,
// the doc's own bracket-table key spelling, and a reason naming the model.
func TestBuildRoster_UndefinedMainModel_ExactError(t *testing.T) {
	models := manifestModels(testCfg(), mustRegistry(t))
	_, err := buildRoster(RosterInput{Main: "zai/glm-4"}, models)
	if err == nil {
		t.Fatal("buildRoster() error = nil, want an undefined-model error")
	}
	rerr, ok := err.(*RosterError)
	if !ok {
		t.Fatalf("buildRoster() error type = %T, want *RosterError", err)
	}
	want := &RosterError{File: "pix.toml", Key: "[models].main", Reason: `"zai/glm-4" is not a generated model`}
	if *rerr != *want {
		t.Fatalf("buildRoster() error = %+v, want %+v", rerr, want)
	}
	wantText := `pix.toml: [models].main: "zai/glm-4" is not a generated model`
	if rerr.Error() != wantText {
		t.Fatalf("Error() = %q, want %q", rerr.Error(), wantText)
	}
}

// TestBuildRoster_UndefinedAgentModel_ExactError pins the identical shape for
// an `[agents].<name>` entry, including the exact bracket-table spelling
// naming the offending agent.
func TestBuildRoster_UndefinedAgentModel_ExactError(t *testing.T) {
	models := manifestModels(testCfg(), mustRegistry(t))
	in := RosterInput{
		Main:          "zai/glm-5",
		Agents:        map[string]string{"engineer": "zai/glm-4"},
		ShippedAgents: []string{"engineer", "review"},
	}
	_, err := buildRoster(in, models)
	if err == nil {
		t.Fatal("buildRoster() error = nil, want an undefined-model error")
	}
	rerr, ok := err.(*RosterError)
	if !ok {
		t.Fatalf("buildRoster() error type = %T, want *RosterError", err)
	}
	want := &RosterError{File: "pix.toml", Key: "[agents].engineer", Reason: `"zai/glm-4" is not a generated model`}
	if *rerr != *want {
		t.Fatalf("buildRoster() error = %+v, want %+v", rerr, want)
	}
}

// TestBuildRoster_AbsentAgentMapsToMain pins §6.4: a shipped agent name with
// no [agents] entry resolves to Main, while an authored entry overrides it.
func TestBuildRoster_AbsentAgentMapsToMain(t *testing.T) {
	models := manifestModels(testCfg(), mustRegistry(t))
	in := RosterInput{
		Main:          "zai/glm-5",
		Agents:        map[string]string{"engineer": "zai/glm-5"},
		ShippedAgents: []string{"engineer", "review", "fanout"},
	}
	roster, err := buildRoster(in, models)
	if err != nil {
		t.Fatalf("buildRoster() error = %v", err)
	}
	if roster == nil {
		t.Fatal("buildRoster() = nil, want a roster")
	}
	if roster.Main != "zai/glm-5" {
		t.Fatalf("roster.Main = %q, want zai/glm-5", roster.Main)
	}
	want := map[string]string{"engineer": "zai/glm-5", "review": "zai/glm-5", "fanout": "zai/glm-5"}
	if len(roster.Agents) != len(want) {
		t.Fatalf("roster.Agents = %+v, want %+v", roster.Agents, want)
	}
	for name, model := range want {
		if roster.Agents[name] != model {
			t.Fatalf("roster.Agents[%q] = %q, want %q (absent [agents] entry must map to main)", name, roster.Agents[name], model)
		}
	}
}

// TestBuildRoster_CustomAgentNotShippedGetsNoEntry: a custom project agent
// name (not in ShippedAgents) that names no [agents] entry gets NO roster
// entry at all — the composition boundary never invents one for a name it
// was not told about. Its own `model:` precedence is reader-side (E3.2) and
// out of scope here.
func TestBuildRoster_CustomAgentNotShippedGetsNoEntry(t *testing.T) {
	models := manifestModels(testCfg(), mustRegistry(t))
	in := RosterInput{Main: "zai/glm-5", ShippedAgents: []string{"engineer"}}
	roster, err := buildRoster(in, models)
	if err != nil {
		t.Fatalf("buildRoster() error = %v", err)
	}
	if _, ok := roster.Agents["custom-agent"]; ok {
		t.Fatalf("roster.Agents = %+v, must not carry an entry for an unnamed custom agent", roster.Agents)
	}
}

// TestBuildRoster_AuthoredAgentOverridesShippedDefault confirms an authored
// [agents] entry wins over the absent-maps-to-main default, even when the
// name is also a shipped agent.
func TestBuildRoster_AuthoredAgentOverridesShippedDefault(t *testing.T) {
	cfg := testCfg()
	cfg.Inference.Backends["google"] = config.InferenceBackend{Driver: "native", Auth: "sbx-session"}
	cfg.Inference.Models = append(cfg.Inference.Models, config.InferenceModelBinding{
		Model: "google/gemini-3.1-pro-preview", Backend: "google", Upstream: "gemini-3.1-pro-preview", Available: true,
	})
	models := manifestModels(cfg, mustRegistry(t))
	in := RosterInput{
		Main:          "zai/glm-5",
		Agents:        map[string]string{"review": "google/gemini-3.1-pro-preview"},
		ShippedAgents: []string{"engineer", "review"},
	}
	roster, err := buildRoster(in, models)
	if err != nil {
		t.Fatalf("buildRoster() error = %v", err)
	}
	if roster.Agents["engineer"] != "zai/glm-5" {
		t.Fatalf("roster.Agents[engineer] = %q, want the main default", roster.Agents["engineer"])
	}
	if roster.Agents["review"] != "google/gemini-3.1-pro-preview" {
		t.Fatalf("roster.Agents[review] = %q, want the authored override", roster.Agents["review"])
	}
}

// TestManifestModelsAlwaysReferenceAGeneratedBackend pins the invariant
// buildRoster's validation leans on: every model RuntimeManifest
// emits already references a backend present in the SAME manifest's
// Backends map — never a backend narrowed out by exclusivity or otherwise
// absent — so validating a roster reference against the model list is
// exactly "resolves to a model whose backend Pix generates a provider for",
// with no separate backend lookup to fall out of sync.
func TestManifestModelsAlwaysReferenceAGeneratedBackend(t *testing.T) {
	cfg := testCfg()
	manifest, err := RuntimeManifest(cfg, RosterInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Models) == 0 {
		t.Fatal("manifest has no models to check")
	}
	for _, m := range manifest.Models {
		if _, ok := manifest.Backends[m.Backend]; !ok {
			t.Fatalf("model %q references backend %q, which is not in the generated backends map %+v", m.ID, m.Backend, manifest.Backends)
		}
	}
}

// TestRuntimeManifest_RosterErrorPropagates confirms an invalid
// roster fails RuntimeManifest itself (the composition boundary a
// real caller uses), carrying the same *RosterError a direct buildRoster
// call produces — never swallowed into a generic error.
func TestRuntimeManifest_RosterErrorPropagates(t *testing.T) {
	cfg := testCfg()
	_, err := RuntimeManifest(cfg, RosterInput{Main: "zai/glm-4"})
	if err == nil {
		t.Fatal("RuntimeManifest() error = nil, want the undefined-model roster error")
	}
	if _, ok := err.(*RosterError); !ok {
		t.Fatalf("RuntimeManifest() error type = %T, want *RosterError", err)
	}
}

// TestSynthesizeInferenceKit_NoRosterOmitsKey pins old-v1-reader
// compatibility: a caller passing the zero-value RosterInput (every caller
// this story does not yet wire) must produce a manifest with NO "roster"
// key at all, not a present-but-empty one, so an older reader — which
// never heard of the field either way — sees byte-for-byte the same shape
// it always has.
func TestSynthesizeInferenceKit_NoRosterOmitsKey(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	dir, err := SynthesizeInferenceKit(testCfg(), RosterInput{})
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	raw := readManifestFile(t, dir)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("inference.json did not parse: %v", err)
	}
	if _, present := doc["roster"]; present {
		t.Fatalf("inference.json carries a %q key with no environment roster selected: %s", "roster", raw)
	}
}

// TestSynthesizeInferenceKit_WithRosterWritesAdditiveField proves the
// additive v1 shape end to end: version/backends/models are unchanged, and
// "roster" appears with exactly {main, agents{}}.
func TestSynthesizeInferenceKit_WithRosterWritesAdditiveField(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	in := RosterInput{Main: "zai/glm-5", Agents: map[string]string{"engineer": "zai/glm-5"}, ShippedAgents: []string{"engineer"}}
	dir, err := SynthesizeInferenceKit(testCfg(), in)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	raw := readManifestFile(t, dir)
	var doc struct {
		Version  int                       `json:"version"`
		Backends map[string]map[string]any `json:"backends"`
		Models   []map[string]any          `json:"models"`
		Roster   *struct {
			Main   string            `json:"main"`
			Agents map[string]string `json:"agents"`
		} `json:"roster"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("inference.json did not parse: %v\n%s", err, raw)
	}
	if doc.Version != 1 {
		t.Fatalf("version = %d, want 1", doc.Version)
	}
	if len(doc.Models) == 0 || len(doc.Backends) == 0 {
		t.Fatalf("existing manifest compatibility broken: backends=%+v models=%+v", doc.Backends, doc.Models)
	}
	if doc.Roster == nil {
		t.Fatalf("inference.json has no roster key: %s", raw)
	}
	if doc.Roster.Main != "zai/glm-5" {
		t.Fatalf("roster.main = %q, want zai/glm-5", doc.Roster.Main)
	}
	if doc.Roster.Agents["engineer"] != "zai/glm-5" {
		t.Fatalf("roster.agents.engineer = %q, want zai/glm-5", doc.Roster.Agents["engineer"])
	}
}

func readManifestFile(t *testing.T, kitDir string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(kitDir, "files", "home", ".pi", "agent", inferenceManifestFilename))
	if err != nil {
		t.Fatalf("inference.json unreadable: %v", err)
	}
	return raw
}

func mustRegistry(t *testing.T) *Catalog {
	t.Helper()
	reg, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	return reg
}
