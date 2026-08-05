package inference

import (
	"fmt"

	"pix/host/config"
	"pix/host/routing"
)

// session.go turns an INTENT into the concrete model an interactive session
// will call. It lives here because the answer needs both facts this package
// keeps together: what the catalog routes to, and what this host can call.

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
		// An unknown intent must NOT fall back to the policy default: that hid a
		// bad --intent behind a Sonnet launch. run.go exits on an explicit typo.
		return "", fmt.Errorf("unknown intent %q (see `pix models show` for the intent list)", intent)
	}
	// Once bindings exist they are the availability authority.
	if cfg, cerr := config.Load(); cerr == nil && Configured(cfg) {
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

// ConfiguredSummary counts the callable models and names their distinct
// backends — the "is any inference wired here at all" question.
func ConfiguredSummary(cfg *config.Config) (int, []string) {
	var backends []string
	seen := map[string]bool{}
	bindings := Bindings(cfg)
	for _, b := range bindings {
		if !seen[b.Backend] {
			seen[b.Backend] = true
			backends = append(backends, b.Backend)
		}
	}
	return len(bindings), backends
}
