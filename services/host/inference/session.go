package inference

import (
	"pix/host/config"
)

// session.go's remaining fact: how many callable models this host has bound
// and which backends they come from (the "is any inference wired here at
// all" question, no routing involved).

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
