package routing

import (
	"strings"
	"testing"
)

func in(name, taskType, objective string) Intent {
	return Intent{Name: name, TaskType: taskType, Objective: objective}
}

func TestResolveSingleAvailableModelServesEveryIntent(t *testing.T) {
	reg := &Registry{Models: []Model{{ID: "lab/only", Provider: "lab", Available: true}}}
	sc := &Scorecard{Scores: []Score{{Model: "lab/only", TaskType: "code", Accuracy: .5, CostUSD: 1, LatencyMsP50: 99999}}}
	pol := &Policy{DefaultFallback: "other/missing"}
	in := Intent{Name: "code", TaskType: "code", Providers: []string{"other"}, MinAccuracy: .99, MaxCostUSD: .01}
	d := Resolve(reg, sc, pol, in)
	if d.Model != "lab/only" || d.ConstraintsMet {
		t.Fatalf("decision = %+v", d)
	}
}

func TestResolve_AccuracyNoCap(t *testing.T) {
	// Pure accuracy, no ceilings: the most accurate AVAILABLE model wins (opus),
	// never the more-accurate-but-unavailable openai/retired.
	d := Resolve(testReg(), testSc(), testPol(), in("max", "code", "accuracy"))
	if d.Model != "anthropic/opus" {
		t.Fatalf("model = %q, want anthropic/opus", d.Model)
	}
	if !d.ConstraintsMet {
		t.Fatal("constraints should be met (no constraints)")
	}
}

func TestResolve_CostCapDropsOpus(t *testing.T) {
	// max_cost 0.10 excludes opus ($0.30); best remaining accuracy is openai/gpt.
	it := in("code", "code", "accuracy")
	it.MaxCostUSD = 0.10
	d := Resolve(testReg(), testSc(), testPol(), it)
	if d.Model != "openai/gpt" {
		t.Fatalf("model = %q, want openai/gpt", d.Model)
	}
	if !d.ConstraintsMet {
		t.Fatal("should be feasible")
	}
}

func TestResolve_ObjectiveCost(t *testing.T) {
	// Cheapest wins under objective=cost: the local model at $0.
	d := Resolve(testReg(), testSc(), testPol(), in("cheap", "code", "cost"))
	if d.Model != "ollama/local" {
		t.Fatalf("model = %q, want ollama/local", d.Model)
	}
}

func TestResolve_ObjectiveLatency(t *testing.T) {
	// Fastest wins under objective=latency: haiku at 12s (local is 60s).
	d := Resolve(testReg(), testSc(), testPol(), in("fast", "code", "latency"))
	if d.Model != "anthropic/haiku" {
		t.Fatalf("model = %q, want anthropic/haiku", d.Model)
	}
}

func TestResolve_MinAccuracyFloor(t *testing.T) {
	// objective=cost but accuracy floor 0.80 excludes local (0.45) and haiku
	// (0.68). Cheapest survivor is openai/gpt ($0.05) vs sonnet ($0.06).
	it := in("floor", "code", "cost")
	it.MinAccuracy = 0.80
	d := Resolve(testReg(), testSc(), testPol(), it)
	if d.Model != "openai/gpt" {
		t.Fatalf("model = %q, want openai/gpt", d.Model)
	}
}

// TestResolve_ProviderPreferenceOutranksObjective: a preference reorders the
// feasible set, so the best model from a preferred vendor beats a more accurate
// one elsewhere. Among openai/gpt (0.86) and ollama/local (0.45), accuracy picks
// openai/gpt over the (unpreferred) more accurate anthropic/opus.
func TestResolve_ProviderPreferenceOutranksObjective(t *testing.T) {
	it := in("review", "code", "accuracy")
	it.PreferProviders = []string{"openai", "ollama"}
	d := Resolve(testReg(), testSc(), testPol(), it)
	if d.Model != "openai/gpt" {
		t.Fatalf("model = %q, want openai/gpt", d.Model)
	}
	if !d.PreferenceMet {
		t.Fatal("a preference that WAS honored must report PreferenceMet")
	}
	// Honoring a preference is not a constraint violation.
	if !d.ConstraintsMet || len(d.Relaxed) > 0 {
		t.Fatalf("preference must not touch constraint reporting: %+v", d)
	}
	// Unpreferred models stay in the running, ranked behind — never filtered out.
	var sawAnthropic bool
	for _, c := range d.Alternatives {
		if c.Provider == "anthropic" {
			sawAnthropic = true
		}
	}
	if !sawAnthropic {
		t.Fatal("a preference must RANK, not exclude: anthropic should still be an alternative")
	}
}

