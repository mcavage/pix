package routing

import "testing"

func TestValidate(t *testing.T) {
	reg, sc, pol := testReg(), testSc(), testPol()
	if err := Validate(reg, sc, pol); err != nil {
		t.Fatalf("clean fixtures should validate: %v", err)
	}

	// Unknown objective is rejected (not silently treated as balanced).
	bad := &Policy{DefaultFallback: "anthropic/sonnet", Intents: []Intent{
		{Name: "x", TaskType: "code", Objective: "accuarcy"},
	}}
	if err := Validate(reg, sc, bad); err == nil {
		t.Fatal("expected unknown-objective rejection")
	}

	// Fallback not in registry is rejected.
	bad2 := &Policy{DefaultFallback: "anthropic/sonnet", Intents: []Intent{
		{Name: "x", TaskType: "code", Objective: "accuracy", Fallback: "who/knows"},
	}}
	if err := Validate(reg, sc, bad2); err == nil {
		t.Fatal("expected unknown-fallback rejection")
	}

	// Missing default fallback is rejected.
	if err := Validate(reg, sc, &Policy{}); err == nil {
		t.Fatal("expected missing default_fallback rejection")
	}

	// Negative price is rejected.
	badReg := &Registry{Models: []Model{{ID: "a/b", Provider: "a", Available: true, InputPerMTok: -1}}}
	if err := Validate(badReg, &Scorecard{}, testPol()); err == nil {
		t.Fatal("expected negative-price rejection")
	}
}

func TestIsQualifiedID(t *testing.T) {
	for _, ok := range []string{"a/b", "openai/gpt-5.6-sol", "x/y/z"} {
		if !IsQualifiedID(ok) {
			t.Errorf("%q should be qualified", ok)
		}
	}
	for _, bad := range []string{"", "haiku", "/x", "x/", "/"} {
		if IsQualifiedID(bad) {
			t.Errorf("%q should NOT be qualified", bad)
		}
	}
}

func TestValidateSuite_JudgeModelMustBeInRegistry(t *testing.T) {
	cases := []Case{{ID: "j", TaskType: "qa", Scorer: Scorer{Kind: "judge", JudgeModel: "ghost/model", Expect: "r"}}}
	if err := ValidateSuite(cases, testReg()); err == nil {
		t.Fatal("expected judge_model-not-in-registry rejection")
	}
	// Same suite validates when no registry is supplied (registry check is opt-in).
	if err := ValidateSuite(cases, nil); err != nil {
		t.Fatalf("nil registry should skip the judge-model check: %v", err)
	}
}

// TestRunEvals_JudgeCostBudgetNotScorecard proves the review fix: judge spend
// counts against the BUDGET but is NOT folded into the candidate's scorecard
// cost (judges don't run at routing time).
func TestRunEvals_JudgeCostBudgetNotScorecard(t *testing.T) {
	reg := &Registry{Models: []Model{
		{ID: "cand/model", Provider: "c", Available: true, InputPerMTok: 10, OutputPerMTok: 0},
		{ID: "judge/model", Provider: "j", Available: true, InputPerMTok: 100, OutputPerMTok: 0},
	}}
	cases := []Case{{ID: "j1", TaskType: "qa",
		Scorer: Scorer{Kind: "judge", JudgeModel: "judge/model", Expect: "r"}}}
	runner := &fakeRunner{
		outputs: map[string]string{"cand/model": "answer", "judge/model": "1.0"},
		inTok:   1_000_000, outTok: 0, // candidate costs $10; judge costs $100
	}
	rep, out, err := RunEvals(reg, &Scorecard{}, cases, EvalOptions{
		Models: []string{"cand/model"}, BudgetUSD: 1000,
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	// Budget spend includes BOTH candidate ($10) and judge ($100).
	if rep.SpentUSD != 110 {
		t.Fatalf("SpentUSD = %v, want 110 (candidate + judge)", rep.SpentUSD)
	}
	// Scorecard cost for the candidate is the CANDIDATE cost only ($10), not $110.
	s, ok := out.Lookup("cand/model", "qa")
	if !ok {
		t.Fatal("missing candidate score")
	}
	if s.CostUSD != 10 {
		t.Fatalf("scorecard cost = %v, want 10 (judge cost excluded)", s.CostUSD)
	}
	if s.Accuracy != 1 {
		t.Fatalf("accuracy = %v, want 1 (judge said 1.0)", s.Accuracy)
	}
}
