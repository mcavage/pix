package launch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/envinfo"
	"pix/host/inference"
)

// inference_integration_test.go proves the Round-7 production wiring end to
// end, the exact shape `pix run` composes at create time (run_cmd.go): a
// selected environment's OWN pix.toml [inference.*] declarations merge over
// machine config (EffectiveInferenceConfig), the merged config synthesizes a
// REAL create-time mixin kit (inference.SynthesizeInferenceKit), and that
// kit's path reaches the effective document `sbx env create` is handed —
// never a parallel, hand-built assertion about what "should" happen.

// generatedInferenceManifest reads back the exact file
// extensions/inference.ts reads at runtime: files/home/.pi/agent/inference.json.
func generatedInferenceManifest(t *testing.T, kitDir string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(kitDir, "files", "home", ".pi", "agent", "inference.json"))
	if err != nil {
		t.Fatalf("read generated inference.json: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal inference.json: %v", err)
	}
	return m
}

func modelIDs(t *testing.T, manifest map[string]any) []string {
	t.Helper()
	var ids []string
	for _, raw := range manifest["models"].([]any) {
		ids = append(ids, raw.(map[string]any)["id"].(string))
	}
	return ids
}

// TestEnvironmentAuthoredOllamaInference_ProducesAMaterializedKitInCreateArgv
// is the Ollama half of item 2's integration requirement: an environment
// pix.toml naming an Ollama backend and model, with NO machine-level
// inference configured at all, still produces a create argv (`sbx env
// create <effective>`) referencing a materialized kit whose
// files/home/.pi/agent/inference.json names that exact backend and model.
func TestEnvironmentAuthoredOllamaInference_ProducesAMaterializedKitInCreateArgv(t *testing.T) {
	stateHome(t)
	cfg := &config.Config{} // machine config: no inference configured at all
	sc := &envinfo.Sidecar{
		Models: envinfo.ModelsSection{Main: "ollama/qwen3.5:9b"},
		Inference: envinfo.InferenceSection{
			Backends: map[string]envinfo.InferenceBackend{
				"ollama": {Driver: "ollama", BaseURL: "http://host.docker.internal:11434/v1", Auth: "none"},
			},
			Models: []envinfo.InferenceModel{
				{ID: "ollama/qwen3.5:9b", Backend: "ollama", UpstreamID: "qwen3.5:9b"},
			},
		},
	}

	eff, err := EffectiveInferenceConfig(cfg, sc)
	if err != nil {
		t.Fatalf("EffectiveInferenceConfig: %v", err)
	}
	roster := RosterInputFor(sc, nil)
	kitDir, err := inference.SynthesizeInferenceKit(eff, roster)
	if err != nil {
		t.Fatalf("SynthesizeInferenceKit: %v", err)
	}
	if kitDir == "" {
		t.Fatal("SynthesizeInferenceKit returned no kit for a configured environment")
	}
	defer os.RemoveAll(kitDir)

	manifest := generatedInferenceManifest(t, kitDir)
	backends := manifest["backends"].(map[string]any)
	ob, ok := backends["ollama"].(map[string]any)
	if !ok {
		t.Fatalf("manifest has no ollama backend: %+v", manifest)
	}
	if ob["driver"] != "ollama" {
		t.Errorf("backend driver = %v, want ollama", ob["driver"])
	}
	if ids := modelIDs(t, manifest); len(ids) != 1 || ids[0] != "ollama/qwen3.5:9b" {
		t.Fatalf("model ids = %v, want [ollama/qwen3.5:9b]", ids)
	}
	roasterVal, ok := manifest["roster"].(map[string]any)
	if !ok || roasterVal["main"] != "ollama/qwen3.5:9b" {
		t.Fatalf("roster = %+v, want main=ollama/qwen3.5:9b", manifest["roster"])
	}

	// The generated kit now reaches the SAME create argv `pix run` composes
	// (EnvExtraKits/ MixinKit -> RenderEffectiveEnvironment -> EnvCreateArgs).
	in := testInput("pix-ollama-env")
	in.MixinKit = kitDir
	eff2, err := RenderEffectiveEnvironment(in, nil)
	if err != nil {
		t.Fatalf("RenderEffectiveEnvironment: %v", err)
	}
	args := EnvCreateArgs(eff2.Path)
	if len(args) != 4 || args[0] != "env" || args[1] != "create" || args[2] != "--auto-approve" || args[3] != eff2.Path {
		t.Fatalf("create argv = %v, want [env create --auto-approve %s]", args, eff2.Path)
	}
	onDisk, err := os.ReadFile(eff2.Path)
	if err != nil {
		t.Fatalf("read persisted effective document: %v", err)
	}
	if !strings.Contains(string(onDisk), kitDir) {
		t.Fatalf("the effective document the create argv references does not name the materialized kit %q:\n%s", kitDir, onDisk)
	}
}

