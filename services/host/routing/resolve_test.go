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

func TestResolve_ProviderAllowlist(t *testing.T) {
	// Review-style: exclude anthropic. Among openai/gpt (0.86) and ollama/local
	// (0.45), accuracy picks openai/gpt.
	it := in("review", "code", "accuracy")
	it.Providers = []string{"openai", "ollama"}
	d := Resolve(testReg(), testSc(), testPol(), it)
	if d.Model != "openai/gpt" {
		t.Fatalf("model = %q, want openai/gpt", d.Model)
	}
	if d.Chosen == nil || d.Chosen.Provider == "anthropic" {
		t.Fatal("must not choose anthropic under the allowlist")
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
