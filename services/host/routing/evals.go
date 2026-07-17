package routing

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Case is one eval: a prompt with a task type and a scorer. A suite is a
// directory of *.json case files.
type Case struct {
	ID       string `json:"id"`
	TaskType string `json:"task_type"`
	Prompt   string `json:"prompt"`
	Scorer   Scorer `json:"scorer"`
}

// Scorer says how to turn a model's output into a 0..1 accuracy for one case.
//
//	contains — output contains Expect (case-insensitive) -> 1 else 0
//	regex    — output matches Expect -> 1 else 0
//	command  — write output to <workdir>/output.txt, seed Files, run Command in
//	           workdir; exit 0 -> 1 else 0. The mechanical coding scorer
//	           (build/test/patch): the model's answer is graded by a real command.
//	judge    — an LLM (JudgeModel) rates output 0..1 against the Expect rubric.
//	           Costs money; opt-in.
type Scorer struct {
	Kind       string            `json:"kind"`
	Expect     string            `json:"expect,omitempty"`
	Files      map[string]string `json:"files,omitempty"`
	Command    []string          `json:"command,omitempty"`
	JudgeModel string            `json:"judge_model,omitempty"`
}

// RunResult is one model invocation's outcome.
type RunResult struct {
	Output       string
	InputTokens  int
	OutputTokens int
	LatencyMs    float64
	Err          error
}

// Runner invokes a model with a prompt. The real implementation shells out to
// pi (host side); tests inject a fake so the whole harness runs with zero spend.
type Runner interface {
	Run(model, prompt string) RunResult
}

// EvalOptions tune a sweep.
type EvalOptions struct {
	Models    []string         // subset of registry ids; empty = all available
	BudgetUSD float64          // hard spend cap; 0 = no cap
	DryRun    bool             // print the plan + estimate, call nothing
	Now       func() time.Time // injectable clock (defaults to time.Now)
}

