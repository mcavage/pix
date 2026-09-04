package launch

import (
	"path/filepath"
	"testing"

	"pix/host/envinfo"
)

// The end-to-end fact this mechanism exists for: an authored `${PIX_HOME}`
// inside a local MCP server's argv reaches the effective document as the
// real path, because `mcp.servers[].args` is static argv with no shell and
// no observed upstream expansion. Anything else would hand `docker run -v`
// a literal `${PIX_HOME}` and create a directory named after the expression.
func TestEnvMCPWrapperFacts_ResolvesPixHomeInArgv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)

	doc := &envinfo.Document{}
	doc.MCP.Servers = []envinfo.MCPServer{{
		Name:    "google-workspace",
		Command: "docker",
		Args: []string{
			"run", "--rm", "-i",
			"-v", "${PIX_HOME}/.state/integrations/gog:/home/gog",
			"-e", "GOG_KEYRING_PASSWORD",
			"ghcr.io/example/gogcli@sha256:" + "0000000000000000000000000000000000000000000000000000000000000000",
		},
	}}

	facts := EnvMCPWrapperFacts(doc, nil)
	if len(facts) != 1 {
		t.Fatalf("got %d facts, want 1", len(facts))
	}
	want := filepath.Join(home, ".state/integrations/gog") + ":/home/gog"
	if facts[0].Args[4] != want {
		t.Fatalf("mount arg = %q, want %q", facts[0].Args[4], want)
	}
	// A host variable is NOT resolved here: sbx owns those, and the
	// effective document is persisted state that must not become a sink of
	// resolved host values.
	if facts[0].Args[6] != "GOG_KEYRING_PASSWORD" {
		t.Fatalf("passthrough env name was rewritten: %q", facts[0].Args[6])
	}
}

// The fingerprint resolver must key the value the launch actually used. If
// it fell through to the host environment while the argv expansion used
// Pix's own home, an unchanged environment would fingerprint differently on
// a host that exported nothing.
func TestResolveInterpolation_PixHomeMatchesTheArgvExpansion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)

	got := resolveInterpolation(func(string) (string, bool) { return "", false }, "PIX_HOME", nil)
	if got != home {
		t.Fatalf("resolveInterpolation(PIX_HOME) = %q, want %q", got, home)
	}
}
