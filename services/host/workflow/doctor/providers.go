package doctor

import (
	"pix/host/cli"
	"pix/host/readiness"
	"pix/host/readiness/axis"
	"sort"

	"pix/host/config"
	"pix/host/routing"
)

// SecretCheck reports whether a provider secret is set. When sbx is
// unreachable (e.g. inside the sandbox) it emits a TODO rather than a false OK.
// Kept for hoststate.go/onboard.go's per-provider booleans (a DIFFERENT need
// from doctor's readiness axes below: they just want a plain ok/not-ok per
// key, not the core-vs-informational split doctor renders).
func SecretCheck(label, key, sbxOut string, sbxOK bool) readiness.Check {
	cmd := "sbx secret set -g " + key
	if !sbxOK {
		return readiness.Check{Label: label, Verdict: readiness.VerdictTodo, Detail: "sbx unavailable here (set on the host)", Todo: cmd}
	}
	if cli.GrepWord(sbxOut, key) {
		return readiness.Check{Label: label, Verdict: readiness.VerdictReady, Detail: "set"}
	}
	return readiness.Check{Label: label, Verdict: readiness.VerdictTodo, Detail: "not set", Todo: cmd}
}

// OllamaCloudCandidates returns bound non-local ollama models: the set whose
// entitlement only a real call can establish.
func OllamaCloudCandidates(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	reg, err := routing.LoadRegistry()
	if err != nil {
		return nil
	}
	var out []string
	for _, b := range cfg.Inference.Models {
		if !b.Available || b.Source != "" || !axis.OllamaBindingDriver(cfg, b) {
			continue
		}
		if m, ok := reg.Get(b.Model); ok && !m.Local {
			out = append(out, b.Model)
		}
	}
	sort.Strings(out)
	return out
}

// LegacyVerifiedOllamaBindings returns bindings that claim Verified without
// naming a probe. Only a PRE-UPGRADE config can produce this shape: everything
// this codebase promotes writes VerifiedBy="probe" in the same assignment, and
// everything it demotes clears both. So the row fires exactly once and clears
// on the next setup whether that setup promotes or demotes the binding.
func LegacyVerifiedOllamaBindings(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	var out []string
	for _, b := range cfg.Inference.Models {
		if b.Verified && b.VerifiedBy != config.VerifiedByProbe && b.Source == "" && axis.OllamaBindingDriver(cfg, b) {
			out = append(out, b.Model)
		}
	}
	sort.Strings(out)
	return out
}
