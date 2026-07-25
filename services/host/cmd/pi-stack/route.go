// pi-stack route — a thin launcher passthrough to the sibling pi-stack-host
// binary, which owns the model router (registry + scorecard + resolver). Kept
// here so a user drives the whole feature from the one `pi-stack` command. See
// docs/design/routing.md.

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
// propagates the exit code. Used by the route passthrough.
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
		fmt.Print(routeUsage())
		return
	}
	execHost("route", argv)
}

// routeUsage is a func (not a const) so the override paths it prints are the
// REAL resolved paths (honoring $ROUTING_DIR / $XDG_DATA_HOME), never a
// hardcoded guess — and never the repo's embedded default source, which only
// exists in a pi-stack checkout and means nothing on a consumer's machine.
func routeUsage() string {
	return `usage: pi-stack route <command>

The model router: turn a declared INTENT (a hard cost/latency/accuracy
constraint) into a concrete model, from a registry of models + a measured
scorecard. Replaces hand-pinning a model on every agent.

commands:
  pick <intent> [--json]   resolve one intent to a model (+ rationale)
  compile [--out PATH]      write the intent->model map (routing.json), read
                            by the sandbox. With no --out it writes into the
                            override dir below; --out PATH targets a specific
                            file (a pi-stack checkout uses --out ./routing.json,
                            then ` + "`make load`" + ` to bake it into the image — maintainer-only).
  show [--json]             registry + scorecard + resolved table
  models [--json]           list the model registry

Add a model: one entry in ` + routing.ModelsPath() + `.
Hand-edit its scores into ` + routing.ScorecardPath() + `, then run
` + "`pi-stack route compile`" + `.
(Maintaining pi-stack itself, not a personal override? The shipped defaults
live in services/host/routing/defaults/*.json in the pi-stack repo checkout.)
`
}
