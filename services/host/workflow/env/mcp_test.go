package env

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// Execute the actual Gateway command produced by the environment compiler,
// with an op fixture that refuses an unrelated broken provider reference.
func TestEnvironmentFacts_CredentialsAreScopedAndReadAtSpawn(t *testing.T) {
	home := t.TempDir()
	bin := t.TempDir()
	op := `#!/bin/bash
for arg in "$@"; do
  case "$arg" in --env-file=*) refs=${arg#--env-file=} ;; esac
done
content=$(cat "$refs") || exit
case "$content" in *ANTHROPIC_API_KEY*) exit 73 ;; esac
printf '%s\n' "$content"
while [ "$1" != -- ]; do shift; done
shift
exec "$@"
`
	if err := os.WriteFile(filepath.Join(bin, "op"), []byte(op), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	// Deliberately differ from the supplied home: the compiler must use its
	// caller's resolved home, not ambient process state.
	t.Setenv("PIX_HOME", t.TempDir())
	refs := filepath.Join(home, "secrets.env")
	write := func(value string) {
		t.Helper()
		body := "ANTHROPIC_API_KEY=op://fixture/missing/api key\nGOOGLE_KEY=" + value + "\n export ACCOUNT = account@example.com\nOTHER_KEY=op://fixture/other/credential\n"
		if err := os.WriteFile(refs, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("op://fixture/google/old")
	doc := &envinfo.Document{}
	doc.MCP.Servers = []envinfo.MCPServer{{Name: "google", Command: "/bin/cat"}, {Name: "other", Command: "/bin/cat"}}
	sidecar := &envinfo.Sidecar{}
	sidecar.Host.MCP = map[string]envinfo.HostMCPEntry{
		"google": {EnvKeys: []string{"GOOGLE_KEY"}, PlainKeys: []string{"ACCOUNT"}},
		"other":  {EnvKeys: []string{"OTHER_KEY"}},
	}
	facts, err := EnvironmentFacts(doc, sidecar, home)
	if err != nil {
		t.Fatal(err)
	}
	write("op://fixture/google/rotated")
	for i, fact := range facts {
		cmd := exec.Command(fact.Command, fact.Args...)
		cmd.Stdin = strings.NewReader("request body\n")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v: %s", fact.Name, err, out)
		}
		got := string(out)
		if !strings.Contains(got, "request body\n") {
			t.Fatalf("lost server stdin: %s", got)
		}
		if i == 0 {
			if !strings.Contains(got, "GOOGLE_KEY=op://fixture/google/rotated") || !strings.Contains(got, "ACCOUNT = account@example.com") || strings.Contains(got, "OTHER_KEY") {
				t.Fatalf("wrong Google credentials: %s", got)
			}
		} else if !strings.Contains(got, "OTHER_KEY=") || strings.Contains(got, "GOOGLE_KEY") || strings.Contains(got, "ACCOUNT") {
			t.Fatalf("wrong other credentials: %s", got)
		}
	}
	if err := os.Remove(refs); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(facts[0].Command, facts[0].Args...)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("unreadable refs must refuse: %s", out)
	}
}