// TestEnvironmentAuthoredLlmmanOpenAICompatibleInference_WorksAndCarriesEgressAuth
// is the llmman half: an OpenAI-compatible endpoint (exactly the shape
// llmman's own OpenAI-compatible surface presents) with sbx-session auth
// produces a kit whose spec.yaml carries the egress host AND the
// proxy-managed credential injection this environment configured.
func TestEnvironmentAuthoredLlmmanOpenAICompatibleInference_WorksAndCarriesEgressAuth(t *testing.T) {
	stateHome(t)
	cfg := &config.Config{}
	sc := &envinfo.Sidecar{
		Models: envinfo.ModelsSection{Main: "llmman/local-coder"},
		Inference: envinfo.InferenceSection{
			Backends: map[string]envinfo.InferenceBackend{
				"llmman": {
					Driver: "openai-compatible", Protocol: "openai-completions",
					BaseURL: "https://llmman.internal.example/v1", Auth: "sbx-session",
					KeyEnv: "LLMMAN_TOKEN",
				},
			},
			Models: []envinfo.InferenceModel{
				{ID: "llmman/local-coder", Backend: "llmman", UpstreamID: "local-coder"},
			},
		},
	}

	eff, err := EffectiveInferenceConfig(cfg, sc)
	if err != nil {
		t.Fatalf("EffectiveInferenceConfig: %v", err)
	}
	// A bare sbx-session backend needs a credential_service too
	// (InferenceKitSpec's own requirement) — set it on the merged snapshot
	// the way a fuller pix.toml (or a future sidecar field) would.
	b := eff.Inference.Backends["llmman"]
	b.CredentialService = "llmman"
	eff.Inference.Backends["llmman"] = b

	roster := RosterInputFor(sc, nil)
	kitDir, err := inference.SynthesizeInferenceKit(eff, roster)
	if err != nil {
		t.Fatalf("SynthesizeInferenceKit: %v", err)
	}
	if kitDir == "" {
		t.Fatal("SynthesizeInferenceKit returned no kit")
	}
	defer os.RemoveAll(kitDir)

	manifest := generatedInferenceManifest(t, kitDir)
	if ids := modelIDs(t, manifest); len(ids) != 1 || ids[0] != "llmman/local-coder" {
		t.Fatalf("model ids = %v, want [llmman/local-coder]", ids)
	}
	spec, err := os.ReadFile(filepath.Join(kitDir, "spec.yaml"))
	if err != nil {
		t.Fatalf("read spec.yaml: %v", err)
	}
	specStr := string(spec)
	if !strings.Contains(specStr, "llmman.internal.example") {
		t.Errorf("spec.yaml does not name the configured egress host:\n%s", specStr)
	}
	if !strings.Contains(specStr, "credentials:") || !strings.Contains(specStr, "LLMMAN_TOKEN") {
		t.Errorf("spec.yaml does not carry the configured sbx-session credential injection:\n%s", specStr)
	}
}

// TestEffectiveInferenceConfig_InvalidBackendDriverFailsBeforeAnySbxMutation
// proves the fail-closed half of item 2: an environment naming an
// unsupported driver refuses at the merge step, before SynthesizeInferenceKit
// (and therefore before any kit is materialized, and long before `sbx env
// create` could ever run).
func TestEffectiveInferenceConfig_InvalidBackendDriverFailsBeforeAnySbxMutation(t *testing.T) {
	stateHome(t)
	cfg := &config.Config{}
	sc := &envinfo.Sidecar{
		Inference: envinfo.InferenceSection{
			Backends: map[string]envinfo.InferenceBackend{
				"weird": {Driver: "carrier-pigeon", BaseURL: "http://example.invalid"},
			},
		},
	}
	if _, err := EffectiveInferenceConfig(cfg, sc); err == nil {
		t.Fatal("an unsupported driver must refuse before any kit is synthesized")
	}
}

// TestEffectiveInferenceConfig_EnvironmentBackendWinsOverMachineBackend
// proves the "wholesale override, never field-by-field merge" rule: an
// environment authoring the SAME backend name as machine config gets its
// own entry verbatim, and machine-only backends survive untouched.
func TestEffectiveInferenceConfig_EnvironmentBackendWinsOverMachineBackend(t *testing.T) {
	cfg := &config.Config{Inference: config.InferenceConfig{
		Backends: map[string]config.InferenceBackend{
			"ollama":  {Driver: "ollama", BaseURL: "http://machine:11434/v1", Auth: "none"},
			"machine": {Driver: "native", Auth: "none"},
		},
	}}
	sc := &envinfo.Sidecar{
		Inference: envinfo.InferenceSection{
			Backends: map[string]envinfo.InferenceBackend{
				"ollama": {Driver: "ollama", BaseURL: "http://env-local:11434/v1", Auth: "none"},
			},
		},
	}
	eff, err := EffectiveInferenceConfig(cfg, sc)
	if err != nil {
		t.Fatalf("EffectiveInferenceConfig: %v", err)
	}
	if eff.Inference.Backends["ollama"].BaseURL != "http://env-local:11434/v1" {
		t.Errorf("ollama.base_url = %q, want the environment's own value", eff.Inference.Backends["ollama"].BaseURL)
	}
	if _, ok := eff.Inference.Backends["machine"]; !ok {
		t.Error("machine-only backend was dropped by the environment overlay")
	}
	// The original cfg is never mutated.
	if cfg.Inference.Backends["ollama"].BaseURL != "http://machine:11434/v1" {
		t.Error("EffectiveInferenceConfig mutated the caller's own cfg")
	}
}
