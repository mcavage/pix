package config

// ServiceEnabled reports whether name is in the resolved service set.
//
// It lived in doctor.go, which meant every package asking "is knowledge turned
// on" had to depend on the doctor workflow to find out. It is a question about
// the config file and belongs with the config file.
func ServiceEnabled(cfg *Config, name string) bool {
	for _, s := range cfg.Services {
		if s == name {
			return true
		}
	}
	return false
}
