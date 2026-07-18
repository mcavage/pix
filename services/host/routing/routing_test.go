package routing

import (
	"testing"
	"time"
)

// testReg / testSc build a small, fully controlled fixture so tests assert on
// exact numbers rather than the shipped seed (which may be tuned over time).
func testReg() *Registry {
	return &Registry{Models: []Model{
		{ID: "anthropic/opus", Provider: "anthropic", Available: true, InputPerMTok: 15, OutputPerMTok: 75},
		{ID: "anthropic/sonnet", Provider: "anthropic", Available: true, InputPerMTok: 3, OutputPerMTok: 15},
		{ID: "anthropic/haiku", Provider: "anthropic", Available: true, InputPerMTok: 0.8, OutputPerMTok: 4},
		{ID: "openai/gpt", Provider: "openai", Available: true, InputPerMTok: 1.25, OutputPerMTok: 10},
		{ID: "ollama/local", Provider: "ollama", Available: true, Local: true},
		{ID: "openai/retired", Provider: "openai", Available: false},
	}}
}

func testSc() *Scorecard {
	return &Scorecard{Scores: []Score{
		{Model: "anthropic/opus", TaskType: "code", Accuracy: 0.90, LatencyMsP50: 45000, CostUSD: 0.300, Source: "seed"},
		{Model: "anthropic/sonnet", TaskType: "code", Accuracy: 0.82, LatencyMsP50: 25000, CostUSD: 0.060, Source: "seed"},
		{Model: "anthropic/haiku", TaskType: "code", Accuracy: 0.68, LatencyMsP50: 12000, CostUSD: 0.012, Source: "seed"},
		{Model: "openai/gpt", TaskType: "code", Accuracy: 0.86, LatencyMsP50: 30000, CostUSD: 0.050, Source: "seed"},
		{Model: "ollama/local", TaskType: "code", Accuracy: 0.45, LatencyMsP50: 60000, CostUSD: 0.000, Source: "seed"},
		// openai/retired has a score but is not Available — must never be chosen.
		{Model: "openai/retired", TaskType: "code", Accuracy: 0.99, LatencyMsP50: 1, CostUSD: 0, Source: "seed"},
	}}
}

func testPol() *Policy { return &Policy{DefaultFallback: "anthropic/sonnet"} }

func TestEmbeddedDefaultsLoad(t *testing.T) {
	// The shipped defaults must parse and be non-empty (guards a bad edit to the
	// JSON that would otherwise only fail at runtime on a real host).
	t.Setenv("ROUTING_DIR", t.TempDir()) // force embedded path (empty dir)
	reg, err := LoadRegistry()
	if err != nil || len(reg.Models) == 0 {
		t.Fatalf("registry: %v (%d models)", err, len(reg.Models))
	}
	sc, err := LoadScorecard()
	if err != nil || len(sc.Scores) == 0 {
		t.Fatalf("scorecard: %v (%d scores)", err, len(sc.Scores))
	}
	pol, err := LoadPolicy()
	if err != nil || len(pol.Intents) == 0 {
		t.Fatalf("policy: %v (%d intents)", err, len(pol.Intents))
	}
	// Every intent's fallback (or the default) and every score's model must be a
	// real registry id — a typo here silently routes to a keyless/absent model.
	for _, in := range pol.Intents {
		fb := in.Fallback
		if fb == "" {
			fb = pol.DefaultFallback
		}
		if _, ok := reg.Get(fb); !ok {
			t.Errorf("intent %q fallback %q not in registry", in.Name, fb)
		}
	}
	for _, s := range sc.Scores {
		if _, ok := reg.Get(s.Model); !ok {
			t.Errorf("scorecard model %q not in registry", s.Model)
		}
	}
}

