package envinfo_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"pix/host/envinfo"
)

// writeFixture writes yaml to dir/name and returns the full path — the
// same shape docs/design/environments.md §5.1's example and Story 0's own
// uatenvmatrix fixtures.go use, hand-authored literal bytes rather than a
// second renderer this package would then be testing against itself.
func writeFixture(t *testing.T, dir, name, yaml string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParse_MinimalValidDocument(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, ".sbxenv.yaml", `schemaVersion: "1"
agent: pix

kits:
  - ./kit

sandboxOptions:
  memory: 16g

env:
  PIX_MEMORY_SCOPE: personal
`)
	doc, err := envinfo.Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if doc.SchemaVersion != "1" {
		t.Errorf("SchemaVersion = %q, want \"1\"", doc.SchemaVersion)
	}
	if doc.Agent != "pix" {
		t.Errorf("Agent = %q, want pix", doc.Agent)
	}
	if len(doc.Kits) != 1 || doc.Kits[0].Raw != "./kit" {
		t.Fatalf("Kits = %+v, want one entry ./kit", doc.Kits)
	}
	if doc.SandboxOptions["memory"] != "16g" {
		t.Errorf("sandboxOptions.memory = %q, want 16g", doc.SandboxOptions["memory"])
	}
	if doc.Env["PIX_MEMORY_SCOPE"] != "personal" {
		t.Errorf("env.PIX_MEMORY_SCOPE = %q, want personal", doc.Env["PIX_MEMORY_SCOPE"])
	}
	if doc.Source != path {
		t.Errorf("Source = %q, want %q", doc.Source, path)
	}
}

func TestParse_UnknownTopLevelFieldRefused(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, ".sbxenv.yaml", `schemaVersion: "1"
agent: pix
bogusField: true
`)
	if _, err := envinfo.Parse(path); err == nil {
		t.Fatal("Parse: expected an error for an unknown top-level field, got nil")
	}
}

func TestParse_UnknownNestedFieldRefused(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, ".sbxenv.yaml", `schemaVersion: "1"
agent: pix
secrets:
  anthropic:
    ref: op://Personal/Anthropic/api-key
    bogus: nope
`)
	if _, err := envinfo.Parse(path); err == nil {
		t.Fatal("Parse: expected an error for an unknown nested field, got nil")
	}
}

func TestParse_UnsupportedVersionRefused(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, ".sbxenv.yaml", `schemaVersion: "2"
agent: pix
`)
	_, err := envinfo.Parse(path)
	if err == nil {
		t.Fatal("Parse: expected an error for an unsupported schemaVersion, got nil")
	}
	if !errors.Is(err, envinfo.ErrUnsupportedSchemaVersion) {
		t.Errorf("Parse error = %v, want errors.Is ErrUnsupportedSchemaVersion", err)
	}
}

func TestParse_MissingVersionRefused(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, ".sbxenv.yaml", `agent: pix
`)
	_, err := envinfo.Parse(path)
	if !errors.Is(err, envinfo.ErrUnsupportedSchemaVersion) {
		t.Errorf("Parse error = %v, want errors.Is ErrUnsupportedSchemaVersion", err)
	}
}

func TestParse_RelativeKitResolvesAgainstSourceDir_NotCWD(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "envs", "home")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	path := writeFixture(t, sub, ".sbxenv.yaml", `schemaVersion: "1"
agent: pix

kits:
  - ./kit
`)
	// Run from an unrelated cwd to prove resolution never consults it.
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	elsewhere := t.TempDir()
	if err := os.Chdir(elsewhere); err != nil {
		t.Fatal(err)
	}

	doc, err := envinfo.Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := filepath.Join(sub, "kit")
	if doc.Kits[0].Resolved != want {
		t.Errorf("Kits[0].Resolved = %q, want %q", doc.Kits[0].Resolved, want)
	}
	if !doc.Kits[0].Local {
		t.Errorf("Kits[0].Local = false, want true")
	}
}

