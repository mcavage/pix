package routing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// budgetUnlimited is the sentinel for "no budget cap" passed to the scorers, so a
// negative *remaining* budget (after an over-budget call) is never mistaken for
// unlimited.
const budgetUnlimited = -1.0

// env returns $key trimmed, or def when unset/blank. (Local copy so the routing
// package stays independent of the host main package.)
func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

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
	// CostUSD is the provider-reported cost when the runner could read pi's own
	// usage.cost.total (authoritative — it accounts for cache tokens + real
	// provider rates). CostReported says whether to trust it over the registry
	// estimate. Zero + CostReported=false means "fall back to registry pricing".
	CostUSD      float64
	CostReported bool
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

// knownScorers is the set of scorer kinds RunEvals can execute.
var knownScorers = map[string]bool{"contains": true, "regex": true, "command": true, "judge": true}

// ValidateSuite checks every case BEFORE any paid model call, so a typo'd regex,
// an unknown scorer kind, a command scorer with no command, or a judge scorer
// with no judge model fails fast instead of silently scoring 0 (which would
// poison the scorecard after real spend). Returns the first problem found.
func ValidateSuite(cases []Case, reg *Registry) error {
	if len(cases) == 0 {
		return fmt.Errorf("empty suite")
	}
	seen := map[string]bool{}
	for _, c := range cases {
		if c.ID == "" {
			return fmt.Errorf("a case has an empty id")
		}
		if seen[c.ID] {
			return fmt.Errorf("duplicate case id %q", c.ID)
		}
		seen[c.ID] = true
		if c.TaskType == "" {
			return fmt.Errorf("case %q: empty task_type", c.ID)
		}
		if !knownScorers[c.Scorer.Kind] {
			return fmt.Errorf("case %q: unknown scorer kind %q", c.ID, c.Scorer.Kind)
		}
		switch c.Scorer.Kind {
		case "regex":
			if _, err := regexp.Compile(c.Scorer.Expect); err != nil {
				return fmt.Errorf("case %q: bad regex: %w", c.ID, err)
			}
		case "contains":
			if c.Scorer.Expect == "" {
				return fmt.Errorf("case %q: contains scorer needs a non-empty expect", c.ID)
			}
		case "command":
			if len(c.Scorer.Command) == 0 {
				return fmt.Errorf("case %q: command scorer needs a command", c.ID)
			}
			for name := range c.Scorer.Files {
				if !safeRelPath(name) {
					return fmt.Errorf("case %q: unsafe seeded file path %q (no absolute or ../ paths)", c.ID, name)
				}
			}
		case "judge":
			if c.Scorer.JudgeModel == "" {
				return fmt.Errorf("case %q: judge scorer needs a judge_model", c.ID)
			}
			if reg != nil {
				if _, ok := reg.Get(c.Scorer.JudgeModel); !ok {
					return fmt.Errorf("case %q: judge_model %q not in registry", c.ID, c.Scorer.JudgeModel)
				}
			}
		}
	}
	return nil
}

