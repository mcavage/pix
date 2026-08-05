package config

// ServiceEnabled reports whether name is in the resolved service set.
func ServiceEnabled(cfg *Config, name string) bool {
	for _, s := range cfg.Services {
		if s == name {
			return true
		}
	}
	return false
}
