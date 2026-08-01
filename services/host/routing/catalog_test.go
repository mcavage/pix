package routing

import (
	"encoding/json"
	"strings"
	"testing"
)

// catalog_test.go guards the SHIPPED defaults (services/host/routing/defaults/*.json),
// not a fixture. These are hand-maintained files, so the failure modes are
// typos that no compiler catches and no user ever sees as an error — the model
// simply never gets used, or gets used and fails at call time.
//
// It reads the embedded bytes directly rather than LoadRegistry/LoadScorecard,
// which prefer a developer's ~/.pix/routing/*.json override and would make the
// result depend on whose machine ran the test.

func embeddedDefaults[T any](t *testing.T, name string) *T {
	t.Helper()
	b, err := defaults.ReadFile("defaults/" + name)
	if err != nil {
		t.Fatalf("read embedded defaults/%s: %v", name, err)
	}
	v := new(T)
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("parse embedded defaults/%s: %v", name, err)
	}
	return v
}

// TestShippedOllamaIDsAreValidOllamaReferences catches a catalog id that no
// real `ollama list` can ever print. Ollama references are `name[:tag]` and a
// tag may not contain a colon (see the upstream name parser), so a two-colon
// id like `qwen3.5:397b:cloud` is unmatchable: configureOllamaInference compares
// the catalog id to the listing's first field EXACTLY, so such a model is
// advertised by `pix route show`, scored in the scorecard, and silently never
// bindable on any machine. That is strictly worse than the retired kimi-k3,
// which at least failed loudly at call time.
func TestShippedOllamaIDsAreValidOllamaReferences(t *testing.T) {
	reg := embeddedDefaults[Registry](t, "models.json")
	seen := 0
	for _, m := range reg.Models {
		if m.Provider != "ollama" {
			continue
		}
		seen++
		ref := strings.TrimPrefix(m.ID, "ollama/")
		if ref == m.ID {
			t.Errorf("%s: an ollama-provider model id must be prefixed `ollama/`", m.ID)
			continue
		}
		name, tag, hasTag := strings.Cut(ref, ":")
		if name == "" {
			t.Errorf("%s: empty model name in ollama reference %q", m.ID, ref)
		}
		if hasTag {
			if tag == "" {
				t.Errorf("%s: empty tag in ollama reference %q", m.ID, ref)
			}
			if strings.Contains(tag, ":") {
				t.Errorf("%s: ollama tag %q contains a colon; a reference is name[:tag] and `ollama list` can never print this, so the model would never bind (cloud tags look like `397b-cloud`)", m.ID, tag)
			}
		}
		if strings.ContainsAny(ref, " \t/") {
			t.Errorf("%s: ollama reference %q contains whitespace or a slash", m.ID, ref)
		}
	}
	if seen == 0 {
		t.Fatal("no ollama models in the shipped catalog; this guard is not testing anything")
	}
}

// TestEveryAvailableModelIsFullyScored closes the resolver's silent-skip hole:
// Resolve drops an available model that has no scorecard row for the task type
// (resolve.go), and neither Compile nor `route compile` warns. So adding a
// model to models.json and forgetting one of the four task_type rows removes it
// from that intent's candidate set with no signal at all.
func TestEveryAvailableModelIsFullyScored(t *testing.T) {
	reg := embeddedDefaults[Registry](t, "models.json")
	sc := embeddedDefaults[Scorecard](t, "scorecard.json")
	pol := embeddedDefaults[Policy](t, "policy.json")

	taskTypes := map[string]bool{}
	for _, it := range pol.Intents {
		if it.TaskType != "" {
			taskTypes[it.TaskType] = true
		}
	}
	if len(taskTypes) == 0 {
		t.Fatal("policy declares no task types; this guard is not testing anything")
	}

	scored := map[string]bool{}
	for _, s := range sc.Scores {
		scored[s.Model+"\x00"+s.TaskType] = true
	}
	for _, m := range reg.Models {
		if !m.Available {
			continue // retired models are deliberately unscored
		}
		for tt := range taskTypes {
			if !scored[m.ID+"\x00"+tt] {
				t.Errorf("%s is available but has no scorecard row for task_type %q; the resolver will silently drop it from every intent using that task type", m.ID, tt)
			}
		}
	}
}

// TestScoredModelsExistInRegistry is the other direction: a scorecard row for a
// model id that is not in models.json is dead weight, and usually means a
// rename landed in one file and not the other.
func TestScoredModelsExistInRegistry(t *testing.T) {
	reg := embeddedDefaults[Registry](t, "models.json")
	sc := embeddedDefaults[Scorecard](t, "scorecard.json")

	known := map[string]bool{}
	for _, m := range reg.Models {
		known[m.ID] = true
	}
	for _, s := range sc.Scores {
		if !known[s.Model] {
			t.Errorf("scorecard scores %q (task_type %q) but models.json has no such model", s.Model, s.TaskType)
		}
	}
}
