package reset

import "pix/host/config"

// defaultCfg is a minimal Config carrying the fields these tests read.
func defaultCfg() *config.Config {
	return &config.Config{Services: []string{"memory"}}
}
