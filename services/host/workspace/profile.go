package workspace

import "pix/host/config"

// LoadResolvedConfig loads the config. Profiles were removed (the active PACK is the
// unit of context), so this is a thin wrapper over config.Load kept for call sites.
func LoadResolvedConfig() (*config.Config, string, error) {
	cfg, err := config.Load()
	return cfg, "", err
}