// EvalRun is one (model, case) result.
type EvalRun struct {
	Model        string  `json:"model"`
	CaseID       string  `json:"case_id"`
	TaskType     string  `json:"task_type"`
	Score        float64 `json:"score"`
	CostUSD      float64 `json:"cost_usd"`
	LatencyMs    float64 `json:"latency_ms"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	Err          string  `json:"err,omitempty"`
}

// EvalReport is the outcome of a sweep.
type EvalReport struct {
	Runs       []EvalRun `json:"runs"`
	Aggregates []Score   `json:"aggregates"` // new source=eval scores
	SpentUSD   float64   `json:"spent_usd"`
	Stopped    string    `json:"stopped,omitempty"` // reason if halted early (budget)
	DryRun     bool      `json:"dry_run"`
	Plan       []string  `json:"plan,omitempty"`
}

// SuiteDir is the default on-disk suite: $ROUTING_DIR/suite (else ~/.pi-stack/routing/suite).
func SuiteDir() string { return filepath.Join(Dir(), "suite") }

// LoadDefaultSuite reads the embedded starter suite (used when no --suite dir is
// given and none exists on disk). It is a small, mostly-deterministic smoke
// suite; real coding-accuracy cases (command/judge scorers) are added by the user.
func LoadDefaultSuite() ([]Case, error) {
	entries, err := defaults.ReadDir("defaults/suite")
	if err != nil {
		return nil, err
	}
	var cases []Case
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := defaults.ReadFile("defaults/suite/" + e.Name())
		if err != nil {
			return nil, err
		}
		var c Case
		if err := json.Unmarshal(b, &c); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if c.ID == "" {
			c.ID = strings.TrimSuffix(e.Name(), ".json")
		}
		cases = append(cases, c)
	}
	sort.Slice(cases, func(i, j int) bool { return cases[i].ID < cases[j].ID })
	return cases, nil
}

// LoadSuite reads every *.json case file in dir.
func LoadSuite(dir string) ([]Case, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var cases []Case
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var c Case
		if err := json.Unmarshal(b, &c); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if c.ID == "" {
			c.ID = strings.TrimSuffix(e.Name(), ".json")
		}
		cases = append(cases, c)
	}
	sort.Slice(cases, func(i, j int) bool { return cases[i].ID < cases[j].ID })
	return cases, nil
}

// RunEvals runs cases × models, scores each, enforces the budget, and returns a
// report plus a NEW scorecard (existing source=seed scores replaced by the
// measured source=eval ones for the (model, task_type) pairs it covered). Pure
// except for the injected Runner and command-scorer exec, so it is unit-tested
// with a fake runner.
func RunEvals(reg *Registry, base *Scorecard, cases []Case, opts EvalOptions, runner Runner) (*EvalReport, *Scorecard, error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	models := opts.Models
	if len(models) == 0 {
		for _, m := range reg.Models {
			if m.Available {
				models = append(models, m.ID)
			}
		}
	}
	rep := &EvalReport{DryRun: opts.DryRun}

	if opts.DryRun {
		for _, id := range models {
			for _, c := range cases {
				rep.Plan = append(rep.Plan, fmt.Sprintf("%s × %s (%s/%s)", id, c.ID, c.TaskType, c.Scorer.Kind))
			}
		}
		return rep, base, nil
	}

	// Accumulate per (model, task_type).
	type acc struct {
		scores []float64
		lats   []float64
		costs  []float64
	}
	agg := map[string]*acc{}
	key := func(m, t string) string { return m + "\x00" + t }

	for _, id := range models {
		model, ok := reg.Get(id)
		if !ok {
			return nil, nil, fmt.Errorf("model %q not in registry", id)
		}
		for _, c := range cases {
			if opts.BudgetUSD > 0 && rep.SpentUSD >= opts.BudgetUSD {
				rep.Stopped = fmt.Sprintf("budget $%.2f reached after $%.4f", opts.BudgetUSD, rep.SpentUSD)
				goto done
			}
			res := runner.Run(id, c.Prompt)
			cost := model.CostFor(res.InputTokens, res.OutputTokens)
			rep.SpentUSD += cost
			run := EvalRun{
				Model: id, CaseID: c.ID, TaskType: c.TaskType,
				CostUSD: cost, LatencyMs: res.LatencyMs,
				InputTokens: res.InputTokens, OutputTokens: res.OutputTokens,
			}
			if res.Err != nil {
				run.Err = res.Err.Error()
				// A failed invocation scores 0 (a model that errors is not accurate).
			} else {
				run.Score = scoreCase(c, res.Output, runner)
			}
			rep.Runs = append(rep.Runs, run)
			k := key(id, c.TaskType)
			if agg[k] == nil {
				agg[k] = &acc{}
			}
			agg[k].scores = append(agg[k].scores, run.Score)
			agg[k].lats = append(agg[k].lats, res.LatencyMs)
			agg[k].costs = append(agg[k].costs, cost)
		}
	}
done:

	// Fold aggregates into a copy of the base scorecard.
	out := &Scorecard{Scores: append([]Score(nil), base.Scores...)}
	ts := now().UTC().Format(time.RFC3339)
	for k, a := range agg {
		parts := strings.SplitN(k, "\x00", 2)
		s := Score{
			Model:        parts[0],
			TaskType:     parts[1],
			Accuracy:     mean(a.scores),
			LatencyMsP50: p50(a.lats),
			CostUSD:      mean(a.costs),
			N:            len(a.scores),
			Source:       "eval",
			Updated:      ts,
		}
		out.Upsert(s)
		rep.Aggregates = append(rep.Aggregates, s)
	}
	sort.Slice(rep.Aggregates, func(i, j int) bool {
		if rep.Aggregates[i].TaskType != rep.Aggregates[j].TaskType {
			return rep.Aggregates[i].TaskType < rep.Aggregates[j].TaskType
		}
		return rep.Aggregates[i].Accuracy > rep.Aggregates[j].Accuracy
	})
	return rep, out, nil
}

// scoreCase turns one output into a 0..1 accuracy per the case's scorer.
func scoreCase(c Case, output string, runner Runner) float64 {
	switch c.Scorer.Kind {
	case "contains":
		if strings.Contains(strings.ToLower(output), strings.ToLower(c.Scorer.Expect)) {
			return 1
		}
		return 0
	case "regex":
		re, err := regexp.Compile(c.Scorer.Expect)
		if err != nil {
			return 0
		}
		if re.MatchString(output) {
			return 1
		}
		return 0
	case "command":
		return scoreCommand(c, output)
	case "judge":
		return scoreJudge(c, output, runner)
	default:
		return 0
	}
}

// scoreCommand writes the model output to <workdir>/output.txt, seeds any static
// files, runs the command in workdir, and scores 1 on exit 0 else 0. The
// mechanical coding grader: a real command (go test, a build, a diff apply)
// decides correctness.
func scoreCommand(c Case, output string) float64 {
	if len(c.Scorer.Command) == 0 {
		return 0
	}
	dir, err := os.MkdirTemp("", "eval-cmd-*")
	if err != nil {
		return 0
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "output.txt"), []byte(output), 0o644); err != nil {
		return 0
	}
	for name, content := range c.Scorer.Files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return 0
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return 0
		}
	}
	cmd := exec.Command(c.Scorer.Command[0], c.Scorer.Command[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "OUTPUT_FILE="+filepath.Join(dir, "output.txt"))
	if err := cmd.Run(); err != nil {
		return 0
	}
	return 1
}

// judgeRe extracts a leading 0..1 float the judge model is asked to emit first.
var judgeRe = regexp.MustCompile(`(?s)^\s*([01](?:\.\d+)?)`)

// scoreJudge asks an LLM to rate the output 0..1 against a rubric. Opt-in and
// metered (it calls a model). Returns 0 on any parse/invocation failure.
func scoreJudge(c Case, output string, runner Runner) float64 {
	if runner == nil || c.Scorer.JudgeModel == "" {
		return 0
	}
	prompt := fmt.Sprintf(
		"You are a strict grader. Rate the ANSWER from 0.0 to 1.0 against the RUBRIC. "+
			"Reply with ONLY the number on the first line.\n\nRUBRIC:\n%s\n\nANSWER:\n%s\n",
		c.Scorer.Expect, output)
	res := runner.Run(c.Scorer.JudgeModel, prompt)
	if res.Err != nil {
		return 0
	}
	m := judgeRe.FindStringSubmatch(res.Output)
	if m == nil {
		return 0
	}
	var v float64
	if _, err := fmt.Sscanf(m[1], "%f", &v); err != nil {
		return 0
	}
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	return v
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func p50(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	cp := append([]float64(nil), xs...)
	sort.Float64s(cp)
	return cp[len(cp)/2]
}
