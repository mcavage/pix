package env

import (
	"slices"

	"pix/host/config"
)

// Register records name -> path in cfg through the config-owned helper
// (config.AddEnvironment), returning the canonical absolute root actually
// stored. This package performs no canonicalization, name validation, or
// path expansion of its own — config already owns all three (E1.5) — and it
// never writes a byte to the environment directory itself: registration is
// a config.toml mutation only, and the caller (a future `pix env add`,
// E1.10) still owns Save().
func Register(cfg *config.Config, name, path string) (string, error) {
	return cfg.AddEnvironment(name, path)
}

// Unregister removes name's registration through the config-owned helper
// (config.RemoveEnvironment), reporting whether anything changed. It deletes
// only the [environments] entry (and the machine default, if name was it) —
// never the environment directory on disk. docs/design/environments.md §8.1:
// "forget NAME never deletes the environment directory: the source is
// untouched."
func Unregister(cfg *config.Config, name string) bool {
	return cfg.RemoveEnvironment(name)
}

// Root returns the canonical root currently registered under name, and
// whether name is registered at all. This is an EXACT lookup: no prefix or
// fuzzy match, matching docs/design/environments.md §8's "Names are exact.
// Only `add` accepts a path. There is no fuzzy or prefix action."
func Root(cfg *config.Config, name string) (string, bool) {
	root, ok := cfg.Environments[name]
	return root, ok
}

// Known returns every registered name, sorted, for a caller that needs a
// deterministic listing (an unknown-name error's "known:" line, `pix env
// ls`). Returned as a fresh slice: callers may not observe or mutate cfg's
// own map through it.
func Known(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Environments))
	for n := range cfg.Environments {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}
