package axis

import "pix/host/config"

// defaultCfg is a minimal Config carrying the fields these tests read.
func defaultCfg() *config.Config {
	c := &config.Config{}
	c.Services = []string{"memory"}
	c.MemoryWatcherModel = "gemma4"
	c.MemoryEmbedModel = "nomic-embed-text"
	return c
}
