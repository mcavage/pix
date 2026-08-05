package inference

import (
	"fmt"

	"pix/host/config"
	"pix/host/routing"
)

// session.go turns an INTENT into the concrete model an interactive session
// (or a verb that has to name one) will actually call. It lives here, not in
// routing, because the answer is the two facts this package exists to keep
// together: what the catalog routes to, and what this host can call.

// ResolveSessionModel turns a --intent into a concrete model id, using the same
// router (registry + scorecard + policy) the subagent crew uses.
func ResolveSessionModel(intent string) (string, error) {
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
		// An unknown intent must NOT silently fabricate a task type and fall back
		// to the policy default (that hid a bad --intent/run_intent behind a
		// Sonnet launch). Error instead: run.go exits on an explicit --intent typo
		// and degrades to pi's default on a bad config-sourced run_intent.
		return "", fmt.Errorf("unknown intent %q (see `pix models show` for the intent list)", intent)
	}
	// Once backend bindings exist they are the availability authority. The
	// shipped catalog alone never proves that a model is callable.
	if cfg, cerr := config.Load(); cerr == nil && len(cfg.Inference.Models) > 0 {
		bindings := Bindings(cfg)
		d := routing.Resolve(routing.RegistryForBindings(reg, bindings, ""), sc, pol, it)
		for _, b := range bindings {
			if b.Available && b.Model == d.Model {
				return RuntimeID(b), nil
			}
		}
		return "", fmt.Errorf("intent %q has no callable model binding", intent)
	}
	d := routing.Resolve(reg, sc, pol, it)
	if d.Model == "" {
		return "", fmt.Errorf("router returned no model")
	}
	return d.Model, nil
}

// ConfiguredSummary counts the models this host can actually call and names the
// distinct backends they run through — the "is any inference wired here at all"
// question setup and the models workflow ask before they offer to wire one.
func ConfiguredSummary(cfg *config.Config) (int, []string) {
	if cfg == nil {
		return 0, nil
	}
	seen := map[string]bool{}
	var backends []string
	count := 0
	for _, b := range cfg.Inference.Models {
		if !Callable(cfg, b) {
			continue
		}
		count++
		if !seen[b.Backend] {
			seen[b.Backend] = true
			backends = append(backends, b.Backend)
		}
	}
	return count, backends
}
