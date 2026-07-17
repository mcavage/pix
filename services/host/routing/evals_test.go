package routing

import (
	"fmt"
	"testing"
	"time"
)

// fakeRunner returns canned output + token counts so the harness is exercised
// with zero spend. outputs maps model id -> the text it "produces"; tokens is
// the per-call (in,out) charged so cost/budget math is assertable.
type fakeRunner struct {
	outputs map[string]string
	inTok   int
	outTok  int
	calls   int
}

func (f *fakeRunner) Run(model, prompt string) RunResult {
	f.calls++
	return RunResult{
		Output:       f.outputs[model],
		InputTokens:  f.inTok,
		OutputTokens: f.outTok,
		LatencyMs:    100,
	}
}

func evalReg() *Registry {
	return &Registry{Models: []Model{
		{ID: "good", Provider: "x", Available: true, InputPerMTok: 10, OutputPerMTok: 10},
		{ID: "bad", Provider: "x", Available: true, InputPerMTok: 10, OutputPerMTok: 10},
		{ID: "off", Provider: "x", Available: false, InputPerMTok: 10, OutputPerMTok: 10},
	}}
}

func TestRunEvals_ContainsScoringAndCost(t *testing.T) {
	cases := []Case{
		{ID: "c1", TaskType: "code", Prompt: "capital of france?",
			Scorer: Scorer{Kind: "contains", Expect: "paris"}},
	}
	runner := &fakeRunner{
		outputs: map[string]string{"good": "The answer is Paris.", "bad": "London"},
		inTok:   1_000_000, outTok: 1_000_000, // -> $20 per call at $10/$10
	}
	rep, out, err := RunEvals(evalReg(), &Scorecard{}, cases, EvalOptions{
		Now: func() time.Time { return time.Unix(0, 0) },
	}, runner)
	if err != nil {
		t.Fatal(err)
	}
	// "off" is unavailable -> not called. good + bad only = 2 calls.
	if runner.calls != 2 {
		t.Fatalf("calls = %d, want 2 (unavailable model skipped)", runner.calls)
	}
	// good scores 1, bad scores 0.
	gs, _ := out.Lookup("good", "code")
	bs, _ := out.Lookup("bad", "code")
	if gs.Accuracy != 1 || bs.Accuracy != 0 {
		t.Fatalf("accuracy good=%v bad=%v, want 1/0", gs.Accuracy, bs.Accuracy)
	}
	if gs.Source != "eval" {
		t.Fatalf("source = %q, want eval", gs.Source)
	}
	// Cost: 2 calls × $20 = $40.
	if rep.SpentUSD != 40 {
		t.Fatalf("spent = %v, want 40", rep.SpentUSD)
	}
}

