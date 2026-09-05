package env

import (
	"path/filepath"
	"testing"

	"pix/host/envinfo"
	"pix/host/stack"
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

	facts, err := EnvironmentFacts(doc, nil, home)
	if err != nil {
		t.Fatal(err)
	}
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

func TestEnvironmentFacts_LocalServersAreScopedRemoteServersShared(t *testing.T) {
	doc := &envinfo.Document{}
	doc.MCP.Servers = []envinfo.MCPServer{
		{Name: "files", Command: "docker", Args: []string{"run", "${PIX_HOME}/files"}},
		{Name: "remote", URL: "https://example.com/mcp"},
	}
	var previous string
	for range 2 {
		home := t.TempDir()
		facts, err := EnvironmentFacts(doc, nil, home)
		if err != nil {
			t.Fatal(err)
		}
		id, err := stack.ID(home)
		if err != nil {
			t.Fatal(err)
		}
		if facts[0].Name != "files-"+id || facts[0].Name == previous {
			t.Fatalf("local registration does not identify its home: %v", facts)
		}
		if facts[1].Name != "remote" {
			t.Fatalf("remote registration should remain shared: %v", facts)
		}
		previous = facts[0].Name
	}
	if _, err := EnvironmentFacts(doc, nil, ""); err == nil {
		t.Fatal("unknown home must not produce a local registration")
	}
}
