package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// runPackSetup runs a pack's required setup contributions after the
// pack has been adopted through the normal Tier-1 trust gate. Every step is
// resumable: its bounded check runs first, apply runs only when check fails,
// and the same check must pass afterward before Pix reports readiness.
func runPackSetup(env shellEnv, out io.Writer, root string, requested []string) error {
	p, err := loadPack(root)
	if err != nil {
		return err
	}
	wanted := map[string]bool{}
	for _, id := range requested {
		wanted[strings.TrimSpace(id)] = true
	}
	known := map[string]bool{}
	for _, step := range p.Manifest.Setup {
		known[step.ID] = true
		if !step.Required && !wanted[step.ID] {
			continue
		}
		path := filepath.Join(p.Root, step.Path)
		label := strings.TrimSpace(step.Description)
		if label == "" {
			label = step.ID
		}
		if packSetupCheck(env, path, step.CheckArgs) {
			fmt.Fprintf(out, "  ✓ %s: ready\n", label)
			continue
		}
		fmt.Fprintf(out, "\npack setup: %s\n", label)
		if env.runInteractive == nil {
			return fmt.Errorf("pack setup %s needs an interactive command runner", step.ID)
		}
		if err := env.runInteractive(path, step.ApplyArgs...); err != nil {
			return fmt.Errorf("pack setup %s failed: %w", step.ID, err)
		}
		if !packSetupCheck(env, path, step.CheckArgs) {
			return fmt.Errorf("pack setup %s did not pass its verification probe after apply", step.ID)
		}
		fmt.Fprintf(out, "  ✓ %s: verified\n", label)
	}
	for id := range wanted {
		if id == "" || !known[id] {
			return fmt.Errorf("pack has no setup hook %q", id)
		}
	}
	return nil
}

func packSetupCheck(env shellEnv, path string, args []string) bool {
	_, timedOut, err := probeRun(env, path, args...)
	return !timedOut && err == nil
}
