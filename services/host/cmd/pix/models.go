// pix models — a thin launcher passthrough to the sibling pix-host
// binary, which owns the model router (registry + scorecard + resolver). Kept
// here so a user drives the whole feature from the one `pix` command. See
// docs/design/routing.md and docs/design/models-cli.md (the noun rename).
//
// `models` is the noun the owner asked for (docs/design/models-cli.md,
// Problem B): `ls`/`show`/`pick`/`route` are thin passthroughs to the
// unchanged `pix-host route` subcommand tree (see execHost below); bare
// `pix models` is a launcher-local, read-only status screen. `models add`
// and `models setup` (Problem A: wiring a second provider key in) are a
// DELIBERATE gap in this file — a later change adds them as two more cases
// in runModels' switch, on top of the tree built here.

package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"

	"pix/host/config"
	"pix/host/routing"
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
		// An unknown intent must NOT silently fabricate a task type and fall back to
		// the policy default (that hid a bad --intent/run_intent behind a Sonnet
		// launch). Error instead: run.go exits on an explicit --intent typo and
		// degrades to pi's default on a bad config-sourced run_intent; doctor renders
		// "does not resolve".
		return "", fmt.Errorf("unknown intent %q (see `pix models show` for the intent list)", intent)
	}
	// Once backend bindings exist they are the availability authority. The
	// shipped catalog alone never proves that a model is callable.
	var binding *routing.Binding
	if cfg, cerr := config.Load(); cerr == nil && len(cfg.Inference.Models) > 0 {
		bindings := routingBindings(cfg)
		reg = routing.RegistryForBindings(reg, bindings, "")
		d := routing.Resolve(reg, sc, pol, it)
		for _, b := range bindings {
			if b.Available && b.Model == d.Model {
				bb := b
				binding = &bb
				break
			}
		}
		if binding == nil {
			return "", fmt.Errorf("intent %q has no callable model binding", intent)
		}
		return boundRuntimeID(*binding), nil
	}
	d := routing.Resolve(reg, sc, pol, it)
	if d.Model == "" {
		return "", fmt.Errorf("router returned no model")
	}
	return d.Model, nil
}

// execHost runs `pix-host <verb> <args...>` with inherited stdio and
// propagates the exit code. Used by the models passthrough.
func execHost(verb string, argv []string) { execHostAs("models", verb, argv) }

// execHostAs is execHost with the launcher-facing verb named separately from
// the host one. They differ for every `pix models` subcommand, since the host
// tree is still spelled `pix-host route`: without this split, a missing
// pix-host made `pix models ls` report itself as `pix route:` — resurrecting
// the retired noun in user-facing output, where the source guard cannot see it
// because the string is assembled by a format verb.
func execHostAs(displayVerb, hostVerb string, argv []string) {
	bin, err := findHostBinary()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix %s: %v\n", displayVerb, err)
		os.Exit(1)
	}
	cmd := exec.Command(bin, append([]string{hostVerb}, argv...)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			os.Exit(exit.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "pix %s: exec %s: %v\n", displayVerb, bin, err)
		os.Exit(1)
	}
}

