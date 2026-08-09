package routing

import "testing"

// anthropicOnlyRegistry is the shape of a host where `pix setup` wired Anthropic
// and nothing else: the other vendors are in the catalog but no probed binding
// made them callable, which is exactly what RegistryForBindings produces there.
func anthropicOnlyRegistry(t *testing.T) (*Registry, *Scorecard) {
	t.Helper()
	reg := embeddedDefaults[Registry](t, "models.json")
	sc := embeddedDefaults[Scorecard](t, "scorecard.json")
	for i := range reg.Models {
		if reg.Models[i].Provider != "anthropic" {
			reg.Models[i].Available = false
		}
	}
	return reg, sc
}

// TestOverlordRelaxesToOpusNotFable pins the shipped default the whole stack
// leans on: `overlord` is the run_intent, so whatever it resolves to IS the
// interactive session model.
//
// prefer_providers is a PREFERENCE, so on an Anthropic-only host the OpenAI
// preference simply goes unhonored and the best model overall wins. With no cost
// ceiling that was Fable 5 (reasoning accuracy 0.94 > Opus 5's 0.93) — the
// frontier model deliberately reserved for red-team, quietly installed as the
// default session model at roughly twice Opus's per-task price, on a host that
// never asked for it. The 0.30 cap is the fix; this test is why it cannot be
// dropped again.
func TestOverlordRelaxesToOpusNotFable(t *testing.T) {
	reg, sc := anthropicOnlyRegistry(t)
	pol := embeddedDefaults[Policy](t, "policy.json")
	in, ok := pol.Intent("overlord")
	if !ok {
		t.Fatal("shipped policy must define the overlord intent: it is the default run_intent")
	}
	d := Resolve(reg, sc, pol, in)
	if d.Chosen == nil {
		t.Fatalf("overlord must resolve on an anthropic-only host: %+v", d)
	}
	if d.Chosen.ID == "anthropic/claude-fable-5" {
		t.Fatalf("overlord fell back to Fable 5, the red-team-reserved frontier model: %+v", d)
	}
	if d.Chosen.ID != "anthropic/claude-opus-5" {
		t.Fatalf("overlord fallback = %q, want anthropic/claude-opus-5", d.Chosen.ID)
	}
	if d.PreferenceMet {
		t.Fatal("the openai preference cannot be met here and must report as unmet")
	}
}

// TestOverlordPrefersOpenAIWhenWired is the contrast: the cap must not disturb
// the intended cross-vendor route on a fully wired host.
func TestOverlordPrefersOpenAIWhenWired(t *testing.T) {
	reg := embeddedDefaults[Registry](t, "models.json")
	sc := embeddedDefaults[Scorecard](t, "scorecard.json")
	pol := embeddedDefaults[Policy](t, "policy.json")
	in, _ := pol.Intent("overlord")
	d := Resolve(reg, sc, pol, in)
	if d.Chosen == nil || d.Chosen.ID != "openai/gpt-5.6-sol" {
		t.Fatalf("fully wired overlord = %+v, want openai/gpt-5.6-sol", d.Chosen)
	}
}
