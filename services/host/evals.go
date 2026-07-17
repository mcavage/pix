// pi-stack-host evals — the eval harness, now a thin driver over promptfoo.
//
// promptfoo owns running + scoring (evals/ in the repo: promptfooconfig.yaml +
// providers/pi.js + suites/*.yaml). This subcommand shells `promptfoo eval`,
// then imports the results.json into the router's scorecard. `pi` stays the
// model-invocation layer (via the pi provider), so credentials remain
// proxy-managed and every provider pi reaches is evaluable. See
// docs/design/routing.md.

package main

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	"pi-stack/host/routing"
)

// configProviderLabels reads the provider LABELS from promptfooconfig.yaml — the
// exact key `runPromptfoo` filters on (--filter-providers matches the label). A
// requested model is valid iff it equals a label; validating against config.model
// instead would pass a model that the filter then can't select. The config sets
// label == model id (documented there); a provider whose label differs from its
// config.model is a misconfig and is reported.
func configProviderLabels(cfgPath string) (labels map[string]bool, mislabeled []string) {
	labels = map[string]bool{}
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		return labels, nil
	}
	var c struct {
		Providers []struct {
			Label  string `yaml:"label"`
			Config struct {
				Model string `yaml:"model"`
			} `yaml:"config"`
		} `yaml:"providers"`
	}
	if yaml.Unmarshal(b, &c) != nil {
		return labels, nil
	}
	for _, p := range c.Providers {
		if p.Label != "" {
			labels[p.Label] = true
			if p.Config.Model != "" && p.Config.Model != p.Label {
				mislabeled = append(mislabeled, fmt.Sprintf("%s (label) != %s (model)", p.Label, p.Config.Model))
			}
		}
	}
	return labels, mislabeled
}