// safeRelPath reports whether name is a relative path that stays inside the
// work dir (no absolute path, no .. traversal).
func safeRelPath(name string) bool {
	if name == "" || filepath.IsAbs(name) {
		return false
	}
	clean := filepath.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return false
	}
	return true
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
	// Validate the whole suite BEFORE any paid call so a bad case never spends
	// money and then poisons the scorecard with a spurious 0.
	if err := ValidateSuite(cases, reg); err != nil {
		return nil, nil, err
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
		// Canonicalize: an id may be an alias, but the scorecard + resolver key on
		// the canonical model.ID, so aggregate under that (else --models <alias>
		// --save would write a row the resolver never reads).
		canon := model.ID
		for _, c := range cases {
			if opts.BudgetUSD > 0 && rep.SpentUSD >= opts.BudgetUSD {
				rep.Stopped = fmt.Sprintf("budget $%.2f reached after $%.4f", opts.BudgetUSD, rep.SpentUSD)
				goto done
			}
			res := runner.Run(canon, c.Prompt)
			cost := costOf(model, res)
			rep.SpentUSD += cost
			run := EvalRun{
				Model: canon, CaseID: c.ID, TaskType: c.TaskType,
				CostUSD: cost, LatencyMs: res.LatencyMs,
				InputTokens: res.InputTokens, OutputTokens: res.OutputTokens,
			}
			// invocationFailed = the model call itself errored (auth, timeout, dead
			// stream). infraFailed = a scorer could not run (bad setup). Neither is a
			// real accuracy-0 signal, so both are EXCLUDED from the aggregate rather
			// than dragging a model's measured score to 0 after a transient blip.
			exclude := false
			if res.Err != nil {
				run.Err = res.Err.Error()
				exclude = true
			} else {
				// budgetLeft: unlimited sentinel when no cap, else remaining CLAMPED to
				// >=0 so a judge is skipped once the cap is hit (never treated as
				// unlimited via a negative remainder).
				budgetLeft := budgetUnlimited
				if opts.BudgetUSD > 0 {
					budgetLeft = opts.BudgetUSD - rep.SpentUSD
					if budgetLeft < 0 {
						budgetLeft = 0
					}
				}
				score, extraCost, serr := scoreCase(reg, c, res.Output, runner, budgetLeft)
				// Judge-model spend counts against the BUDGET, but is NOT attributed to
				// the candidate's scorecard cost (judges don't run at routing time, so
				// folding it in would wrongly inflate the candidate's runtime cost).
				rep.SpentUSD += extraCost
				if serr != nil {
					run.Err = serr.Error()
					exclude = true
				} else {
					run.Score = score
				}
			}
			rep.Runs = append(rep.Runs, run)
			if exclude {
				continue // do not let a failed run overwrite a good score
			}
			k := key(canon, c.TaskType)
			if agg[k] == nil {
				agg[k] = &acc{}
			}
			agg[k].scores = append(agg[k].scores, run.Score)
			agg[k].lats = append(agg[k].lats, res.LatencyMs)
			agg[k].costs = append(agg[k].costs, run.CostUSD)
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

// costOf returns the authoritative cost of one invocation: pi's own reported
// cost.total when available (it accounts for cache tokens + real provider
// rates), else the registry-price estimate from token counts.
func costOf(m Model, res RunResult) float64 {
	if res.CostReported {
		return res.CostUSD
	}
	return m.CostFor(res.InputTokens, res.OutputTokens)
}

// scoreCase turns one output into a 0..1 accuracy per the case's scorer. It
// returns (score, extraCostUSD, infraErr): extraCost is judge-model spend that
// must count against the budget; infraErr is a scorer-infrastructure failure
// (bad command, judge unreachable) that must NOT be recorded as a real 0.
// budgetLeft<0 means unlimited; a judge scorer is skipped when it is 0.
func scoreCase(reg *Registry, c Case, output string, runner Runner, budgetLeft float64) (float64, float64, error) {
	switch c.Scorer.Kind {
	case "contains":
		if strings.Contains(strings.ToLower(output), strings.ToLower(c.Scorer.Expect)) {
			return 1, 0, nil
		}
		return 0, 0, nil
	case "regex":
		re, err := regexp.Compile(c.Scorer.Expect)
		if err != nil {
			return 0, 0, fmt.Errorf("bad regex: %w", err)
		}
		if re.MatchString(output) {
			return 1, 0, nil
		}
		return 0, 0, nil
	case "command":
		s, err := scoreCommand(c, output)
		return s, 0, err
	case "judge":
		return scoreJudge(reg, c, output, runner, budgetLeft)
	default:
		return 0, 0, fmt.Errorf("unknown scorer kind %q", c.Scorer.Kind)
	}
}

// scoreCommand writes the model output to <workdir>/output.txt, seeds any static
// files, runs the command in workdir, and scores 1 on exit 0 else 0. The
// mechanical coding grader: a real command (go test, a build, a diff apply)
// decides correctness.
// commandScorerTimeout bounds a command scorer so a hung grader can't wedge the
// sweep. Tunable via EVAL_CMD_TIMEOUT_MS.
func commandScorerTimeout() time.Duration {
	if v := strings.TrimSpace(os.Getenv("EVAL_CMD_TIMEOUT_MS")); v != "" {
		var ms int
		if _, err := fmt.Sscanf(v, "%d", &ms); err == nil && ms > 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	return 60 * time.Second
}

// scoreCommand writes the model output to <workdir>/output.txt, seeds any static
// files, runs the command in workdir, and scores 1 on exit 0 else 0. Hardened:
// the command runs with a DEADLINE (killed on timeout), a SCRUBBED environment
// (no inherited host secrets), and in its own process group (so a fork can't
// outlive the deadline). Returns an infra error for a setup failure so a broken
// case is not misread as a legitimate score 0. NOTE: the command still runs on
// the host; a command scorer must only be pointed at TRUSTED graders. Untrusted
// model output being executed should run inside a container/VM — see
// docs/design/routing.md.
func scoreCommand(c Case, output string) (float64, error) {
	if len(c.Scorer.Command) == 0 {
		return 0, fmt.Errorf("command scorer has no command")
	}
	dir, err := os.MkdirTemp("", "eval-cmd-*")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "output.txt"), []byte(output), 0o644); err != nil {
		return 0, err
	}
	for name, content := range c.Scorer.Files {
		if !safeRelPath(name) {
			return 0, fmt.Errorf("unsafe seeded file path %q", name)
		}
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return 0, err
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return 0, err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), commandScorerTimeout())
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Scorer.Command[0], c.Scorer.Command[1:]...)
	cmd.Dir = dir
	// Scrubbed, minimal env: no inherited secrets. Only PATH + the output path.
	cmd.Env = []string{
		"PATH=" + env("PATH", "/usr/bin:/bin"),
		"OUTPUT_FILE=" + filepath.Join(dir, "output.txt"),
	}
	// Own process group + group kill on cancel, so a grader that forks children
	// can't leave descendants running past the deadline.
	setPgid(cmd)
	cmd.Cancel = func() error { return killGroup(cmd) }
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return 0, fmt.Errorf("command scorer timed out after %s", commandScorerTimeout())
		}
		// A grader that RAN and exited non-zero is a legitimate score 0. A grader
		// that could not START (not found, permission) is an INFRA failure — do
		// not record it as the model being wrong.
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return 0, nil
		}
		return 0, fmt.Errorf("command scorer failed to run: %w", err)
	}
	return 1, nil
}

