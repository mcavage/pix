package routing

import (
	"os"
	"testing"
	"time"
)

// TestImportPromptfoo_RealFixture pins the adapter to promptfoo's ACTUAL output
// schema: testdata/promptfoo-smoke.json is a real `promptfoo eval --output` file
// captured from a live run (1 case x haiku, task_type=search, score 1).
func TestImportPromptfoo_RealFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/promptfoo-smoke.json")
	if err != nil {
		t.Fatal(err)
	}
	out, sum, err := ImportPromptfoo(&Scorecard{}, data, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Rows != 1 || sum.Scored != 1 {
		t.Fatalf("summary rows=%d scored=%d, want 1/1", sum.Rows, sum.Scored)
	}
	s, ok := out.Lookup("anthropic/claude-haiku-4-5", "search")
	if !ok {
		t.Fatal("expected a (haiku, search) score from the real fixture")
	}
	if s.Accuracy != 1 {
		t.Fatalf("accuracy = %v, want 1", s.Accuracy)
	}
	if s.Source != "eval" || s.N != 1 {
		t.Fatalf("source=%q n=%d, want eval/1", s.Source, s.N)
	}
	if s.CostUSD <= 0 || s.LatencyMsP50 <= 0 {
		t.Fatalf("cost=%v latency=%v, both should be >0 from the real run", s.CostUSD, s.LatencyMsP50)
	}
}

// craftResults builds a promptfoo results.json in the REAL schema with multiple
// models x task_types so aggregation (mean accuracy, p50 latency, mean cost, n)
// and exclusion are testable deterministically.
const craftResults = `{
  "results": { "results": [
    {"provider":{"id":"pi:m-good","label":"good"},"success":true,"score":1,"latencyMs":100,"cost":0.01,"testCase":{"metadata":{"task_type":"code"}}},
    {"provider":{"id":"pi:m-good","label":"good"},"success":false,"score":0,"latencyMs":300,"cost":0.03,"testCase":{"metadata":{"task_type":"code"}}},
    {"provider":{"id":"pi:m-good","label":"good"},"success":true,"score":1,"latencyMs":200,"cost":0.02,"testCase":{"metadata":{"task_type":"code"}}},
    {"provider":{"id":"pi:m-good","label":"good"},"success":true,"score":1,"latencyMs":50,"cost":0.005,"testCase":{"metadata":{"task_type":"reasoning"}}},
    {"provider":{"id":"pi:m-err","label":"err"},"response":{"error":"pi exited 1"},"score":0,"latencyMs":0,"cost":0,"testCase":{"metadata":{"task_type":"code"}}},
    {"provider":{"id":"pi:m-good","label":"good"},"success":false,"error":"Assertion failed","score":0,"latencyMs":150,"cost":0.015,"testCase":{"metadata":{"task_type":"reasoning"}}},
    {"provider":{"id":"openai:raw","label":"raw"},"success":true,"score":1,"testCase":{"metadata":{"task_type":"code"}}},
    {"provider":{"id":"pi:m-good","label":"good"},"success":true,"score":1,"testCase":{"metadata":{}}}
  ] }
}`

func TestImportPromptfoo_Aggregation(t *testing.T) {
	out, sum, err := ImportPromptfoo(&Scorecard{}, []byte(craftResults), time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Rows != 8 {
		t.Fatalf("rows=%d, want 8", sum.Rows)
	}
	// m-good/code: 3 scored rows (1,0,1) -> acc 0.667, p50 latency 200, mean cost 0.02.
	s, ok := out.Lookup("m-good", "code")
	if !ok {
		t.Fatal("missing m-good/code")
	}
	if s.N != 3 {
		t.Fatalf("n=%d, want 3 (the errored m-good... is a DIFFERENT model; non-pi + no-task_type rows skipped)", s.N)
	}
	if got := round3(s.Accuracy); got != 0.667 {
		t.Fatalf("accuracy=%v, want 0.667", got)
	}
	if s.LatencyMsP50 != 200 {
		t.Fatalf("p50=%v, want 200", s.LatencyMsP50)
	}
	if round3(s.CostUSD) != 0.02 { // mean(0.01, 0.03, 0.02)
		t.Fatalf("mean cost=%v, want 0.02", s.CostUSD)
	}
	// m-err: the only row errored -> excluded -> no score row at all.
	if _, ok := out.Lookup("m-err", "code"); ok {
		t.Fatal("errored-only model must not get a score row")
	}
	// Non-pi provider (openai:raw) and empty-metadata row are skipped.
	if _, ok := out.Lookup("raw", "code"); ok {
		t.Fatal("non-pi provider must be skipped")
	}
	if sum.Errored != 1 {
		t.Fatalf("errored=%d, want 1", sum.Errored)
	}
	if sum.Skipped != 2 { // openai:raw + empty-metadata
		t.Fatalf("skipped=%d, want 2", sum.Skipped)
	}
	// m-good/reasoning: two rows (score 1 + a failed-ASSERTION 0, which is a
	// legitimate 0, NOT an invocation error) -> acc 0.5, n=2.
	if r, ok := out.Lookup("m-good", "reasoning"); !ok || r.Accuracy != 0.5 || r.N != 2 {
		t.Fatalf("m-good/reasoning acc=%v n=%d ok=%v, want 0.5/2", r.Accuracy, r.N, ok)
	}
}

func TestImportPromptfoo_PreservesExistingScores(t *testing.T) {
	// A seed score for an untouched (model, task_type) survives an import.
	base := &Scorecard{Scores: []Score{
		{Model: "keep/me", TaskType: "qa", Accuracy: 0.9, Source: "seed"},
	}}
	out, _, err := ImportPromptfoo(base, []byte(craftResults), time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := out.Lookup("keep/me", "qa"); !ok || s.Accuracy != 0.9 {
		t.Fatal("untouched seed score must survive an import")
	}
}

func round3(f float64) float64 {
	return float64(int(f*1000+0.5)) / 1000
}
