package workspace

import "pix/host/config"

// LoadResolvedConfig loads the config. Profiles were removed (the active PACK is
// now the unit of context, see docs/design/packs.md), so there is nothing to
// resolve — this is a thin wrapper over config.Load kept so the many call sites
// stay unchanged. The second return (formerly the active profile name) is always
// "" and callers ignore it.
func LoadResolvedConfig() (*config.Config, string, error) {
	cfg, err := config.Load()
	return cfg, "", err
}
