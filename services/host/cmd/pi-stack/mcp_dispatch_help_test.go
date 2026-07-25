package main

import (
	"strings"
	"testing"
)

// mcpDispatchSubcommands mirrors runMcpCmd's switch in mcp.go (register, ls,
// load, auth, bundle) and its own "unknown subcommand" error message list.
// Kept as one hand-maintained list (the same convention TestVerbUsage_Routing
// already uses for top-level verbs) so a subcommand added to the dispatcher
// without updating help can never happen silently: this test cross-checks it
// against BOTH the synopsis and bundle description, so drift in help.go fails
// loudly instead of shipping a stale `pi-stack mcp <register|ls|load>` line.
var mcpDispatchSubcommands = []string{"register", "ls", "load", "auth", "bundle"}

// TestMcpUsage_SynopsisListsEverySubcommand is ship blocker #3: the synopsis
// line must include every subcommand the dispatcher actually routes (auth and
// bundle were missing).
func TestMcpUsage_SynopsisListsEverySubcommand(t *testing.T) {
	firstLine := strings.SplitN(mcpUsage, "\n", 2)[0]
	for _, sub := range mcpDispatchSubcommands {
		if !strings.Contains(firstLine, sub) {
			t.Errorf("mcpUsage synopsis %q is missing subcommand %q", firstLine, sub)
		}
	}
}

// TestMcpUsage_BodyDocumentsEverySubcommand: beyond the synopsis line, each
// subcommand must have its own body entry (a stale synopsis fix alone would
// not catch a subcommand whose body line was dropped).
func TestMcpUsage_BodyDocumentsEverySubcommand(t *testing.T) {
	for _, sub := range mcpDispatchSubcommands {
		if !strings.Contains(mcpUsage, "\n  "+sub+" ") && !strings.Contains(mcpUsage, "\n  "+sub+"\n") {
			t.Errorf("mcpUsage body has no entry for subcommand %q:\n%s", sub, mcpUsage)
		}
	}
}

// TestMcpUsage_BundleDescriptionUsesCatalogSourceOfTruth: the bundle line's
// catalog description must name every mcpCatalogNames entry (built via
// mcpCatalogSummary in mcp.go), so it can never silently drift from what
// `pi-stack mcp bundle` actually registers.
func TestMcpUsage_BundleDescriptionUsesCatalogSourceOfTruth(t *testing.T) {
	for name := range mcpCatalogNames {
		if !strings.Contains(mcpUsage, name) {
			t.Errorf("mcpUsage bundle description is missing catalog name %q:\n%s", name, mcpUsage)
		}
	}
}

// TestVerbUsage_Mcp_MatchesMcpUsage: verbUsage("mcp") (help.go's routing map)
// must return the SAME string mcp.go prints, so `pi-stack help mcp` and
// `pi-stack mcp -h` never disagree.
func TestVerbUsage_Mcp_MatchesMcpUsage(t *testing.T) {
	u, ok := verbUsage("mcp")
	if !ok {
		t.Fatal("verbUsage(mcp) not found")
	}
	if u != mcpUsage {
		t.Errorf("verbUsage(mcp) != mcpUsage:\n%s\n---\n%s", u, mcpUsage)
	}
}
