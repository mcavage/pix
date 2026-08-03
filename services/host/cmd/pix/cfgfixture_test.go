package main

import "pix/host/config"

// defaultCfg is a minimal Config carrying the fields these tests read. A copy
// of doctor's: it is test-only, so exporting it would put scaffolding in that
// package's public API.
func defaultCfg() *config.Config {
	c := &config.Config{}
	c.Services = []string{"memory"}
	c.MemoryWatcherModel = "gemma4"
	c.MemoryEmbedModel = "nomic-embed-text"
	return c
}
