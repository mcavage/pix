package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/envinfo"
	"pix/host/hostenv"
	"pix/host/inference"
	"pix/host/pixhome"
	"pix/host/secret"
	"pix/host/workflow/launch"
	"pix/host/workspace"
)

// Every environment uses the same picker. Existing model declarations win;
// setup only fills a missing choice after the user selects it.
func setupModelSelection(d *cli.Deps, home pixhome.Paths, env hostenv.Env, name string) error {
	path := filepath.Join(home.EnvironmentDir(name), "pix.toml")
	sidecar, err := envinfo.ParseSidecar(path)
	if errors.Is(err, os.ErrNotExist) {
		sidecar = &envinfo.Sidecar{Schema: 1}
	} else if err != nil {
		return err
	}
	cfg, _, err := workspace.LoadResolvedConfig()
	if err != nil {
		return err
	}
	cfg, err = launch.EffectiveInferenceConfig(cfg, sidecar)
	if err != nil {
		return err
	}
	catalog, err := inference.LoadCatalog()
	if err != nil {
		return err
	}
	selected := sidecar.Models.Main
	if selected == "" {
		if !d.Interactive {
			return fmt.Errorf("choose a model by running pix setup --env %s in a terminal", name)
		}
		var choices []inference.Model
		if len(sidecar.Inference.Models) > 0 {
			for _, m := range sidecar.Inference.Models {
				label := m.ID
				if known, ok := catalog.Get(m.ID); ok {
					label = known.Label
				}
				choices = append(choices, inference.Model{ID: m.ID, Label: label})
			}
		} else {
			local, _ := installedOllamaChoices(catalog, env)
			choices = append(choices, local...)
			for _, provider := range []string{"openai", "anthropic", "google"} {
				id, err := inference.DefaultModelForProviders(catalog, []string{provider})
				if err != nil {
					continue
				}
				if model, ok := catalog.Get(id); ok {
					choices = append(choices, model)
				}
			}
		}
		if len(choices) == 0 {
			return fmt.Errorf("no models are available for %q", name)
		}
		fmt.Fprintln(d.Out, "Choose your model")
		renderModelChoices(d, choices)
		var picked int
		_, ok := d.Ask(cli.Question{Label: "Model number", Accept: func(v string) error {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > len(choices) {
				return fmt.Errorf("choose a number from 1 to %d", len(choices))
			}
			picked = n - 1
			return nil
		}})
		if !ok {
			return fmt.Errorf("setup paused; continue with pix setup --env %s", name)
		}
		selected = choices[picked].ID
		// Credentials first: cancelling their prompt must not save an unusable choice.
		if err := setupModelCredential(d, env, cfg, selected); err != nil {
			return err
		}
		if err := writeScaffoldedModelMain(env, path, selected); err != nil {
			return err
		}
	} else if err := setupModelCredential(d, env, cfg, selected); err != nil {
		return err
	}
	label := selected
	if model, ok := catalog.Get(selected); ok {
		label = model.Label
	}
	route := ""
	for _, binding := range cfg.Inference.Models {
		if binding.Model == selected && cfg.Inference.Backends[binding.Backend].Auth == "sbx-session" {
			route = " (environment AI gateway)"
			break
		}
	}
	fmt.Fprintf(d.Out, "Model for %q: %s%s\n", name, label, route)
	return nil
}

func setupModelCredential(d *cli.Deps, env hostenv.Env, cfg *config.Config, model string) error {
	if inference.KeylessModel(cfg, model) {
		return nil
	}
	provider, _, _ := strings.Cut(model, "/")
	refs, state := secret.ConfiguredModelRefs(env)
	if state != secret.RefsAnswered {
		return fmt.Errorf("could not read saved account references")
	}
	for _, configured := range refs {
		if configured == provider {
			return nil
		}
	}
	for _, key := range secret.ProviderKeyRefOrder {
		if key.Name != provider {
			continue
		}
		if !d.Interactive {
			return fmt.Errorf("connect %s with pix setup in a terminal", providerDisplayName(provider))
		}
		if !secret.OpInstalled(env) {
			return fmt.Errorf("install the 1Password CLI to connect %s, then run pix setup again", providerDisplayName(provider))
		}
		_, ok := d.Ask(cli.Question{
			Label:   providerDisplayName(provider) + " key in 1Password",
			Detail:  "Copy the secret reference from your API key's field in 1Password. Your key stays in 1Password.",
			Example: "op://Private/OpenAI/api-key",
			Accept:  func(ref string) error { return secret.WriteOpRefQuiet(env, key.EnvVar, secret.NormalizeOpRef(ref)) },
		})
		if !ok {
			return fmt.Errorf("account connection paused; run pix setup again to continue")
		}
		return nil
	}
	return nil // Environment-specific credentials are collected from its declarations.
}

