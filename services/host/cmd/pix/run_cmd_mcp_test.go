package main

import (
	"slices"
	"testing"

	"pix/host/config"
	"pix/host/packinfo"
	"pix/host/workflow/launch"
)

// TestComposeStaticMCP_IncludesPackContributionWithoutMutatingCfg is the
// regression for the composition defect in runLaunch: the create-time
// --static-mcp line used to be built from cfg.MCP + o.MCP alone, discarding
// whatever a verified pack had already folded into o.StaticMCP via
// ApplyPackContribution. A pack-contributed server must survive that fold, and
// composeStaticMCP must never mutate or alias cfg.MCP, o.MCP, or the pack's own
// slice while doing it.
func TestComposeStaticMCP_IncludesPackContributionWithoutMutatingCfg(t *testing.T) {
	cfg := &config.Config{MCP: []string{"slack"}}
	cfgMCPBefore := append([]string(nil), cfg.MCP...)
	cfgMCPBacking := cfg.MCP[:1] // same backing array as cfg.MCP, to catch aliasing

	packMCPNames := []string{"notion"}
	packMCPNamesBefore := append([]string(nil), packMCPNames...)

	o := launch.RunOpts{Workspace: "."}
	o.ApplyPackContribution(packinfo.LaunchContribution{MCPNames: packMCPNames})
	if !slices.Equal(o.StaticMCP, []string{"notion"}) {
		t.Fatalf("pack contribution not folded in: o.StaticMCP = %v", o.StaticMCP)
	}

	got := composeStaticMCP(o.StaticMCP, cfg.MCP, o.MCP)

	// The pack's contribution must be present exactly once alongside the
	// configured server, in the merged create-time set.
	want := []string{"notion", "slack"}
	if !slices.Equal(got, want) {
		t.Fatalf("composeStaticMCP = %v, want %v", got, want)
	}

	// None of the inputs may have been mutated or aliased: cfg.MCP, the pack's
	// own MCPNames slice, and the backing array cfg.MCP shares with itself must
	// all still hold their original values after the fold.
	if !slices.Equal(cfg.MCP, cfgMCPBefore) {
		t.Fatalf("cfg.MCP mutated: got %v, want %v", cfg.MCP, cfgMCPBefore)
	}
	if !slices.Equal(cfgMCPBacking, cfgMCPBefore) {
		t.Fatalf("cfg.MCP backing array aliased/overwritten: got %v, want %v", cfgMCPBacking, cfgMCPBefore)
	}
	if !slices.Equal(packMCPNames, packMCPNamesBefore) {
		t.Fatalf("pack's MCPNames slice mutated: got %v, want %v", packMCPNames, packMCPNamesBefore)
	}

	// Mutating the returned slice must not reach back into any input's backing
	// array — the result is a fresh, unshared slice.
	if len(got) > 0 {
		got[0] = "clobbered"
	}
	if !slices.Equal(cfg.MCP, cfgMCPBefore) || !slices.Equal(packMCPNames, packMCPNamesBefore) {
		t.Fatalf("mutating composeStaticMCP's result aliased back into an input")
	}

	// End-to-end: the actual sbx create argv must carry the pack-contributed
	// server exactly once, same as any configured one.
	o.StaticMCP = composeStaticMCP(o.StaticMCP, cfg.MCP, o.MCP)
	args := launch.BuildSbxArgs(cfg, o, "0.0.99")
	for _, name := range []string{"notion", "slack"} {
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
