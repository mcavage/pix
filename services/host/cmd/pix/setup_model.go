// setup_model.go — setup's TTY-only current-model picker (PRD "Explicit
// Inference Setup" item 5: "Current-model picker writing [models].main to
// same-run scaffolded default env"). It is deliberately narrow:
//
//   - It never runs off a TTY: a script or pipe gets no prompt and no write.
//   - It writes ONLY into a default `pix.toml` this SAME setup run created
//     (defaultEnvCreated, threaded straight from provision.Result). An
//     existing default environment is never rewritten; the picker degrades
//     to a display of the model, the rule, and the exact file/key to edit
//     by hand (R9).
//   - The FIRST question is a plain-language yes/no: it names the provider
//     the fallback rule chose ("OpenAI is first among your configured
//     providers") and the model by its human label, never the internal
//     "configured provider default" source string a model choice is
//     stamped with elsewhere. Enter, "y", or anything that is not a "no"
//     keeps that fallback.
//   - Only on an explicit "no" does it list alternatives, and ONLY for that
//     one provider — never every configured provider at once. The list is
//     catalog id + human label; it never prints a context window (that is
//     an internal sizing fact, not a selection cue) and never repeats the
//     "configured provider default" wording.
//   - The default answer at every prompt is always "keep the shown
//     fallback": empty input, an out-of-range choice, or anything
//     unparsable leaves the scaffolded pix.toml exactly as
//     EnsureDefaultEnvironment wrote it.
//   - The listed choices are the catalog's own `available`, non-local
//     models for the ONE provider the fallback rule chose, in catalog
//     order — never scored, priced, or ranked (that machinery does not
//     exist here and this file must not grow it).
package main

import (
	"bufio"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"pix/host/cli"
	"pix/host/envinfo"
	"pix/host/hostenv"
	"pix/host/inference"
	"pix/host/pixhome"
	"pix/host/secret"
	"pix/host/workflow/provision"
)

// setupModelSelection is setup's model step (PRD §7 Phase I, step 9). It
// fires only on a TTY, only when this home configures at least one provider
// (secrets.env answerable and non-empty), and only lists models for THOSE
// providers. It never fails setup: every early return is a silent skip,
// because a picker is a convenience on top of resolveRunModel's own
// authoritative fallback, never a second source of truth for it.
func setupModelSelection(d *cli.Deps, home pixhome.Paths, env hostenv.Env, defaultEnvCreated bool) {
	if !d.Interactive {
		return
	}
	providers, state := secret.ConfiguredModelRefs(env)
	if state != secret.RefsAnswered || len(providers) == 0 {
		// Already reported by setupCredentials: either secrets.env could not
		// be read, or no provider is configured yet. Nothing new to add.
		return
	}
	// The fallback resolveRunModel would pick for a scaffolded environment
	// with NO [models].main set — the same authoritative function `pix run`
	// and `pix env show` use, never a parallel computation of it (R3).
	fallbackModel, _, err := resolveRunModel("", nil, env)
	if err != nil || fallbackModel == "" {
		return
	}
	sidecarPath := filepath.Join(home.EnvironmentDir(provision.DefaultEnvironmentName), "pix.toml")

	catalog, err := inference.LoadCatalog()
	if err != nil {
		// A corrupt override must never fail setup outright; resolveRunModel
		// already surfaced (or would surface) that same failure at launch.
		return
	}
	fallback, ok := catalog.Get(fallbackModel)
	if !ok {
		return
	}
	why := fmt.Sprintf("%s is first among your configured providers.", providerDisplayName(fallback.Provider))

	if !defaultEnvCreated {
		// R9: setup did not create the default environment this run, so its
		// pix.toml is user-owned and untouched. Display only.
		fmt.Fprintln(d.Out, "")
		fmt.Fprintf(d.Out, "Default model: %s The default environment already exists; it will use %s.\n", why, fallback.Label)
		fmt.Fprintf(d.Out, "  To change it, edit %s and set [models].main.\n", sidecarPath)
		return
	}

	fmt.Fprintln(d.Out, "")
	fmt.Fprintf(d.Out, "Default model: %s Use %s? [Y/n] ", why, fallback.Label)

	sc := bufio.NewScanner(d.In)
	if !sc.Scan() {
		return
	}
	answer := strings.ToLower(strings.TrimSpace(sc.Text()))
	if answer != "n" && answer != "no" {
		fmt.Fprintf(d.Out, "using %s (%s).\n", fallback.Label, fallbackModel)
		return
	}

	var choices []inference.Model
	for _, m := range catalog.Models {
		if m.Available && !m.Local && m.Provider == fallback.Provider {
			choices = append(choices, m)
		}
	}
	if len(choices) == 0 {
		return
	}

	fmt.Fprintf(d.Out, "%s models:\n", providerDisplayName(fallback.Provider))
	for i, m := range choices {
		fmt.Fprintf(d.Out, "  %d) %-32s %s\n", i+1, m.ID, m.Label)
	}
	fmt.Fprintf(d.Out, "Pick a number, or press Enter to keep %s: ", fallback.Label)

	if !sc.Scan() {
		return
	}
	line := strings.TrimSpace(sc.Text())
	if line == "" {
		fmt.Fprintf(d.Out, "keeping %s.\n", fallback.Label)
		return
	}
	idx, err := strconv.Atoi(line)
	if err != nil || idx < 1 || idx > len(choices) {
		fmt.Fprintf(d.Out, "%q is not one of the listed choices; keeping %s.\n", line, fallback.Label)
		return
	}
	chosen := choices[idx-1]
	if chosen.ID == fallbackModel {
		fmt.Fprintf(d.Out, "%s is already the default; nothing to record.\n", chosen.Label)
		return
	}
	if err := writeScaffoldedModelMain(env, sidecarPath, chosen.ID); err != nil {
		fmt.Fprintf(d.Out, "pix setup: could not record the model choice: %v\n", err)
		return
	}
	fmt.Fprintf(d.Out, "recorded [models].main = %q in %s\n", chosen.ID, sidecarPath)
}

// providerDisplayName renders a catalog provider key in the plain language
// the picker's prompts use, never the internal "configured provider
// default" source string resolveRunModel stamps a model choice with.
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
		return fmt.Errorf("read %s: %w", path, err)
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
		return fmt.Errorf("%s has no [models] section", path)
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