// TestResolve_UnreachablePreferenceIsNotAFailure is the bug this replaced a
// hard allowlist to fix. The shipped policy prefers OpenAI for `overlord` (the
// default run_intent) while `pix setup` wires Anthropic, so the DEFAULT install
// could never satisfy its DEFAULT route. Under the old allowlist that install
// reported FALLBACK on its most important route while working perfectly.
func TestResolve_UnreachablePreferenceIsNotAFailure(t *testing.T) {
	it := in("overlord", "code", "accuracy")
	it.PreferProviders = []string{"provider-with-no-models-here"}
	d := Resolve(testReg(), testSc(), testPol(), it)
	if !d.ConstraintsMet {
		t.Fatalf("an unreachable PREFERENCE is not a constraint violation: %+v", d)
	}
	if len(d.Relaxed) > 0 {
		t.Fatalf("nothing was relaxed; a preference is not on the ladder: %v", d.Relaxed)
	}
	if d.PreferenceMet {
		t.Fatal("PreferenceMet must report the miss even though the route is valid")
	}
	if d.Model != "anthropic/opus" {
		t.Fatalf("model = %q, want the best model overall once the preference cannot apply", d.Model)
	}
}

// TestResolve_LegacyProvidersKeyStillPreferred: an existing hand-written
// policy.json spells the field `providers`. It must keep working — as a
// PREFERENCE now — and it must do so for an Intent built in code, not only one
// that happened to come through LoadPolicy.
func TestResolve_LegacyProvidersKeyStillPreferred(t *testing.T) {
	it := in("review", "code", "accuracy")
	it.Providers = []string{"openai"}
	d := Resolve(testReg(), testSc(), testPol(), it)
	if d.Model != "openai/gpt" || !d.PreferenceMet {
		t.Fatalf("legacy `providers` must still steer the choice: model=%q met=%v", d.Model, d.PreferenceMet)
	}
	// The new spelling wins when both are present.
	it.PreferProviders = []string{"anthropic"}
	if d := Resolve(testReg(), testSc(), testPol(), it); d.Chosen.Provider != "anthropic" {
		t.Fatalf("prefer_providers must win over the legacy key, got %q", d.Chosen.Provider)
	}
}

func TestResolve_InfeasibleRelaxesOneClassAtATime(t *testing.T) {
	// Was TestResolve_InfeasibleFallsBack, which asserted the CLIFF: nothing
	// feasible dropped every constraint at once and returned the best available
	// model overall (anthropic/opus). The ladder surrenders one class at a time,
	// so an impossible accuracy floor is dropped BEFORE the cost ceiling and the
	// free local model — which honors the ceiling — wins.
	it := in("tight", "code", "accuracy")
	it.MaxCostUSD = 0.0001 // below every non-local model, and local is 0 so...
	it.MinAccuracy = 0.99  // ...the accuracy floor rules the free local model out too
	it.Fallback = "anthropic/haiku"
	d := Resolve(testReg(), testSc(), testPol(), it)
	if d.ConstraintsMet {
		t.Fatal("should be infeasible")
	}
	if d.Model != "ollama/local" {
		t.Fatalf("degraded model = %q, want ollama/local (only the accuracy floor was surrendered)", d.Model)
	}
	if got := strings.Join(d.Relaxed, ","); got != "accuracy" {
		t.Fatalf("relaxed = %q, want \"accuracy\" (the cost ceiling still holds)", got)
	}
	if d.Reason == "" {
		t.Fatal("infeasible decision must explain itself")
	}
}

func TestResolve_UnknownTaskTypeUsesDefaultFallback(t *testing.T) {
	// No scores for this task type and no intent fallback -> policy default.
	d := Resolve(testReg(), testSc(), testPol(), in("weird", "nonexistent", "accuracy"))
	if d.ConstraintsMet {
		t.Fatal("should not be met")
	}
	if d.Model != "anthropic/sonnet" {
		t.Fatalf("model = %q, want policy default anthropic/sonnet", d.Model)
	}
}

func TestResolve_Balanced(t *testing.T) {
	// Balanced blends the three axes. With the fixture, sonnet and gpt are the
	// all-round picks over opus (pricey/slow) and local (inaccurate). Assert the
	// winner is one of the balanced mid-tier models, not an extreme.
	d := Resolve(testReg(), testSc(), testPol(), in("bal", "code", "balanced"))
	if d.Model != "openai/gpt" && d.Model != "anthropic/sonnet" && d.Model != "anthropic/haiku" {
		t.Fatalf("balanced chose %q, expected a mid-tier all-rounder", d.Model)
	}
	if d.Model == "anthropic/opus" || d.Model == "ollama/local" {
		t.Fatalf("balanced should avoid the extremes, got %q", d.Model)
	}
}

func TestResolve_Deterministic(t *testing.T) {
	it := in("code", "code", "accuracy")
	it.MaxCostUSD = 0.10
	first := Resolve(testReg(), testSc(), testPol(), it).Model
	for i := 0; i < 50; i++ {
		if got := Resolve(testReg(), testSc(), testPol(), it).Model; got != first {
			t.Fatalf("run %d = %q, first = %q (non-deterministic)", i, got, first)
		}
	}
}