// runModels dispatches the `pix models` verb tree (docs/design/models-cli.md).
// `ls`/`show`/`pick`/`route` (plus the undocumented `compile` alias) are thin
// passthroughs to the unchanged `pix-host route` subcommand tree; bare
// `pix models` is the launcher-local read-only status screen.
func runModels(argv []string) {
	if len(argv) == 0 {
		runModelsStatus()
		return
	}
	if argv[0] == "-h" || argv[0] == "--help" {
		fmt.Print(modelsUsage())
		return
	}
	switch argv[0] {
	case "ls":
		execHost("route", append([]string{"models"}, argv[1:]...))
	case "show":
		execHost("route", append([]string{"show"}, argv[1:]...))
	case "pick":
		execHost("route", append([]string{"pick"}, argv[1:]...))
	case "route", "compile":
		// `models compile` is an undocumented alias for `models route`, kept
		// because that spelling is muscle memory in skills/model-refresh/SKILL.md,
		// extensions/subagents.ts, and the Makefile. Both map to the same
		// `pix-host route compile`.
		execHost("route", append([]string{"compile"}, argv[1:]...))
	// EXTENSION POINT (later unit, docs/design/models-cli.md "reconcile seam"):
	// case "add":   runModelsAdd(argv[1:])
	// case "setup": runModelsSetup(argv[1:])
	// Both are launcher-local (no execHost) and land here as a small additive
	// change once reconcileDirectInference exists; nothing above needs to move.
	default:
		// Usage goes to stderr on the error path, matching every sibling verb
		// (task.go, agent.go, state.go, config.go), so `pix models tpyo --json`
		// cannot put usage prose on a caller's stdout.
		fmt.Fprintf(os.Stderr, "pix models: unknown subcommand %q\n\n", argv[0])
		fmt.Fprint(os.Stderr, modelsUsage())
		os.Exit(2)
	}
}

// runModelsStatus renders the bare `pix models` screen: read-only, no probe,
// no mutation. It answers the discovery half of Problem A in
// docs/design/models-cli.md — the noun a user reaches for when they want to
// know what pix can use has somewhere to look.
func runModelsStatus() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix models: %v\n", err)
		os.Exit(1)
	}
	renderModelsStatus(cfg, os.Stdout)
}

// modelsBackendRow is one line of the bare status screen's Backends block: a
// backend name plus how many of its bindings are callable vs. live-verified.
type modelsBackendRow struct {
	name     string
	driver   string
	keyEnv   string
	total    int
	verified int
}

// modelsBackendRows groups cfg.Inference.Models by backend, counting only
// bindings inferenceBindingAllowed lets through (topology + roster), sorted by
// backend name for a stable render.
func modelsBackendRows(cfg *config.Config) []modelsBackendRow {
	rows := map[string]*modelsBackendRow{}
	var order []string
	for _, b := range cfg.Inference.Models {
		if !inferenceBindingAllowed(cfg, b) {
			continue
		}
		r, ok := rows[b.Backend]
		if !ok {
			backend := cfg.Inference.Backends[b.Backend]
			r = &modelsBackendRow{name: b.Backend, driver: backend.Driver, keyEnv: backend.KeyEnv}
			rows[b.Backend] = r
			order = append(order, b.Backend)
		}
		r.total++
		if b.Verified {
			r.verified++
		}
	}
	sort.Strings(order)
	out := make([]modelsBackendRow, 0, len(order))
	for _, name := range order {
		out = append(out, *rows[name])
	}
	return out
}

// modelsRuntimeLabel names the current inference topology in the same terms
// doctor/setup already use: an exclusive pack, direct provider keys (the
// default), a local Ollama backend, or a custom gateway.
func modelsRuntimeLabel(cfg *config.Config) string {
	if cfg.Inference.ExclusiveSource != "" {
		return "pack (" + cfg.Inference.ExclusiveSource + ")"
	}
	if len(cfg.Inference.Backends) == 0 {
		return "not configured yet"
	}
	if inferenceNeedsOnePassword(cfg) {
		return "direct provider keys (1Password)"
	}
	if b, ok := cfg.Inference.Backends["ollama"]; ok && b.Driver == "ollama" {
		return "local Ollama"
	}
	return "custom gateway"
}

// modelsRosterLine renders cfg.Inference.AllowedModels, or the "no personal
// restriction" state when it is empty (every callable model is available).
func modelsRosterLine(cfg *config.Config) string {
	if len(cfg.Inference.AllowedModels) == 0 {
		return "(no restriction \u2014 every callable model is available)"
	}
	return fmt.Sprintf("%s   (%d available)", strings.Join(cfg.Inference.AllowedModels, ", "), len(cfg.Inference.AllowedModels))
}