// setPgid puts the command in its own process group (best-effort).
func setPgid(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killGroup SIGKILLs the command's whole process group (best-effort).
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	// Negative pid = the process group led by that pid.
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// judgeRe extracts a leading 0..1 float the judge model is asked to emit first.
var judgeRe = regexp.MustCompile(`(?s)^\s*([01](?:\.\d+)?)`)

// scoreJudge asks an LLM to rate the output 0..1 against a rubric. Opt-in and
// metered (it calls a model). Returns 0 on any parse/invocation failure.
func scoreJudge(reg *Registry, c Case, output string, runner Runner, budgetLeft float64) (float64, float64, error) {
	if runner == nil || c.Scorer.JudgeModel == "" {
		return 0, 0, fmt.Errorf("judge scorer misconfigured")
	}
	// Respect the budget: a judge call costs money, so skip it (infra-skip, not a
	// real 0) when the cap is bounded and there is no headroom left.
	if budgetLeft != budgetUnlimited && budgetLeft <= 0 {
		return 0, 0, fmt.Errorf("judge skipped: budget exhausted")
	}
	prompt := fmt.Sprintf(
		"You are a strict grader. Rate the ANSWER from 0.0 to 1.0 against the RUBRIC. "+
			"Reply with ONLY the number on the first line.\n\nRUBRIC:\n%s\n\nANSWER:\n%s\n",
		c.Scorer.Expect, output)
	res := runner.Run(c.Scorer.JudgeModel, prompt)
	// The judge's own spend counts against the budget regardless of parse success.
	// Prefer pi's reported cost; fall back to registry pricing for the judge model.
	cost := res.CostUSD
	if !res.CostReported && reg != nil {
		if jm, ok := reg.Get(c.Scorer.JudgeModel); ok {
			cost = jm.CostFor(res.InputTokens, res.OutputTokens)
		}
	}
	if res.Err != nil {
		return 0, cost, fmt.Errorf("judge invocation failed: %w", res.Err)
	}
	m := judgeRe.FindStringSubmatch(res.Output)
	if m == nil {
		return 0, cost, fmt.Errorf("judge did not emit a leading 0..1 score")
	}
	var v float64
	if _, err := fmt.Sscanf(m[1], "%f", &v); err != nil {
		return 0, cost, fmt.Errorf("judge score parse: %w", err)
	}
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	return v, cost, nil
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
