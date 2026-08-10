package config

import "strings"

// ServiceEnabled reports whether name is in the resolved service set — the SAME
// resolution `serve` performs, which is the whole point of it living here.
//
// An EMPTY (or whitespace-only) `services` means ALL services, not none. That
// is serve's rule (`resolveServices`: "an empty result means all", and
// `enabledSvc`: `len(want) == 0 || want[name]`), and reading it as "none" made
// this function disagree with the daemon about the same host: on a config with
// no `services` key, `serve` starts memory and doctor called memory OPTIONAL,
// so a memory unit that was genuinely down could not fail readiness. A verdict
// about a host that contradicts what the host actually runs is the same defect
// class as the providers row that demanded a key nothing read.
func ServiceEnabled(cfg *Config, name string) bool {
	if cfg == nil {
		return false
	}
	any := false
	for _, s := range cfg.Services {
		if strings.TrimSpace(s) == "" {
			continue
		}
		any = true
		if s == name {
			return true
		}
	}
	return !any
}
