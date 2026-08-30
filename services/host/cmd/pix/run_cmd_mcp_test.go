package main

import (
	"slices"
	"testing"

	"pix/host/config"
	"pix/host/workflow/launch"
)

// TestComposeStaticMCP_DedupesConfigAndFlagServersWithoutMutatingCfg proves
// composeStaticMCP folds the configured (cfg.MCP) and flag-requested (o.MCP)
// servers into one create-time --static-mcp set without mutating or aliasing
// either input's backing array. The pack-contributed third input this test
// used to prove was deleted with the pack system in the Pix v2 cutover
// (AC-16): there is no more pack to fold in.
func TestComposeStaticMCP_DedupesConfigAndFlagServersWithoutMutatingCfg(t *testing.T) {
	cfg := &config.Config{MCP: []string{"slack"}}
	cfgMCPBefore := append([]string(nil), cfg.MCP...)
	cfgMCPBacking := cfg.MCP[:1] // same backing array as cfg.MCP, to catch aliasing

	oMCP := []string{"notion"}
	oMCPBefore := append([]string(nil), oMCP...)

	got := composeStaticMCP(nil, cfg.MCP, oMCP)

	want := []string{"slack", "notion"}
	if !slices.Equal(got, want) {
		t.Fatalf("composeStaticMCP = %v, want %v", got, want)
	}

	// Neither input may have been mutated or aliased: cfg.MCP, the flag's own
	// slice, and the backing array cfg.MCP shares with itself must all still
	// hold their original values after the fold.
	if !slices.Equal(cfg.MCP, cfgMCPBefore) {
		t.Fatalf("cfg.MCP mutated: got %v, want %v", cfg.MCP, cfgMCPBefore)
	}
	if !slices.Equal(cfgMCPBacking, cfgMCPBefore) {
		t.Fatalf("cfg.MCP backing array aliased/overwritten: got %v, want %v", cfgMCPBacking, cfgMCPBefore)
	}
	if !slices.Equal(oMCP, oMCPBefore) {
		t.Fatalf("flag MCP slice mutated: got %v, want %v", oMCP, oMCPBefore)
	}

	// Mutating the returned slice must not reach back into any input's backing
	// array — the result is a fresh, unshared slice.
	if len(got) > 0 {
		got[0] = "clobbered"
	}
	if !slices.Equal(cfg.MCP, cfgMCPBefore) || !slices.Equal(oMCP, oMCPBefore) {
		t.Fatalf("mutating composeStaticMCP's result aliased back into an input")
	}

	// End-to-end: the actual sbx create argv must carry each server exactly once.
	o := launch.RunOpts{Workspace: "."}
	o.StaticMCP = composeStaticMCP(o.StaticMCP, cfg.MCP, oMCP)
	args := launch.BuildSbxArgs(cfg, o, "0.0.99")
	for _, name := range []string{"slack", "notion"} {
		if n := countStaticMCPFlag(args, name); n != 1 {
			t.Errorf("--static-mcp %s occurs %d times in create args, want exactly 1: %v", name, n, args)
		}
	}
}

func countStaticMCPFlag(args []string, name string) int {
	n := 0
	for i, a := range args {
		if a == "--static-mcp" && i+1 < len(args) && args[i+1] == name {
			n++
		}
	}
	return n
}
