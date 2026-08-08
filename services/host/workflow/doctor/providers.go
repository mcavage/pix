package doctor

// providers.go is what is LEFT of the readiness providers group: one
// config-only question doctor's key probe cannot answer, because it is about a
// binding's provenance rather than a key's presence.

import (
	"sort"

	"pix/host/config"
	"pix/host/inference"
)

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
		if b.Verified && b.VerifiedBy != config.VerifiedByProbe && b.Source == "" && inference.OllamaBindingDriver(cfg, b) {
			out = append(out, b.Model)
		}
	}
	sort.Strings(out)
	return out
}
