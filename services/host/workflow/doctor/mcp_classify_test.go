package doctor

import (
	"slices"
	"testing"

	"pix/host/config"
	"pix/host/mcp"
)

// mcp_classify_test.go pins the CLASSIFICATION half of the MCP diagnosis: which
// repair is honest for a name, and what would count as proof it works.
//
// The classification is now a pure map lookup against what the active pack
// declares — pix ships no built-in servers and special-cases no vendor — so
// every case here is "this is what the pack said" or "the pack said nothing".
// The old gog special case is gone with the hardcoded Google Workspace
// integration: a pack that wants that server declares it like any other, and
// the transport it declares is what decides the classification.

// TestClassifyMCPServer_PackDeclarationDecidesTheKind: each transport a pack can
// declare produces a different classification, because each has a different
// thing that could be broken — a remote endpoint has control-plane auth, a
// command has a binary on PATH, and a container/manifest server has neither.
func TestClassifyMCPServer_PackDeclarationDecidesTheKind(t *testing.T) {
	declared := map[string]config.MCPServer{
		"docs":     {RemoteURL: "https://docs.example.invalid/mcp"},
		"gsuite":   {Command: "gog", Args: []string{"--readonly", "mcp"}, Probe: []string{"gog", "auth", "doctor"}},
		"hr":       {Image: "hr-mcp:0.0.1"},
		"meetings": {Manifest: "https://example.invalid/server.json"},
	}
	remote := classifyMCPServer("docs", declared, mcp.Credentials{})
	if !remote.Remote || remote.Command != "" {
		t.Errorf("a pack-declared url server = %+v, want Remote with no host command", remote)
	}
	if remote.RegisterFix != "pix mcp add docs" {
		t.Errorf("RegisterFix = %q, want the plain add command", remote.RegisterFix)
	}

	// A command server is the ONE kind whose registration can rot into a binary
	// that no longer exists, so it is the one kind that carries a binary to
	// verify — and its pack-declared probe travels with it.
	command := classifyMCPServer("gsuite", declared, mcp.Credentials{})
	if command.Command != "gog" {
		t.Errorf("Command = %q, want the declared host binary", command.Command)
	}
	if command.Remote {
		t.Error("a host command server has no hosted control-plane auth to probe")
	}
	if !slices.Equal(command.Probe, []string{"gog", "auth", "doctor"}) {
		t.Errorf("Probe = %v, want the pack-declared probe argv", command.Probe)
	}

	// Container and manifest servers: the gateway runs them, so there is no host
	// binary to resolve and no control-plane login to check.
	for _, name := range []string{"hr", "meetings"} {
		got := classifyMCPServer(name, declared, mcp.Credentials{})
		if got.Remote || got.Command != "" || got.Undeclared {
			t.Errorf("gateway-run server %q = %+v, want neither remote, command, nor undeclared", name, got)
		}
		if got.RegisterFix != "pix mcp add "+name {
			t.Errorf("%q RegisterFix = %q", name, got.RegisterFix)
		}
	}
}

// TestClassifyMCPServer_UndeclaredNameHasNoHonestRepair: a configured name no
// active pack declares is reported as exactly that. There is no repair command
// to print, because pix does not know what the name was meant to be — and
// guessing one costs a user more time than silence. The catalog is the single
// exception: pix knows those endpoints itself.
func TestClassifyMCPServer_UndeclaredNameHasNoHonestRepair(t *testing.T) {
	got := classifyMCPServer("invented", nil, mcp.Credentials{})
	if !got.Undeclared {
		t.Errorf("a name nothing declares = %+v, want Undeclared", got)
	}
	if got.RegisterFix != "" || got.Remote || got.Command != "" {
		t.Errorf("an undeclared server must carry no repair and no kind, got %+v", got)
	}
	// "linear" LOOKS like a catalog name but is not in the shipped catalog:
	// treating it as one would print a repair that silently cannot work.
	if mcp.McpCatalogNames["linear"] {
		t.Error("this case needs a name the shipped catalog does NOT know; linear is now in it")
	} else if plausible := classifyMCPServer("linear", nil, mcp.Credentials{}); !plausible.Undeclared {
		t.Errorf("a plausible-looking non-catalog name = %+v, want Undeclared", plausible)
	}

	var catalog string
	for n := range mcp.McpCatalogNames {
		catalog = n
		break
	}
	if catalog == "" {
		t.Fatal("the shipped MCP catalog is empty; this test has nothing to check")
	}
	// A name pix knows the endpoint for is registerable unaided, so it gets a
	// plain `pix mcp add <name>` — no --url, because it needs none from the user.
	if got := classifyMCPServer(catalog, nil, mcp.Credentials{}); !got.Remote || got.Undeclared || got.RegisterFix != "pix mcp add "+catalog {
		t.Errorf("catalog server %q = %+v, want remote + `pix mcp add %s`", catalog, got, catalog)
	}
	// An explicit pack declaration outranks the catalog: the pack states the kind.
	packed := classifyMCPServer(catalog, map[string]config.MCPServer{catalog: {Command: "local-bin"}}, mcp.Credentials{})
	if packed.Remote || packed.Command != "local-bin" {
		t.Errorf("a pack-declared %q = %+v, want the pack's own command classification", catalog, packed)
	}
}