func TestEmbeddedFastBalancedRoutesToFlash(t *testing.T) {
	// fast-balanced exists to give a fast, mid-accuracy model a home: a sub-10s
	// latency cap (drops Pro/Sol/Terra/Sonnet) above a 0.65 accuracy floor (drops
	// Flash-Lite). It must resolve to gemini-3.5-flash; if a scorecard/price edit
	// silently moves it, that is a routing regression worth catching here.
	t.Setenv("ROUTING_DIR", t.TempDir()) // force embedded defaults
	reg, sc, pol := mustLoadAll(t)
	var intent *Intent
	for i := range pol.Intents {
		if pol.Intents[i].Name == "fast-balanced" {
			intent = &pol.Intents[i]
		}
	}
	if intent == nil {
		t.Fatal("fast-balanced intent missing from embedded policy")
	}
	d := Resolve(reg, sc, pol, *intent)
	if !d.ConstraintsMet {
		t.Fatalf("fast-balanced fell back (%s): %s", d.Model, d.Reason)
	}
	if d.Model != "google/gemini-3.5-flash" {
		t.Fatalf("fast-balanced = %q, want google/gemini-3.5-flash (%s)", d.Model, d.Reason)
	}
}

// mustLoadAll loads the three embedded truth sources for a test or fails.
func mustLoadAll(t *testing.T) (*Registry, *Scorecard, *Policy) {
	t.Helper()
	reg, err := LoadRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	sc, err := LoadScorecard()
	if err != nil {
		t.Fatalf("scorecard: %v", err)
	}
	pol, err := LoadPolicy()
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	return reg, sc, pol
}

func TestCostFor(t *testing.T) {
	m := Model{InputPerMTok: 15, OutputPerMTok: 75}
	got := m.CostFor(1_000_000, 1_000_000)
	if got != 90 {
		t.Fatalf("cost = %v, want 90", got)
	}
	local := Model{Local: true, InputPerMTok: 15, OutputPerMTok: 75}
	if c := local.CostFor(1_000_000, 1_000_000); c != 0 {
		t.Fatalf("local cost = %v, want 0", c)
	}
}

func TestScorecardUpsert(t *testing.T) {
	sc := &Scorecard{}
	sc.Upsert(Score{Model: "m", TaskType: "code", Accuracy: 0.5})
	sc.Upsert(Score{Model: "m", TaskType: "code", Accuracy: 0.9}) // replace
	sc.Upsert(Score{Model: "m", TaskType: "qa", Accuracy: 0.7})   // new key
	if len(sc.Scores) != 2 {
		t.Fatalf("len = %d, want 2", len(sc.Scores))
	}
	if s, _ := sc.Lookup("m", "code"); s.Accuracy != 0.9 {
		t.Fatalf("code accuracy = %v, want 0.9 (replaced)", s.Accuracy)
	}
}

func TestScorecardSaveRoundTrip(t *testing.T) {
	t.Setenv("ROUTING_DIR", t.TempDir())
	sc := &Scorecard{Scores: []Score{
		{Model: "m", TaskType: "code", Accuracy: 0.5},
		{Model: "n", TaskType: "code", Accuracy: 0.9},
	}}
	if err := sc.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := LoadScorecard()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Scores) != 2 {
		t.Fatalf("len = %d, want 2", len(got.Scores))
	}
	// Save sorts descending accuracy within task_type: n (0.9) before m (0.5).
	if got.Scores[0].Model != "n" {
		t.Fatalf("first = %q, want n (sorted by accuracy desc)", got.Scores[0].Model)
	}
}

func TestCompileReproducible(t *testing.T) {
	reg, sc := testReg(), testSc()
	pol := &Policy{
		DefaultFallback: "anthropic/sonnet",
		Intents: []Intent{
			{Name: "code", TaskType: "code", Objective: "accuracy", MaxCostUSD: 0.10},
		},
	}
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	a := Compile(reg, sc, pol, now)
	b := Compile(reg, sc, pol, now)
	if a.Routes["code"].Model != b.Routes["code"].Model {
		t.Fatal("compile not reproducible")
	}
	// code @ max_cost 0.10, objective accuracy: opus (0.30) excluded by cost,
	// best remaining accuracy is openai/gpt (0.86).
	if got := a.Routes["code"].Model; got != "openai/gpt" {
		t.Fatalf("code -> %q, want openai/gpt", got)
	}
	if a.Version != CompiledRoutingVersion {
		t.Fatalf("version = %d", a.Version)
	}
}
