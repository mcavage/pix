package main

// models_cmd.go is `pix models` under the cli command contract
// (docs/design/rearchitecture.md, Phase 2). It is the first verb migrated and
// is meant to be read as the worked example for the other 33.
//
// What the shape buys, measured against the hand-rolled version it replaces:
//
//   - The flag, its help text and its validation are ONE struct field. Before,
//     `--local`/`--cloud` were a hand-written loop over argv that rejected
//     unknown flags itself, and the usage describing them was a separate blob
//     maintained by hand. They drifted — the provider list in the error message
//     omitted ollama for as long as ollama was unsupported, and stayed wrong
//     after it was added.
//   - Nothing here calls os.Exit or writes to os.Stdout. Every subcommand runs
//     against a *cli.Deps whose Out is a bytes.Buffer, so its output and its
//     exit code are ordinary assertions instead of a re-exec.

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"pix/host/cli"
	"pix/host/routing"
)

// modelsDescription is the prose kong puts above the generated command list.
// Per-command help lives on the fields; this says only what generated usage
// cannot infer.
// It is a FUNC, not a const, so the override paths it prints are the REAL
// resolved ones (honoring $ROUTING_DIR / $XDG_DATA_HOME) rather than a
// hardcoded guess, and never the repo's embedded default source — which only
// exists in a pix checkout and means nothing on a consumer's machine.
func modelsDescription() string {
	return `Which models pix can use, and which are wired up.

Every command here describes THIS HOST: the shipped catalog narrowed to the
models a probed backend binding makes callable. ls/show/pick/route also take
--catalog, which drops the host filter and describes the shipped catalog itself.

Add a model to the catalog: one entry in
  ` + routing.ModelsPath() + `
and its scores in
  ` + routing.ScorecardPath() + `
then ` + "`pix models route`" + `. Neither file exists until you create it — absent
means "use the defaults built into this binary", so create only the one you
want to override.
(Maintainer-only, not a personal override: the shipped defaults live in
services/host/routing/defaults/*.json in the pix repo checkout, and the image's
baked map is compiled with ` + "`--catalog --out ./routing.json`" + `.)`
}

// ModelsCmd is the verb tree. Bare `pix models` is the status screen, so Status
// is kong's default command rather than a subcommand a user must know.
type ModelsCmd struct {
	Status  ModelsStatusCmd `cmd:"" default:"1" hidden:""`
	Ls      ModelsLsCmd     `cmd:"" help:"One row per model: wired / unwired / retired."`
	Show    ModelsShowCmd   `cmd:"" help:"ls, plus the scorecard and the resolved intent table."`
	Pick    ModelsPickCmd   `cmd:"" help:"Resolve one intent to a model, with the rationale."`
	Route   ModelsRouteCmd  `cmd:"" help:"Resolve every intent and write the intent->model map. (WRITES)"`
	Add     ModelsAddCmd    `cmd:"" help:"Wire a provider in and prove it with a live request. (WRITES)"`
	Compile ModelsRouteCmd  `cmd:"" hidden:"" help:"Undocumented alias for route (muscle memory in skills + the Makefile)."`
}

// ModelsStatusCmd is the read-only screen: no probe, no mutation.
type ModelsStatusCmd struct{}

func (c *ModelsStatusCmd) Run(d *cli.Deps) error {
	cfg, err := d.Config()
	if err != nil {
		return err
	}
	renderModelsStatus(cfg, d.Out)
	return nil
}

// hostQuery is the flag pair every read-only router query shares. It is
// embedded rather than duplicated, but each subcommand is its OWN type: they
// forward to different host verbs, and a shared type could not tell which one
// kong had selected.
type hostQuery struct {
	JSON    bool `help:"Emit machine-readable JSON."`
	Catalog bool `help:"Describe the shipped catalog, not this host."`
}

func (q hostQuery) flags() []string {
	var out []string
	if q.JSON {
		out = append(out, "--json")
	}
	if q.Catalog {
		out = append(out, "--catalog")
	}
	return out
}

type ModelsLsCmd struct{ hostQuery }

func (c *ModelsLsCmd) Run(d *cli.Deps) error { return execHostRoute(d, "models", c.flags()) }

type ModelsShowCmd struct{ hostQuery }

func (c *ModelsShowCmd) Run(d *cli.Deps) error { return execHostRoute(d, "show", c.flags()) }

// ModelsPickCmd resolves one intent.
type ModelsPickCmd struct {
	hostQuery
	Intent string `arg:"" help:"Intent name (see 'pix models show')."`
}

func (c *ModelsPickCmd) Run(d *cli.Deps) error {
	return execHostRoute(d, "pick", append([]string{c.Intent}, c.flags()...))
}

// ModelsRouteCmd writes the compiled intent->model map.
type ModelsRouteCmd struct {
	Out     string `help:"Write to PATH instead of the routing override dir." placeholder:"PATH"`
	Catalog bool   `help:"Compile the host-independent map (maintainer-only: baking the image default)."`
}

func (c *ModelsRouteCmd) Run(d *cli.Deps) error {
	var argv []string
	if c.Out != "" {
		argv = append(argv, "--out", c.Out)
	}
	if c.Catalog {
		argv = append(argv, "--catalog")
	}
	return execHostRoute(d, "compile", argv)
}

// ModelsAddCmd wires a provider end to end.
//
// `enum` is doing real work here. The hand-rolled version validated the
// provider in its own if-chain and printed its own "want one of" list, which
// had to be kept in step with providerNames() by hand — and was not.
type ModelsAddCmd struct {
	Provider string `arg:"" enum:"anthropic,openai,google,gemini,ollama" help:"anthropic | openai | google | ollama"`
	Local    bool   `help:"Ollama only: models that run on this machine."`
	Cloud    bool   `help:"Ollama only: models on your ollama.com plan."`
}

func (c *ModelsAddCmd) Run(d *cli.Deps) error {
	name := strings.ToLower(strings.TrimSpace(c.Provider))
	if name == "gemini" {
		name = "google"
	}
	if (c.Local || c.Cloud) && name != "ollama" {
		return cli.Usagef("--local/--cloud only apply to ollama, not %q", c.Provider)
	}
	cfg, err := d.Config()
	if err != nil {
		return err
	}
	env := defaultShellEnv()
	if name == "ollama" {
		return addOllamaProvider(d, cfg, env, ollamaSelection{Local: c.Local, Cloud: c.Cloud})
	}
	return addKeyedProvider(d, cfg, env, name)
}

// execHostRoute forwards to the sibling pix-host binary, which owns the router.
// It is the one place that knows the host tree is still spelled `route`.
func execHostRoute(d *cli.Deps, verb string, argv []string) error {
	bin, err := findHostBinary()
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, append([]string{"route", verb}, argv...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, d.Out, d.Err
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			// The host already said what was wrong in its own words; repeating it
			// here would double-report one cause.
			return cli.SilentError{Code: exit.ExitCode()}
		}
		return fmt.Errorf("exec %s: %w", bin, err)
	}
	return nil
}
