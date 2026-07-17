// pi-stack route / evals — thin launcher passthroughs to the sibling
// pi-stack-host binary, which owns the model router (registry + scorecard +
// resolver) and the eval harness. Kept here so a user drives the whole feature
// from the one `pi-stack` command. See docs/design/routing.md.

package main

import (
	"fmt"
	"os"
	"os/exec"

	"pi-stack/host/routing"
)

// resolveSessionModel turns a --intent into a concrete model id for the
// interactive session, using the same router (registry + scorecard + policy)
// the subagent crew uses. An unknown intent name is treated as an ad-hoc
// accuracy-objective intent on that task type, matching `route pick`.
func resolveSessionModel(intent string) (string, error) {
	reg, err := routing.LoadRegistry()
	if err != nil {
		return "", err
	}
	sc, err := routing.LoadScorecard()
	if err != nil {
		return "", err
	}
	pol, err := routing.LoadPolicy()
	if err != nil {
		return "", err
	}
	it, ok := pol.Intent(intent)
	if !ok {
		it = routing.Intent{Name: intent, TaskType: intent, Objective: "accuracy"}
	}
	d := routing.Resolve(reg, sc, pol, it)
	if d.Model == "" {
		return "", fmt.Errorf("router returned no model")
	}
	return d.Model, nil
}

// execHost runs `pi-stack-host <verb> <args...>` with inherited stdio and
// propagates the exit code. Shared by the route + evals passthroughs.
func execHost(verb string, argv []string) {
	bin, err := findHostBinary()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack %s: %v\n", verb, err)
		os.Exit(1)
	}
	cmd := exec.Command(bin, append([]string{verb}, argv...)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			os.Exit(exit.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "pi-stack %s: exec %s: %v\n", verb, bin, err)
		os.Exit(1)
	}
}

func runRoute(argv []string) {
	if len(argv) > 0 && (argv[0] == "-h" || argv[0] == "--help") {
		fmt.Print(routeUsage)
		return
	}
	execHost("route", argv)
}

func runEvals(argv []string) {
	if len(argv) > 0 && (argv[0] == "-h" || argv[0] == "--help") {
		fmt.Print(evalsUsage)
		return
	}
	execHost("evals", argv)
}

const routeUsage = `usage: pi-stack route <command>

The model router: turn a declared INTENT (a hard cost/latency/accuracy
constraint) into a concrete model, from a registry of models + a measured
scorecard. Replaces hand-pinning a model on every agent.

commands:
  pick <intent> [--json]   resolve one intent to a model (+ rationale)
  compile [--out PATH]      write the intent->model map (routing.json).
                            Use --out ./routing.json to update the baked file,
                            then ` + "`make load`" + ` to bake it into the image.
  show [--json]             registry + scorecard + resolved table
  models [--json]           list the model registry

Add a model: one entry in ~/.pi-stack/routing/models.json, then
` + "`pi-stack evals run --models <id>`" + ` and ` + "`pi-stack route compile`" + `.
`

const evalsUsage = `usage: pi-stack evals <command>

The accuracy eval harness: run a suite of cases across candidate models, score
each mechanically, record real cost + latency, and write the measured scores
into the router's scorecard so it stops guessing.

commands:
  run [--config P] [--models a,b] [--budget USD] [--dry-run] [--save] [--json]
  import FILE [--save] [--json]   fold a promptfoo results.json into the scorecard
  show [--json]                   the current scorecard
  ls   [--config P]               list the suites/cases

A real sweep calls each model on each case and COSTS MONEY — run it by hand on a
new-model release (use --budget to cap, --dry-run to preview), then
` + "`pi-stack route compile`" + `. Note --budget is advisory: models run one at a
time and the sweep stops before a model that would exceed the cap, but the last
model's own matrix runs whole.
`
