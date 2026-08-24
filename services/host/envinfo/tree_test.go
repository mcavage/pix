package envinfo_test

import (
	"errors"
	"testing"

	"pix/host/envinfo"
)

func mustMergeOne(t *testing.T, yaml string) *envinfo.Merged {
	t.Helper()
	doc := mustParseBytes(t, yaml, "env.yaml", "/envs/home")
	merged, err := envinfo.Merge(doc)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	return merged
}

func TestTree_KitsAreIndexAddressed(t *testing.T) {
	m := mustMergeOne(t, `schemaVersion: "1"
kits:
  - ./a
  - ./a
`)
	tr, err := envinfo.BuildTree(m)
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	if len(tr.Kits) != 2 {
		t.Fatalf("Kits = %+v, want 2 (duplicates allowed, index-addressed)", tr.Kits)
	}
	if tr.Kits[0].KeyPath != "kits[0]" || tr.Kits[1].KeyPath != "kits[1]" {
		t.Errorf("KeyPaths = %q, %q", tr.Kits[0].KeyPath, tr.Kits[1].KeyPath)
	}
}

func TestTree_MCPServerIdentityKeyPath(t *testing.T) {
	m := mustMergeOne(t, `schemaVersion: "1"
mcp:
  servers:
    - name: github
      url: https://api.githubcopilot.com/mcp/
`)
	tr, err := envinfo.BuildTree(m)
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	if len(tr.MCPServers) != 1 || tr.MCPServers[0].KeyPath != "mcp.servers[github]" {
		t.Fatalf("MCPServers = %+v", tr.MCPServers)
	}
}

func TestTree_MCPServerDuplicateNameRefused(t *testing.T) {
	base := mustParseBytes(t, `schemaVersion: "1"
mcp:
  servers:
    - name: github
      url: https://a.example/mcp
`, "base.yaml", "/envs/home")
	overlay := mustParseBytes(t, `schemaVersion: "1"
mcp:
  servers:
    - name: github
      url: https://b.example/mcp
`, "overlay.yaml", "/envs/home")
	merged, err := envinfo.Merge(base, overlay)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	_, err = envinfo.BuildTree(merged)
	if !errors.Is(err, envinfo.ErrDuplicateIdentity) {
		t.Fatalf("BuildTree error = %v, want errors.Is ErrDuplicateIdentity", err)
	}
}

func TestTree_PortIdentityKeyPath(t *testing.T) {
	m := mustMergeOne(t, `schemaVersion: "1"
ports:
  - sandbox: 3000
`)
	tr, err := envinfo.BuildTree(m)
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	if len(tr.Ports) != 1 || tr.Ports[0].KeyPath != "ports[3000]" {
		t.Fatalf("Ports = %+v", tr.Ports)
	}
}

func TestTree_PortDuplicateSandboxPortRefused(t *testing.T) {
	base := mustParseBytes(t, `schemaVersion: "1"
ports:
  - sandbox: 3000
    host: 3000
`, "base.yaml", "/envs/home")
	overlay := mustParseBytes(t, `schemaVersion: "1"
ports:
  - sandbox: 3000
    host: 4000
`, "overlay.yaml", "/envs/home")
	merged, err := envinfo.Merge(base, overlay)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	_, err = envinfo.BuildTree(merged)
	if !errors.Is(err, envinfo.ErrDuplicateIdentity) {
		t.Fatalf("BuildTree error = %v, want errors.Is ErrDuplicateIdentity", err)
	}
}

func TestTree_BindingDomainIdentityKeyPath(t *testing.T) {
	m := mustMergeOne(t, `schemaVersion: "1"
bindings:
  anthropic:
    apiKey:
      domains:
        - api.anthropic.com
`)
	tr, err := envinfo.BuildTree(m)
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	if len(tr.BindingDomains) != 1 {
		t.Fatalf("BindingDomains = %+v", tr.BindingDomains)
	}
	want := "bindings.anthropic.apiKey.domains[api.anthropic.com]"
	if tr.BindingDomains[0].KeyPath != want {
		t.Errorf("KeyPath = %q, want %q", tr.BindingDomains[0].KeyPath, want)
	}
}

func TestTree_HostExecFacet_SecretCommand(t *testing.T) {
	m := mustMergeOne(t, `schemaVersion: "1"
secrets:
  vault:
    command: ["vault-secret-tool", "read"]
`)
	tr, err := envinfo.BuildTree(m)
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	if !hasFacet(tr.HostExecFacets, "secrets.vault", "secret-command") {
		t.Errorf("HostExecFacets = %+v, want secrets.vault/secret-command", tr.HostExecFacets)
	}
}

func TestTree_HostExecFacet_RegistryCommand(t *testing.T) {
	m := mustMergeOne(t, `schemaVersion: "1"
registries:
  docker.io:
    command: ["reg-cred-helper"]
`)
	tr, err := envinfo.BuildTree(m)
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	if !hasFacet(tr.HostExecFacets, "registries.docker.io", "registry-command") {
		t.Errorf("HostExecFacets = %+v, want registries.docker.io/registry-command", tr.HostExecFacets)
	}
}

func TestTree_HostExecFacet_RegistryNoVerify(t *testing.T) {
	m := mustMergeOne(t, `schemaVersion: "1"
registries:
  docker.io:
    ref: op://Personal/Docker/token
    noVerify: true
`)
	tr, err := envinfo.BuildTree(m)
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	if !hasFacet(tr.HostExecFacets, "registries.docker.io", "registry-no-verify") {
		t.Errorf("HostExecFacets = %+v, want registries.docker.io/registry-no-verify", tr.HostExecFacets)
	}
}

func TestTree_HostExecFacet_MCPCommand(t *testing.T) {
	m := mustMergeOne(t, `schemaVersion: "1"
mcp:
  servers:
    - name: warehouse
      command: warehouse-proxy
`)
	tr, err := envinfo.BuildTree(m)
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	if !hasFacet(tr.HostExecFacets, "mcp.servers[warehouse]", "mcp-command") {
		t.Errorf("HostExecFacets = %+v, want mcp.servers[warehouse]/mcp-command", tr.HostExecFacets)
	}
}

func TestTree_NoHostExecFacetsForRefOnlySecretsAndRegistries(t *testing.T) {
	m := mustMergeOne(t, `schemaVersion: "1"
secrets:
  anthropic:
    ref: op://Personal/Anthropic/api-key
registries:
  docker.io:
    ref: op://Personal/Docker/token
`)
	tr, err := envinfo.BuildTree(m)
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	if len(tr.HostExecFacets) != 0 {
		t.Errorf("HostExecFacets = %+v, want none for ref-only declarations", tr.HostExecFacets)
	}
}

func TestTree_MCPServerMissingNameRefused(t *testing.T) {
	m := mustMergeOne(t, `schemaVersion: "1"
mcp:
  servers:
    - url: https://example.com/mcp
`)
	_, err := envinfo.BuildTree(m)
	if err == nil {
		t.Fatal("BuildTree: expected an error for an mcp server with no name")
	}
}

func hasFacet(facets []envinfo.HostExecFacet, keyPath, kind string) bool {
	for _, f := range facets {
		if f.KeyPath == keyPath && f.Kind == kind {
			return true
		}
	}
	return false
}
