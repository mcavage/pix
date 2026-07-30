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
	badReg := &Registry{Models: []Model{{ID: "a/b", Provider: "a", Available: true, ContextWindow: 1, MaxOutputTokens: 1, InputPerMTok: -1}}}
	if err := Validate(badReg, &Scorecard{}, testPol()); err == nil {
		t.Fatal("expected negative-price rejection")
	}

	for name, model := range map[string]Model{
		"missing context":  {ID: "a/b", Provider: "a", Available: true, MaxOutputTokens: 1},
		"missing output":   {ID: "a/b", Provider: "a", Available: true, ContextWindow: 1},
		"output too large": {ID: "a/b", Provider: "a", Available: true, ContextWindow: 1, MaxOutputTokens: 2},
	} {
		t.Run(name, func(t *testing.T) {
			if err := Validate(&Registry{Models: []Model{model}}, &Scorecard{}, &Policy{DefaultFallback: "a/b"}); err == nil {
				t.Fatal("expected invalid model-limit rejection")
			}
		})
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