// TestMCPServers_ConfigOrderAndEmptyConfig: MCPServers is the whole config's
// worth of classification, in config order, and a host with no MCP configured
// produces no servers at all (the probe reports that state as healthy).
func TestMCPServers_ConfigOrderAndEmptyConfig(t *testing.T) {
	if got := MCPServers(&config.Config{}, mcp.Credentials{}); got != nil {
		t.Errorf("no configured MCP = %+v, want nil", got)
	}
	if got := MCPServers(nil, mcp.Credentials{}); got != nil {
		t.Errorf("nil config = %+v, want nil", got)
	}
	// No active pack, so every name is undeclared unless the catalog knows it.
	t.Setenv("PIX_CONFIG", t.TempDir()+"/config.toml")
	got := MCPServers(&config.Config{MCP: []string{"b-server", "a-server"}}, mcp.Credentials{})
	if len(got) != 2 || got[0].Name != "b-server" || got[1].Name != "a-server" {
		t.Fatalf("MCPServers = %+v, want config order preserved", got)
	}
	for _, s := range got {
		if !s.Undeclared {
			t.Errorf("with no active pack, %q must be Undeclared, got %+v", s.Name, s)
		}
	}
}

// TestClassifyMCPServer_ProbeRunsThroughTheSameOpRunWrapper: a server whose
// probe needs credentials must be probed through the SAME `op run --env-file`
// wrapper the gateway uses to spawn it. A probe run in doctor's own shell
// inherits whatever the operator happened to export, which is exactly how a
// broken credential setup passes every check and then fails on first use.
// A credential-FREE server is never wrapped: it must not share fate with
// unrelated refs in the file.
func TestClassifyMCPServer_ProbeRunsThroughTheSameOpRunWrapper(t *testing.T) {
	creds := mcp.Credentials{OpPath: "/usr/bin/op", OpRefsPath: "/cfg/op-refs.env"}
	declared := map[string]config.MCPServer{
		"needs-creds": {Command: "gog", EnvKeys: []string{"GOG_KEYRING_PASSWORD"}, Probe: []string{"gog", "auth", "doctor"}},
		"no-creds":    {Command: "notes-mcp", Probe: []string{"notes-mcp", "--version"}},
	}
	wrapped := classifyMCPServer("needs-creds", declared, creds).Probe
	want := mcp.OpRunWrap(creds.OpPath, creds.OpRefsPath, []string{"gog", "auth", "doctor"})
	if !slices.Equal(wrapped, want) {
		t.Errorf("probe = %v, want it wrapped exactly as the gateway spawns it (%v)", wrapped, want)
	}
	bare := classifyMCPServer("no-creds", declared, creds).Probe
	if !slices.Equal(bare, []string{"notes-mcp", "--version"}) {
		t.Errorf("a credential-free probe must NOT be op-run wrapped, got %v", bare)
	}
	// No op / no op-refs on this host: the probe stays bare rather than becoming
	// an unrunnable command.
	if got := classifyMCPServer("needs-creds", declared, mcp.Credentials{}).Probe; !slices.Equal(got, []string{"gog", "auth", "doctor"}) {
		t.Errorf("without op/op-refs the probe must stay bare, got %v", got)
	}
}