func runEvalsHost(args []string) {
	if len(args) < 1 {
		evalsUsage()
		os.Exit(2)
	}
	switch args[0] {
	case "run":
		evalsRun(args[1:])
	case "import":
		evalsImport(args[1:])
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
	fmt.Fprint(os.Stderr, `pi-stack-host evals — accuracy eval harness (promptfoo)

usage:
  evals run [--config P] [--models a,b] [--budget USD] [--dry-run] [--save] [--json]
  evals import FILE [--save] [--json]   fold a promptfoo results.json into the scorecard
  evals show [--json]                   the current scorecard
  evals ls [--config P]                 list the suites/cases

run flags:
  --config P     path to promptfooconfig.yaml (default: $EVALS_CONFIG or ./evals/promptfooconfig.yaml)
  --models a,b   only these model ids (filters promptfoo providers pi:<model>)
  --budget USD   spend cap: models are evaluated one at a time and the sweep
                 STOPS before a model that would exceed the cap (advisory: the
                 last model's own matrix runs whole)
  --dry-run      print the plan, call nothing, spend nothing
  --save         write measured scores into the scorecard (default: preview)

A real sweep calls each model on each case and COSTS MONEY. Run it by hand on a
new-model release, then `+"`pi-stack route compile`"+`.
`)
}

// evalsConfig resolves the promptfoo config path.
func evalsConfig(args []string) string {
	if c := flagValue(args, "--config", ""); c != "" {
		return c
	}
	if c := strings.TrimSpace(os.Getenv("EVALS_CONFIG")); c != "" {
		return c
	}
	return filepath.Join("evals", "promptfooconfig.yaml")
}

func evalsRun(args []string) {
	cfg := evalsConfig(args)
	if _, err := os.Stat(cfg); err != nil {
		fatal(fmt.Errorf("promptfoo config not found at %s (run from the repo root, or pass --config)", cfg))
	}
	promptfoo := env("PROMPTFOO_BIN", "promptfoo")
	if _, err := exec.LookPath(promptfoo); err != nil {
		fatal(fmt.Errorf("promptfoo not on PATH (npm i -g promptfoo), or set PROMPTFOO_BIN"))
	}

	configured, mislabeled := configProviderLabels(cfg)
	if len(mislabeled) > 0 {
		fmt.Fprintf(os.Stderr, "warning: promptfoo provider label != config.model for %v; --models filters on the label, so keep them equal.\n", mislabeled)
	}

	// Which models: explicit --models, else every available model in the registry
	// that is also a configured promptfoo provider.
	var models []string
	if hasFlag(args, "--models") {
		m := flagValue(args, "--models", "")
		seen := map[string]bool{}
		for _, s := range strings.Split(m, ",") {
			if s = strings.TrimSpace(s); s != "" && !seen[s] {
				seen[s] = true
				models = append(models, s)
			}
		}
		if len(models) == 0 {
			fatal(fmt.Errorf("--models was given but empty"))
		}
		// Every requested model MUST have a provider entry, else promptfoo runs it
		// against nothing and the sweep silently no-ops.
		var missing []string
		for _, m := range models {
			if !configured[m] {
				missing = append(missing, m)
			}
		}
		if len(missing) > 0 {
			fatal(fmt.Errorf("no promptfoo provider for %v in %s (add a providers: entry with that model, then re-run)", missing, cfg))
		}
	} else {
		reg, err := routing.LoadRegistry()
		if err != nil {
			fatal(err)
		}
		for _, mm := range reg.Models {
			if !mm.Available {
				continue
			}
			if !configured[mm.ID] {
				fmt.Fprintf(os.Stderr, "note: %s is available but has no promptfoo provider entry; skipping (add it to %s to eval it).\n", mm.ID, cfg)
				continue
			}
			models = append(models, mm.ID)
		}
		if len(models) == 0 {
			fatal(fmt.Errorf("no models to run: none of the registry's available models have a provider in %s", cfg))
		}
	}

	var budget float64
	if b := flagValue(args, "--budget", ""); b != "" {
		v, e := strconv.ParseFloat(strings.TrimSpace(b), 64)
		if e != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
			fatal(fmt.Errorf("--budget must be a positive number (got %q)", b))
		}
		budget = v
	}
	dryRun := hasFlag(args, "--dry-run")

	if dryRun {
		fmt.Printf("DRY RUN — would run promptfoo (%s) over %d model(s), spend $0:\n", cfg, len(models))
		for _, m := range models {
			fmt.Printf("  %s\n", m)
		}
		if budget > 0 {
			fmt.Printf("budget cap: $%.2f (models run one at a time; stops before exceeding)\n", budget)
		}
		fmt.Println("\nRe-run without --dry-run to execute.")
		return
	}

	base, err := routing.LoadScorecard()
	if err != nil {
		fatal(err)
	}
	sc := base
	var spent float64
	var stopped string
	var totalUpdated []routing.Score

	// When a budget is set, evaluate one model at a time so the sweep can stop
	// before blowing the cap. Without a budget, one run over all models.
	batches := [][]string{models}
	if budget > 0 {
		batches = nil
		for _, m := range models {
			batches = append(batches, []string{m})
		}
	}
	for _, batch := range batches {
		if budget > 0 && spent >= budget {
			stopped = fmt.Sprintf("budget $%.2f reached after $%.4f", budget, spent)
			break
		}
		results, rerr := runPromptfoo(promptfoo, cfg, batch)
		if rerr != nil {
			fatal(rerr)
		}
		updated, sum, ierr := routing.ImportPromptfoo(sc, results, time.Now())
		if ierr != nil {
			fatal(ierr)
		}
		if sum.Scored == 0 {
			fmt.Fprintf(os.Stderr, "warning: promptfoo returned no scored rows for %v (check the config/providers)\n", batch)
		}
		sc = updated
		spent += sum.SpentUSD
		totalUpdated = append(totalUpdated, sum.Updated...)
	}

	if hasFlag(args, "--json") {
		printJSON(map[string]any{"spent_usd": spent, "stopped": stopped, "updated": totalUpdated})
	} else {
		fmt.Printf("ran promptfoo, spent $%.4f\n", spent)
		if stopped != "" {
			fmt.Printf("HALTED: %s\n", stopped)
		}
		fmt.Println("\nmeasured scores (source=eval):")
		for _, s := range totalUpdated {
			fmt.Printf("  %-28s %-10s acc %.2f  $%.4f  %.0fms  (n=%d)\n",
				s.Model, s.TaskType, s.Accuracy, s.CostUSD, s.LatencyMsP50, s.N)
		}
		if budget > 0 && spent > budget {
			fmt.Printf("\nNOTE: spent $%.4f > budget $%.2f (the last model's matrix runs whole).\n", spent, budget)
		}
	}

	if hasFlag(args, "--save") {
		if err := sc.Save(); err != nil {
			fatal(err)
		}
		fmt.Printf("\nsaved scorecard -> %s\n(run `pi-stack route compile` to update routing.json)\n", routing.ScorecardPath())
	} else {
		fmt.Println("\n(preview only — pass --save to write these into the scorecard)")
	}
}

