package main

// models_cmd.go is `pix models`: the flag, its help text and its validation are
// ONE struct field, and nothing here calls os.Exit or writes to os.Stdout — every
// subcommand runs against a *cli.Deps, so its output and its exit code are
// ordinary assertions instead of a re-exec.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"pix/host/cli"
	"pix/host/inference"
	"pix/host/launcher"
	"pix/host/workflow/models"
)

// modelsDescription is the prose kong puts above the generated command list. A
// FUNC, not a const, so the override path it prints is the REAL resolved one
// and never the repo's embedded default, which means nothing to a consumer.
func modelsDescription() string {
	return `Which models pix can use, and which are wired up.

Bare ` + "`pix models`" + ` describes THIS HOST: the models machine config and the
selected environment declare, and the backend each is bound through. Nothing
here picks a model for you — that is ` + "`pix run --model`" + `, [models].main, or a
pack's binding.

Add a model to the catalog: one entry in
  ` + inference.CatalogPath() + `
That file does not exist by default: absent means "use the catalog built into
this binary", so create it only to override.`
}

// ModelsCmd is the verb tree. Bare `pix models` is the status screen, so Status
// is kong's default command rather than a subcommand a user must know.
func (c *ModelsCmd) Help() string { return modelsDescription() }

type ModelsCmd struct {
	Status ModelsStatusCmd `cmd:"" default:"1" hidden:""`
	Add    ModelsAddCmd    `cmd:"" help:"Wire a provider in and prove it with a live request. (WRITES)"`
	// `ls`, `show`, `pick`, `route`/`compile` and the `--catalog` flag are GONE
	// with the router they described (Wave F). Every one of them existed to
	// explain a SCORED pick — wired/unwired taxonomies, the scorecard, the
	// resolved intent table, the rationale — and there is no pick anymore: a
	// model is chosen by name. What survives is the fact screen (bare
	// `pix models`) and wiring a provider in (`add`).
}

// ModelsStatusCmd is the read-only screen: no probe, no mutation.
type ModelsStatusCmd struct{}

func (c *ModelsStatusCmd) Run(d *cli.Deps) error {
	cfg, err := d.Config()
	if err != nil {
		return err
	}
	// shippedAgents is nil: this screen reports MODELS, not the agent roster,
	// and ResolveEnvironmentRoster never needs it for that.
	facts, err := models.ResolveEnvironmentRoster(cfg, nil)
	if err != nil {
		return err
	}
	if err := models.ValidateRoster(cfg, facts); err != nil {
		return cli.UsageError{Err: err}
	}
	renderModelsStatus(cfg, facts, d.Out)
	return nil
}

// ModelsAddCmd wires a provider end to end. `enum` does real work: the provider
// list a user is offered cannot drift from the one that parses.
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
		return models.AddOllamaProvider(d, cfg, env, models.OllamaSelection{Local: c.Local, Cloud: c.Cloud})
	}
	return models.AddKeyedProvider(d, cfg, env, name)
}

// execHostBinary runs the sibling pix-host with argv, wired to this command's
// streams. It is the ONE place cmd/pix maps a host-binary failure: the child
// already said what was wrong, so its exit code travels as a SilentError rather
// than being re-reported here.
func execHostBinary(d *cli.Deps, argv []string) error {
	bin, err := launcher.FindHostBinary()
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, argv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, d.Out, d.Err
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return cli.SilentError{Code: exit.ExitCode()}
		}
		return fmt.Errorf("exec %s: %w", bin, err)
	}
	return nil
}
