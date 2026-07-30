package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"pix/host/config"
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
	snapshots, cleanup, err := snapshotAcceptedPackSetup(env, p, wanted)
	if err != nil {
		return err
	}
	defer cleanup()
	for _, step := range p.Manifest.Setup {
		if !step.Required && !wanted[step.ID] {
			continue
		}
		path := snapshots[step.ID]
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

// snapshotAcceptedPackSetup copies every selected executable into a private
// launcher-owned directory, then fingerprints the complete host surface using
// the captured bytes and requires an exact accepted trust record. Checks,
// apply, and re-check all execute the same immutable snapshot path.
func snapshotAcceptedPackSetup(env shellEnv, p *packInfo, wanted map[string]bool) (map[string]string, func(), error) {
	paths := map[string]string{}
	cleanup := func() {}
	if p == nil {
		return paths, cleanup, nil
	}
	allBytes := map[string][]byte{}
	for _, step := range p.Manifest.Setup {
		data, err := readFileNoSymlink(filepath.Join(p.Root, step.Path))
		if err != nil {
			return nil, cleanup, fmt.Errorf("setup hook %q could not be snapshotted safely: %w", step.ID, err)
		}
		allBytes[step.ID] = data
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, cleanup, fmt.Errorf("loading config for setup trust verification: %w", err)
	}
	bom := computeHostBoM(p, cfg.GogAccount, localMCPClassifier(env, hostBinaryResolver))
	fp, _, err := computeHostExecFingerprintWithSetup(p.Root, bom, allBytes)
	if err != nil {
		return nil, cleanup, err
	}
	err = withPackTrustLock(func() error {
		store, err := loadPackTrustStore()
		if err != nil {
			return fmt.Errorf("pack trust state unreadable: %w", err)
		}
		if got, ok := store.acceptedFingerprint(store.trustKey(p.Root)); !ok || got != fp {
			return fmt.Errorf("pack %s setup hooks are not accepted (or changed since acceptance) — run `pix pack use %s` to review them", p.Manifest.Name, p.Root)
		}
		return nil
	})
	if err != nil {
		return nil, cleanup, err
	}
	state, err := config.StateDir()
	if err != nil {
		return nil, cleanup, err
	}
	base := filepath.Join(state, "pack-setup-snapshots")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, cleanup, err
	}
	dir, err := os.MkdirTemp(base, "run-")
	if err != nil {
		return nil, cleanup, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	for _, step := range p.Manifest.Setup {
		if !step.Required && !wanted[step.ID] {
			continue
		}
		path := filepath.Join(dir, step.ID)
		if err := os.WriteFile(path, allBytes[step.ID], 0o500); err != nil {
			cleanup()
			return nil, func() {}, err
		}
		paths[step.ID] = path
	}
	return paths, cleanup, nil
}

// planPackSetupRequests assigns each optional --with id to the one pack that
// declares it, before any hook runs. Unknown and ambiguous ids fail without
// partially applying required hooks from an earlier pack.
func planPackSetupRequests(roots, requested []string) (map[string][]string, error) {
	plan := map[string][]string{}
	owners := map[string][]string{}
	seenRoots := map[string]bool{}
	for _, root := range roots {
		key := canonicalizePackRoot(root)
		if seenRoots[key] {
			continue
		}
		seenRoots[key] = true
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
			return nil, fmt.Errorf("setup hook %q is declared by multiple active packs (%s); hook IDs must be unique across a composed stack", id, strings.Join(matches, ", "))
		}
	}
	return plan, nil
}

func packSetupCheck(env shellEnv, path string, args []string) bool {
	_, timedOut, err := probeRun(env, path, args...)
	return !timedOut && err == nil
}