func TestRunEvals_BudgetGuardHalts(t *testing.T) {
	// Many cases, tiny budget: the sweep must stop early rather than blow past.
	var cases []Case
	for i := 0; i < 10; i++ {
		cases = append(cases, Case{ID: fmt.Sprintf("c%d", i), TaskType: "code",
			Scorer: Scorer{Kind: "contains", Expect: "x"}})
	}
	runner := &fakeRunner{outputs: map[string]string{"good": "x", "bad": "x"},
		inTok: 1_000_000, outTok: 0} // $10 per call
	rep, _, err := RunEvals(evalReg(), &Scorecard{}, cases, EvalOptions{BudgetUSD: 25}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Stopped == "" {
		t.Fatal("expected the budget guard to halt the sweep")
	}
	// $25 budget at $10/call: stops after spend >= 25, i.e. 3 calls ($30).
	if rep.SpentUSD > 40 {
		t.Fatalf("spent %v — budget guard let it run away", rep.SpentUSD)
	}
}

func TestRunEvals_DryRunCallsNothing(t *testing.T) {
	cases := []Case{{ID: "c1", TaskType: "code", Scorer: Scorer{Kind: "contains", Expect: "x"}}}
	runner := &fakeRunner{outputs: map[string]string{"good": "x", "bad": "x"}}
	rep, _, err := RunEvals(evalReg(), &Scorecard{}, cases, EvalOptions{DryRun: true}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if runner.calls != 0 {
		t.Fatalf("dry-run made %d calls, want 0", runner.calls)
	}
	if len(rep.Plan) != 2 { // 2 available models × 1 case
		t.Fatalf("plan len = %d, want 2", len(rep.Plan))
	}
}

// score is a tiny test helper: run scoreCase with an unlimited budget and assert
// there was no infra error, returning just the score.
func score(t *testing.T, c Case, output string, r Runner) float64 {
	t.Helper()
	s, _, err := scoreCase(nil, c, output, r, budgetUnlimited)
	if err != nil {
		t.Fatalf("unexpected infra error: %v", err)
	}
	return s
}

func TestScoreCommand(t *testing.T) {
	// The mechanical grader: a command reads OUTPUT_FILE and greps it.
	c := Case{Scorer: Scorer{Kind: "command",
		Command: []string{"sh", "-c", "grep -q PASS \"$OUTPUT_FILE\""}}}
	if s := score(t, c, "result: PASS", nil); s != 1 {
		t.Fatalf("PASS output scored %v, want 1", s)
	}
	if s := score(t, c, "result: FAIL", nil); s != 0 {
		t.Fatalf("FAIL output scored %v, want 0", s)
	}
}

func TestScoreCommand_SeedsFiles(t *testing.T) {
	// A seeded file is present in the workdir alongside output.txt.
	c := Case{Scorer: Scorer{Kind: "command",
		Files:   map[string]string{"expected.txt": "42"},
		Command: []string{"sh", "-c", "test \"$(cat expected.txt)\" = \"$(cat \"$OUTPUT_FILE\")\""}}}
	if s := score(t, c, "42", nil); s != 1 {
		t.Fatalf("matching output scored %v, want 1", s)
	}
	if s := score(t, c, "43", nil); s != 0 {
		t.Fatalf("mismatched output scored %v, want 0", s)
	}
}

func TestScoreCommand_UnsafePathIsInfraError(t *testing.T) {
	// A seeded path escaping the workdir must be refused (infra error), never run.
	c := Case{Scorer: Scorer{Kind: "command",
		Files:   map[string]string{"../escape.txt": "x"},
		Command: []string{"true"}}}
	if _, _, err := scoreCase(nil, c, "out", nil, budgetUnlimited); err == nil {
		t.Fatal("expected an infra error for a ../ seeded path")
	}
}

func TestScoreJudge(t *testing.T) {
	// The judge model emits a leading score; we parse and clamp it.
	runner := &fakeRunner{outputs: map[string]string{"judge": "0.75\nBecause it was mostly right."}}
	c := Case{Scorer: Scorer{Kind: "judge", JudgeModel: "judge", Expect: "is it good?"}}
	if s := score(t, c, "some answer", runner); s != 0.75 {
		t.Fatalf("judge score = %v, want 0.75", s)
	}
}

func TestScoreJudge_SkippedWhenBudgetExhausted(t *testing.T) {
	runner := &fakeRunner{outputs: map[string]string{"judge": "1.0"}}
	c := Case{Scorer: Scorer{Kind: "judge", JudgeModel: "judge", Expect: "r"}}
	_, _, err := scoreCase(nil, c, "a", runner, 0) // 0 budget left
	if err == nil {
		t.Fatal("judge should be skipped (infra error) when budget is exhausted")
	}
}

func TestRunEvals_RegexAndP50(t *testing.T) {
	cases := []Case{
		{ID: "a", TaskType: "code", Scorer: Scorer{Kind: "regex", Expect: `\d{3}`}},
	}
	runner := &fakeRunner{outputs: map[string]string{"good": "code 200 ok", "bad": "no digits here"}}
	_, out, err := RunEvals(evalReg(), &Scorecard{}, cases, EvalOptions{}, runner)
	if err != nil {
		t.Fatal(err)
	}
	gs, _ := out.Lookup("good", "code")
	if gs.Accuracy != 1 {
		t.Fatalf("regex good accuracy = %v, want 1", gs.Accuracy)
	}
	if gs.LatencyMsP50 != 100 {
		t.Fatalf("p50 latency = %v, want 100", gs.LatencyMsP50)
	}
}
