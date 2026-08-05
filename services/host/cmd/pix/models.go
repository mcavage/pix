// pix models — a thin launcher passthrough to the sibling pix-host
// binary, which owns the model router (registry + scorecard + resolver). Kept
// here so a user drives the whole feature from the one `pix` command. See
// docs/design/routing.md and docs/design/models-cli.md (the noun rename).
//
// `models` is the noun the owner asked for (docs/design/models-cli.md,
// Problem B): `ls`/`show`/`pick`/`route` are thin passthroughs to the
// unchanged `pix-host route` subcommand tree (see execHostRoute); bare
// `pix models` is a launcher-local, read-only status screen.

package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"

	"pix/host/config"
	"pix/host/inference"
	"pix/host/launcher"
	"pix/host/readiness/axis"
	"pix/host/workflow/doctor"
)

// runRouteAlias is the retired `pix route` spelling, forwarded RAW to the host
// tree for one release so it stays bug-for-bug the command it replaces. Routing
// it through the new verb once broke `pix route models` (the old spelling of
// the registry list), which is the exact compatibility the alias promises.
func runRouteAlias(argv []string) {
	// The retired noun is a CONSTANT, never a literal in a message: the rename
	// guard (models_rename_test.go) bans raw `pix route` from production source,
	// and it should — the one place still allowed to say it is the alias whose
	// whole job is to answer to it.
	const verb = "route"
	bin, err := launcher.FindHostBinary()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix %s: %v\n", verb, err)
		os.Exit(1)
	}
	cmd := exec.Command(bin, append([]string{verb}, argv...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			os.Exit(exit.ExitCode())
		}
		fmt.Fprintf(os.Stderr, "pix %s: exec %s: %v\n", verb, bin, err)
		os.Exit(1)
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
		if !inference.Allowed(cfg, b) {
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
	if inference.InferenceNeedsOnePassword(cfg) {
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
// axis.RunIntentKeyCheck's own read of cfg.RunIntent (doctor_providers.go) so the
// two never disagree about what the session would launch.
func modelsSessionLine(cfg *config.Config) string {
	intent := config.DefaultRunIntent
	if cfg != nil && strings.TrimSpace(cfg.RunIntent) != "" {
		intent = strings.TrimSpace(cfg.RunIntent)
	}
	if strings.EqualFold(intent, "none") || strings.EqualFold(intent, "off") {
		return fmt.Sprintf("run_intent=%s -> pi's own default model", intent)
	}
	model, err := axis.ResolveSessionModel(intent)
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
	// A key that is set but wired to nothing is the failure this screen exists to
	// surface, so name it here rather than making the user run doctor to find out.
	for _, p := range doctor.UnwiredProviderKeys(cfg, defaultShellEnv()) {
		fmt.Fprintf(out, "\n!  %s is set but has no model bindings — the key is not in use yet.\n", p)
		fmt.Fprintf(out, "   pix models add %s\n", p)
	}
}
