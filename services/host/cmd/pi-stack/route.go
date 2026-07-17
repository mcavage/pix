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

Add a model: one entry in ~/.pi-stack/routing/models.json, then re-measure with
` + "`make evals ARGS=\"run --models <id> --save\"`" + ` and ` + "`pi-stack route compile`" + `.
`