// runPromptfoo runs one `promptfoo eval` (optionally filtered to a set of model
// providers) and returns the raw results.json bytes.
func runPromptfoo(bin, cfg string, models []string) ([]byte, error) {
	tmp, err := os.CreateTemp("", "promptfoo-*.json")
	if err != nil {
		return nil, err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())

	cmdArgs := []string{"eval", "-c", cfg, "--output", tmp.Name(), "--no-cache"}
	if len(models) > 0 {
		// Filter to exactly these models. promptfoo matches --filter-providers
		// against the provider LABEL, which promptfooconfig.yaml sets to the model
		// id. Anchor so one model id can't match another as a substring.
		var alts []string
		for _, m := range models {
			alts = append(alts, regexp.QuoteMeta(m))
		}
		cmdArgs = append(cmdArgs, "--filter-providers", "^("+strings.Join(alts, "|")+")$")
	}
	cmd := exec.Command(bin, cmdArgs...)
	cmd.Stdout = os.Stderr // promptfoo's progress goes to the user; results are in the file
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		// promptfoo exits non-zero when tests FAIL, which is normal for an eval.
		// Only treat a missing/empty output file as a real error.
		if fi, statErr := os.Stat(tmp.Name()); statErr != nil || fi.Size() == 0 {
			return nil, fmt.Errorf("promptfoo produced no results: %w", err)
		}
	}
	return os.ReadFile(tmp.Name())
}

func evalsImport(args []string) {
	var file string
	for _, a := range args {
		if a != "" && a[0] != '-' {
			file = a
			break
		}
	}
	if file == "" {
		fatal(fmt.Errorf("evals import: missing results.json path"))
	}
	data, err := os.ReadFile(file)
	if err != nil {
		fatal(err)
	}
	base, err := routing.LoadScorecard()
	if err != nil {
		fatal(err)
	}
	updated, sum, err := routing.ImportPromptfoo(base, data, time.Now())
	if err != nil {
		fatal(err)
	}
	if hasFlag(args, "--json") {
		printJSON(sum)
	} else {
		fmt.Printf("imported %d rows: %d scored, %d skipped, %d errored, spent $%.4f\n",
			sum.Rows, sum.Scored, sum.Skipped, sum.Errored, sum.SpentUSD)
		for _, s := range sum.Updated {
			fmt.Printf("  %-28s %-10s acc %.2f  $%.4f  %.0fms  (n=%d)\n",
				s.Model, s.TaskType, s.Accuracy, s.CostUSD, s.LatencyMsP50, s.N)
		}
	}
	if hasFlag(args, "--save") {
		if err := updated.Save(); err != nil {
			fatal(err)
		}
		fmt.Printf("\nsaved scorecard -> %s\n(run `pi-stack route compile` to update routing.json)\n", routing.ScorecardPath())
	} else {
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
	cfg := evalsConfig(args)
	suitesDir := filepath.Join(filepath.Dir(cfg), "suites")
	entries, err := os.ReadDir(suitesDir)
	if err != nil {
		fatal(fmt.Errorf("no suites dir at %s: %w", suitesDir, err))
	}
	for _, e := range entries {
		if e.IsDir() {
			sub, _ := os.ReadDir(filepath.Join(suitesDir, e.Name()))
			for _, s := range sub {
				if strings.HasSuffix(s.Name(), ".yaml") {
					fmt.Printf("%s/%s\n", e.Name(), s.Name())
				}
			}
			continue
		}
		if strings.HasSuffix(e.Name(), ".yaml") {
			fmt.Println(e.Name())
		}
	}
}
