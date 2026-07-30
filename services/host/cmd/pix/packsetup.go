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
func runPackSetup(env shellEnv, out io.Writer, root string, requested []string, interactive bool) error {
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
	}
	for id := range wanted {
		if id == "" || !known[id] {
			return fmt.Errorf("pack has no setup hook %q", id)
		}
	}
	for _, step := range p.Manifest.Setup {
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
		if !interactive {
			return fmt.Errorf("pack setup %s is not ready and may require interactive authorization; re-run without --yes/--non-interactive", step.ID)
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
	return nil
}

// planPackSetupRequests assigns each optional --with id to the one pack that
// declares it, before any hook runs. Unknown and ambiguous ids fail without
// partially applying required hooks from an earlier pack.
func planPackSetupRequests(roots, requested []string) (map[string][]string, error) {
	plan := map[string][]string{}
	owners := map[string][]string{}
	for _, root := range roots {
		p, err := loadPack(root)
		if err != nil {
			return nil, err
		}
		for _, step := range p.Manifest.Setup {
			owners[step.ID] = append(owners[step.ID], root)
		}
	}
	for _, raw := range requested {
		id := strings.TrimSpace(raw)
		matches := owners[id]
		switch len(matches) {
		case 0:
			return nil, fmt.Errorf("no active pack has setup hook %q", id)
		case 1:
			plan[matches[0]] = append(plan[matches[0]], id)
		default:
			return nil, fmt.Errorf("setup hook %q is declared by multiple active packs; rename it to make ownership unambiguous", id)
		}
	}
	return plan, nil
}

func packSetupCheck(env shellEnv, path string, args []string) bool {
	_, timedOut, err := probeRun(env, path, args...)
	return !timedOut && err == nil
}
