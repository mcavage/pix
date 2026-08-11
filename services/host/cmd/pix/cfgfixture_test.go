package main

import "pix/host/config"

// testMCPServer stands in for an MCP server name the ACTIVE PACK declares. Core
// names no vendor any more, so tests that need a server name in the mcp list use
// an obviously-fictional one: pix resolves it from a pack manifest or not at
// all, and a name hardcoded in core would be exactly the wrong thing to assert.
const testMCPServer = "acme-docs"

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
