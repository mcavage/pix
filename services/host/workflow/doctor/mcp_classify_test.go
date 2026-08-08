package doctor

import (
	"testing"

	"pix/host/config"
	"pix/host/mcp"
)

// TestClassifyMCPServer_Gog pins the one classification that has no other
// witness left. gog is never in the bridge's local-name set (`pix-host mcp
// --list` deliberately excludes it), and W2/U02B deleted the bespoke
// gworkspace doctor group that used to state its registration for it — so
// without the special case in classifyMCPServer, doctor would tell a user to
// go read `sbx mcp add --help` for the one server `pix mcp register`
// registers itself.
func TestClassifyMCPServer_Gog(t *testing.T) {
	// The realistic post-U02B inventory: a known-but-empty local set (no
	// built-in stdio servers remain in the public tree).
	got := classifyMCPServer(config.GWServerName, nil, map[string]bool{}, true)
	if got.RegisterFix != "pix mcp register "+config.GWServerName {
		t.Errorf("RegisterFix = %q, want the generic register command", got.RegisterFix)
	}
	if got.Remote {
		t.Errorf("gog classified Remote=%v; it is host-registered, so there is no control-plane auth to probe", got.Remote)
	}

	// The classification must not depend on the local set being KNOWN: a
	// failed `pix-host mcp --list` leaves every other name unknown (fail
	// closed), but gog's kind is a fact about gog, not about that probe.
	if unknown := classifyMCPServer(config.GWServerName, nil, nil, false); unknown.RegisterFix != got.RegisterFix || unknown.Remote {
		t.Errorf("with an unknown local set: %+v, want the same host-registered classification %+v", unknown, got)
	}

	// A pack that declares gog as its own REMOTE integration still wins: the
	// pack states its kind explicitly, and that beats the built-in special case.
	packed := classifyMCPServer(config.GWServerName,
		map[string]config.MCPContainer{config.GWServerName: {RemoteURL: "https://example.invalid/mcp"}},
		map[string]bool{}, true)
	if !packed.Remote {
		t.Errorf("pack-declared remote gog = %+v, want Remote (an explicit declaration outranks the special case)", packed)
	}
}

// TestClassifyMCPServer_NonGogUnchanged is the negative half: the special case
// is exactly one name wide. A catalog name stays remote, and an unknown name
// under an unusable inventory still fails closed with no repair command.
func TestClassifyMCPServer_NonGogUnchanged(t *testing.T) {
	var catalog string
	for n := range mcp.McpCatalogNames {
		catalog = n
		break
	}
	if catalog == "" {
		t.Fatal("the shipped MCP catalog is empty; this test has nothing to check")
	}
	if got := classifyMCPServer(catalog, nil, map[string]bool{}, true); !got.Remote || got.RegisterFix != "pix mcp bundle" {
		t.Errorf("catalog server %q = %+v, want remote + `pix mcp bundle`", catalog, got)
	}
	if got := classifyMCPServer("invented", nil, nil, false); got.RegisterFix != "" || got.Remote {
		t.Errorf("unknown server under an unusable inventory = %+v, want no repair command (fail closed)", got)
	}
}