func installedOllamaChoices(catalog *inference.Catalog, env hostenv.Env) (choices []inference.Model, uncataloged []string) {
	if env.System == nil {
		return nil, nil
	}
	st := inference.DetectOllama(env)
	if !st.Reachable {
		return nil, nil
	}
	cataloged := map[string]inference.Model{}
	for _, m := range catalog.Models {
		if m.Available && m.Provider == "ollama" {
			cataloged[inference.NormalizeOllamaTag(m.ID)] = m
		}
	}
	for _, tag := range st.ChatModels() {
		if m, ok := cataloged[inference.NormalizeOllamaTag(tag)]; ok {
			choices = append(choices, m)
			continue
		}
		uncataloged = append(uncataloged, tag)
	}
	return choices, uncataloged
}

func renderModelChoices(d *cli.Deps, choices []inference.Model) {
	for i, m := range choices {
		suffix := ""
		if m.Provider == "ollama" {
			suffix = "  · Ollama"
		}
		fmt.Fprintf(d.Out, "  %d. %s%s\n", i+1, m.Label, suffix)
	}
}

func providerDisplayName(provider string) string {
	switch provider {
	case "openai":
		return "OpenAI"
	case "anthropic":
		return "Anthropic"
	case "google":
		return "Google"
	case "ollama":
		return "Ollama"
	default:
		return provider
	}
}

// writeScaffoldedModelMain inserts `main = "<id>"` right after the
// `[models]` header of the pix.toml THIS SAME setup run scaffolded
// (EnsureDefaultEnvironment's own defaultSidecar() text, which declares
// [models] but leaves `main` commented out). It refuses outright if the
// file already declares an active `main = ` line — this function is a
// single, one-shot fill of a known-empty field, never a general-purpose
// pix.toml editor, and it must never clobber a value the same run's own
// prior write (or a hand edit made mid-run) already recorded.
//
// modelID always comes from the shipped catalog (never freehand input: the
// picker only ever offers an index into its own choices list), and
// inference.ValidateCatalog already requires every catalog id to be a
// fully-qualified, control-character-free provider/id string — so the
// inserted line is guaranteed well-formed TOML without a second parse
// round-trip. env.WriteFile itself is leaf-symlink-safe and atomic
// (sys.FS.WriteFile), so this is a single all-or-nothing write.
func writeScaffoldedModelMain(env hostenv.Env, path, modelID string) error {
	content, err := env.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("read %s: %w", path, err)
		}
		content = "schema = 1\n\n[models]\n"
	}
	for _, line := range strings.Split(content, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "main ") || strings.HasPrefix(t, "main=") {
			return fmt.Errorf("%s already declares [models].main; refusing to overwrite it", path)
		}
	}
	const marker = "[models]\n"
	at := strings.Index(content, marker)
	if at < 0 {
		content += "\n[models]\n"
		at = strings.Index(content, marker)
	}
	insertAt := at + len(marker)
	newLine := fmt.Sprintf("main = %q  # set by pix setup on %s\n", modelID, time.Now().UTC().Format(time.RFC3339))
	updated := content[:insertAt] + newLine + content[insertAt:]
	if err := env.WriteFile(path, []byte(updated), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if _, perr := envinfo.ParseSidecar(path); perr != nil {
		return fmt.Errorf("recorded model broke %s: %w", filepath.Base(path), perr)
	}
	return nil
}