func TestParse_AbsoluteKitPathUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, ".sbxenv.yaml", `schemaVersion: "1"
agent: pix

kits:
  - /opt/pix/kit
`)
	doc, err := envinfo.Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if doc.Kits[0].Resolved != "/opt/pix/kit" {
		t.Errorf("Resolved = %q, want /opt/pix/kit", doc.Kits[0].Resolved)
	}
	if !doc.Kits[0].Local {
		t.Errorf("Local = false, want true (absolute path is still local)")
	}
}

func TestParse_RemoteKitReferenceNotResolved(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, ".sbxenv.yaml", `schemaVersion: "1"
agent: pix

kits:
  - git+https://example.com/org/repo.git
`)
	doc, err := envinfo.Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	const raw = "git+https://example.com/org/repo.git"
	if doc.Kits[0].Resolved != raw {
		t.Errorf("Resolved = %q, want unchanged %q", doc.Kits[0].Resolved, raw)
	}
	if doc.Kits[0].Local {
		t.Errorf("Local = true, want false for a remote reference")
	}
}

func TestParse_LiteralValueRefused_Secret(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, ".sbxenv.yaml", `schemaVersion: "1"
agent: pix
secrets:
  anthropic:
    value: sk-literal-secret
`)
	_, err := envinfo.Parse(path)
	if !errors.Is(err, envinfo.ErrLiteralValueRefused) {
		t.Fatalf("Parse error = %v, want errors.Is ErrLiteralValueRefused", err)
	}
}

func TestParse_LiteralValueRefused_Registry(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, ".sbxenv.yaml", `schemaVersion: "1"
agent: pix
registries:
  docker.io:
    value: literal-token
`)
	_, err := envinfo.Parse(path)
	if !errors.Is(err, envinfo.ErrLiteralValueRefused) {
		t.Fatalf("Parse error = %v, want errors.Is ErrLiteralValueRefused", err)
	}
}

func TestParse_RefAndCommandSecretsAllowed(t *testing.T) {
	dir := t.TempDir()
	path := writeFixture(t, dir, ".sbxenv.yaml", `schemaVersion: "1"
agent: pix
secrets:
  anthropic:
    ref: op://Personal/Anthropic/api-key
  vault:
    command: ["vault-secret-tool", "read"]
registries:
  docker.io:
    ref: op://Personal/Docker/token
    noVerify: true
`)
	doc, err := envinfo.Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if doc.Secrets["anthropic"].Ref != "op://Personal/Anthropic/api-key" {
		t.Errorf("secrets.anthropic.ref = %q", doc.Secrets["anthropic"].Ref)
	}
	if len(doc.Secrets["vault"].Command) != 2 {
		t.Errorf("secrets.vault.command = %v, want 2 elements", doc.Secrets["vault"].Command)
	}
	if !doc.Registries["docker.io"].NoVerify {
		t.Errorf("registries[docker.io].NoVerify = false, want true")
	}
}

func TestParse_FullSchemaExampleDecodes(t *testing.T) {
	// Mirrors docs/design/environments.md §5.1's worked example verbatim.
	dir := t.TempDir()
	path := writeFixture(t, dir, ".sbxenv.yaml", `schemaVersion: "1"
agent: pix

kits:
  - ./kit

sandboxOptions:
  memory: 16g

env:
  PIX_MEMORY_SCOPE: personal

secrets:
  anthropic:
    ref: op://Personal/Anthropic/api-key

bindings:
  anthropic:
    apiKey:
      domains:
        - api.anthropic.com

mcp:
  servers:
    - name: github
      url: https://api.githubcopilot.com/mcp/

ports:
  - sandbox: 3000
`)
	doc, err := envinfo.Parse(path)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := doc.Bindings["anthropic"].APIKey.Domains; len(got) != 1 || got[0] != "api.anthropic.com" {
		t.Errorf("bindings.anthropic.apiKey.domains = %v", got)
	}
	if len(doc.MCP.Servers) != 1 || doc.MCP.Servers[0].Name != "github" {
		t.Errorf("mcp.servers = %+v", doc.MCP.Servers)
	}
	if len(doc.Ports) != 1 || doc.Ports[0].Sandbox != 3000 {
		t.Errorf("ports = %+v", doc.Ports)
	}
}
