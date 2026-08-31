package main

import "pix/host/config"

// defaultCfg is a minimal Config carrying the fields these tests read. A copy
// of doctor's: it is test-only, so exporting it would put scaffolding in that
// package's public API. There is no services/mcp/pack list to seed any more —
// config.toml declares none of those in v2.
func defaultCfg() *config.Config {
	return &config.Config{
		MemoryWatcherModel: "gemma4",
		MemoryEmbedModel:   "nomic-embed-text",
	}
}