// modelsSessionLine renders the top-level session's resolved model, matching
// runIntentKeyCheck's own read of cfg.RunIntent (doctor_providers.go) so the
// two never disagree about what the session would launch.
func modelsSessionLine(cfg *config.Config) string {
	intent := config.DefaultRunIntent
	if cfg != nil && strings.TrimSpace(cfg.RunIntent) != "" {
		intent = strings.TrimSpace(cfg.RunIntent)
	}
	if strings.EqualFold(intent, "none") || strings.EqualFold(intent, "off") {
		return fmt.Sprintf("run_intent=%s -> pi's own default model", intent)
	}
	model, err := resolveSessionModel(intent)
	if err != nil || model == "" {
		return fmt.Sprintf("run_intent=%s -> does not resolve (see `pix doctor`)", intent)
	}
	return fmt.Sprintf("run_intent=%s -> %s", intent, model)
}

// renderModelsStatus writes the bare `pix models` screen to out. Read-only:
// every value it prints already lives in cfg or the router's own resolve —
// no network probe, no write.
func renderModelsStatus(cfg *config.Config, out io.Writer) {
	fmt.Fprintf(out, "Inference                                        config: %s\n\n", config.Path())
	fmt.Fprintf(out, "Runtime      %s\n", modelsRuntimeLabel(cfg))
	rows := modelsBackendRows(cfg)
	if len(rows) == 0 {
		fmt.Fprintln(out, "Backends     (none configured yet)")
	} else {
		for i, r := range rows {
			label := "Backends"
			if i > 0 {
				label = ""
			}
			fmt.Fprintf(out, "%-12s %-10s %-8s %-19s %d model(s), %d verified\n", label, r.name, r.driver, r.keyEnv, r.total, r.verified)
		}
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Roster       %s\n", modelsRosterLine(cfg))
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Session      %s\n", modelsSessionLine(cfg))
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Next:  pix models show           the full registry + resolved intents")
	fmt.Fprintln(out, "       pix models route          rewrite routing.json (the only one here that writes)")
	// EXTENSION POINT (later unit): a provider key with no callable binding
	// (cross-referencing hostModeProviderKeys against the rows above) belongs
	// here as a grounded "! X is set but has no model bindings" warning plus a
	// `pix models add <provider>` next step, once that command exists (see
	// docs/design/models-cli.md, "pix models (bare)" and the doctor gap check).
}

// modelsUsage is a func (not a const) so the override paths it prints are the
// REAL resolved paths (honoring $ROUTING_DIR / $XDG_DATA_HOME), never a
// hardcoded guess — and never the repo's embedded default source, which only
// exists in a pix checkout and means nothing on a consumer's machine.
func modelsUsage() string {
	return `usage: pix models <command>

Which models pix can use, and which are wired up. Bare ` + "`pix models`" + ` is a
read-only status screen (runtime, bound providers, roster, resolved session
model). ` + "`ls`" + `/` + "`show`" + `/` + "`pick`" + ` are read-only; ` + "`route`" + ` is the one
command here that writes.

commands:
  ls [--json]              list the model registry
  show [--json]             registry + scorecard + resolved intent table
  pick <intent> [--json]   resolve one intent to a model (+ rationale)
  route [--out PATH]        resolve every intent and write the intent->model map
                            (routing.json), read by the sandbox. With no --out it
                            writes into the override dir below; --out PATH targets
                            a specific file (a pix checkout uses --out ./routing.json,
                            then ` + "`make load`" + ` to bake it into the image — maintainer-only).   (writes)

Add a model: one entry in ` + routing.ModelsPath() + `.
Hand-edit its scores into ` + routing.ScorecardPath() + `, then run
` + "`pix models route`" + `.
(Maintaining pix itself, not a personal override? The shipped defaults
live in services/host/routing/defaults/*.json in the pix repo checkout.)
`
}
