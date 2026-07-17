// pi-stack-host evals — the accuracy eval harness (host side). Runs a suite of
// cases across candidate models, scores each mechanically, records real cost +
// latency, and writes the measured scores into the scorecard so the router
// stops guessing. Budget-guarded and dry-runnable; deliberately a manual,
// on-new-model-release sweep, not an unattended spender. See docs/design/routing.md.

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"pi-stack/host/routing"
)

func runEvalsHost(args []string) {
	if len(args) < 1 {
		evalsUsage()
		os.Exit(2)
	}
	switch args[0] {
	case "run":
		evalsRun(args[1:])
	case "show":
		evalsShow(args[1:])
	case "ls":
		evalsLs(args[1:])
	case "-h", "--help", "help":
		evalsUsage()
	default:
		fmt.Fprintf(os.Stderr, "evals: unknown subcommand %q\n\n", args[0])
		evalsUsage()
		os.Exit(2)
	}
}

func evalsUsage() {
	fmt.Fprint(os.Stderr, `pi-stack-host evals — accuracy eval harness

usage:
  evals run [--suite DIR] [--models a,b] [--budget USD] [--dry-run] [--save] [--json]
  evals show [--json]      the current scorecard
  evals ls   [--suite DIR] list the cases in a suite

run flags:
  --suite DIR    directory of *.json cases (default: embedded starter suite)
  --models a,b   only these model ids (default: every available model)
  --budget USD   hard spend cap; the sweep halts before exceeding it
  --dry-run      print the plan + case matrix, call NO models, spend nothing
  --save         write the measured scores into the scorecard (default: preview)
  --json         machine-readable output

A real sweep costs money (it calls each model on each case). It is meant to be
run BY HAND on a new-model release, then followed by `+"`route compile`"+`.
`)
}

func evalsRun(args []string) {
	reg, sc, _ := loadAll()
	suite := flagValue(args, "--suite", "")
	var cases []routing.Case
	var err error
	switch {
	case suite != "":
		cases, err = routing.LoadSuite(suite)
	default:
		if _, statErr := os.Stat(routing.SuiteDir()); statErr == nil {
			cases, err = routing.LoadSuite(routing.SuiteDir())
		} else {
			cases, err = routing.LoadDefaultSuite()
		}
	}
	if err != nil {
		fatal(err)
	}
	if len(cases) == 0 {
		fatal(fmt.Errorf("no cases found"))
	}

	opts := routing.EvalOptions{
		DryRun: hasFlag(args, "--dry-run"),
		Now:    time.Now,
	}
	if m := flagValue(args, "--models", ""); m != "" {
		for _, s := range strings.Split(m, ",") {
			if s = strings.TrimSpace(s); s != "" {
				opts.Models = append(opts.Models, s)
			}
		}
	}
	if b := flagValue(args, "--budget", ""); b != "" {
		if _, e := fmt.Sscanf(b, "%f", &opts.BudgetUSD); e != nil {
			fatal(fmt.Errorf("bad --budget %q", b))
		}
	}

	rep, updated, err := routing.RunEvals(reg, sc, cases, opts, &piRunner{})
	if err != nil {
		fatal(err)
	}

	if hasFlag(args, "--json") {
		printJSON(rep)
	} else if rep.DryRun {
		fmt.Printf("DRY RUN — %d planned invocations (no models called, $0 spent):\n", len(rep.Plan))
		for _, p := range rep.Plan {
			fmt.Printf("  %s\n", p)
		}
		fmt.Println("\nRe-run without --dry-run to execute (use --budget to cap spend).")
	} else {
		fmt.Printf("ran %d invocations, spent $%.4f\n", len(rep.Runs), rep.SpentUSD)
		if rep.Stopped != "" {
			fmt.Printf("HALTED: %s\n", rep.Stopped)
		}
		fmt.Println("\nmeasured scores (source=eval):")
		for _, s := range rep.Aggregates {
			fmt.Printf("  %-28s %-10s acc %.2f  $%.4f  %.0fms  (n=%d)\n",
				s.Model, s.TaskType, s.Accuracy, s.CostUSD, s.LatencyMsP50, s.N)
		}
	}

	if hasFlag(args, "--save") && !rep.DryRun {
		if err := updated.Save(); err != nil {
			fatal(err)
		}
		fmt.Printf("\nsaved scorecard -> %s\n(run `pi-stack route compile` to update routing.json)\n", routing.ScorecardPath())
	} else if !rep.DryRun {
		fmt.Println("\n(preview only — pass --save to write these into the scorecard)")
	}
}

func evalsShow(args []string) {
	_, sc, _ := loadAll()
	if hasFlag(args, "--json") {
		printJSON(sc)
		return
	}
	for _, s := range sc.Scores {
		fmt.Printf("%-28s %-10s acc %.2f  $%.4f  %.0fms  (%s, n=%d)\n",
			s.Model, s.TaskType, s.Accuracy, s.CostUSD, s.LatencyMsP50, s.Source, s.N)
	}
}

func evalsLs(args []string) {
	suite := flagValue(args, "--suite", "")
	var cases []routing.Case
	var err error
	if suite != "" {
		cases, err = routing.LoadSuite(suite)
	} else {
		cases, err = routing.LoadDefaultSuite()
	}
	if err != nil {
		fatal(err)
	}
	for _, c := range cases {
		fmt.Printf("%-24s %-10s scorer=%s\n", c.ID, c.TaskType, c.Scorer.Kind)
	}
}

// ── piRunner: the real model invoker ─────────────────────────────────────────

// piRunner invokes a model exactly as a subagent does — a headless pi process
// (`pi --model <id> -p <prompt> --mode json --no-session --no-extensions`) — and
// reads the NDJSON event stream back for the answer text + token usage. Reusing
// pi as the invocation layer means every provider pi can already reach (Claude,
// GPT, Gemini, Ollama) is callable with no per-provider code here.
type piRunner struct{}

// piEvent is the subset of pi's --mode json events we need: assistant
// message_end carries usage + text content.
type piEvent struct {
	Type    string `json:"type"`
	Message struct {
		Role    string          `json:"role"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
		Usage   struct {
			Input  int `json:"input"`
			Output int `json:"output"`
		} `json:"usage"`
	} `json:"message"`
}

func (piRunner) Run(model, prompt string) routing.RunResult {
	bin := env("PI_BIN", "pi")
	cmd := exec.Command(bin, "--model", model, "-p", prompt,
		"--mode", "json", "--no-session", "--no-extensions")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return routing.RunResult{Err: err}
	}
	cmd.Stderr = os.Stderr
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return routing.RunResult{Err: err}
	}

	var text strings.Builder
	var inTok, outTok int
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024) // large lines (long completions)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev piEvent
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		if ev.Type == "message_end" && ev.Message.Role == "assistant" {
			inTok += ev.Message.Usage.Input
			outTok += ev.Message.Usage.Output
			text.WriteString(extractText(ev.Message.Content))
		}
	}
	waitErr := cmd.Wait()
	res := routing.RunResult{
		Output:       text.String(),
		InputTokens:  inTok,
		OutputTokens: outTok,
		LatencyMs:    float64(time.Since(start).Milliseconds()),
	}
	if waitErr != nil {
		res.Err = waitErr
	}
	return res
}

// extractText pulls text out of a pi message `content`, which is either a plain
// string or an array of blocks ({type,text}).
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, bl := range blocks {
			if bl.Text != "" {
				b.WriteString(bl.Text)
			}
		}
		return b.String()
	}
	return ""
}
