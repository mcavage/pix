package envinfo_test

import (
	"errors"
	"strings"
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

// TestTree_MCPServerArgsInterpolationScanned pins finding (1): BuildTree
// must scan every authored `${VAR}` reference in mcp.servers[].args, not
// only url/command, and attribute each one to a stable, index-addressed
// destination key path with no resolved value anywhere on the result.
func TestTree_MCPServerArgsInterpolationScanned(t *testing.T) {
	m := mustMergeOne(t, `schemaVersion: "1"
mcp:
  servers:
    - name: warehouse
      command: warehouse-proxy
      args:
        - "--tenant=${WAREHOUSE_TENANT}"
        - "--region=${WAREHOUSE_REGION:-us-east-1}"
`)
	tr, err := envinfo.BuildTree(m)
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	if len(tr.Interpolations) != 2 {
		t.Fatalf("Interpolations = %+v, want 2", tr.Interpolations)
	}
	first, second := tr.Interpolations[0], tr.Interpolations[1]
	if first.Var != "WAREHOUSE_TENANT" || first.KeyPath != "mcp.servers[warehouse].args[0]" {
		t.Errorf("first = %+v, want Var=WAREHOUSE_TENANT KeyPath=mcp.servers[warehouse].args[0]", first)
	}
	if second.Var != "WAREHOUSE_REGION" || second.KeyPath != "mcp.servers[warehouse].args[1]" {
		t.Errorf("second = %+v, want Var=WAREHOUSE_REGION KeyPath=mcp.servers[warehouse].args[1]", second)
	}
	if second.Default == nil || *second.Default != "us-east-1" {
		t.Errorf("second.Default = %v, want us-east-1", second.Default)
	}
	// The literal args survive unresolved on the node itself.
	if len(tr.MCPServers) != 1 || tr.MCPServers[0].Args[0] != "--tenant=${WAREHOUSE_TENANT}" {
		t.Errorf("MCPServers = %+v, want unresolved literal args", tr.MCPServers)
	}
}

// TestTree_KitsInterpolationScanned closes the same audit for the other
// authored-string field BuildTree exposes as a node but did not scan: a
// local kit path's raw authored text.
func TestTree_KitsInterpolationScanned(t *testing.T) {
	m := mustMergeOne(t, `schemaVersion: "1"
kits:
  - "./${KIT_DIR:-kit}"
`)
	tr, err := envinfo.BuildTree(m)
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	if len(tr.Interpolations) != 1 {
		t.Fatalf("Interpolations = %+v, want 1", tr.Interpolations)
	}
	got := tr.Interpolations[0]
	if got.Var != "KIT_DIR" || got.KeyPath != "kits[0]" {
		t.Errorf("got = %+v, want Var=KIT_DIR KeyPath=kits[0]", got)
	}
}

// TestTree_BindingDomainInterpolationScanned closes the audit for the
// remaining authored-string node field BuildTree exposed but never
// scanned: a binding's destination domain.
func TestTree_BindingDomainInterpolationScanned(t *testing.T) {
	m := mustMergeOne(t, `schemaVersion: "1"
bindings:
  anthropic:
    apiKey:
      domains:
        - "${ANTHROPIC_DOMAIN:-api.anthropic.com}"
`)
	tr, err := envinfo.BuildTree(m)
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	if len(tr.Interpolations) != 1 {
		t.Fatalf("Interpolations = %+v, want 1", tr.Interpolations)
	}
	got := tr.Interpolations[0]
	if got.Var != "ANTHROPIC_DOMAIN" {
		t.Errorf("Var = %q, want ANTHROPIC_DOMAIN", got.Var)
	}
	wantKP := "bindings.anthropic.apiKey.domains[${ANTHROPIC_DOMAIN:-api.anthropic.com}]"
	if got.KeyPath != wantKP {
		t.Errorf("KeyPath = %q, want %q", got.KeyPath, wantKP)
	}
}

// TestTree_BindingDomainDuplicateWithinOneFileRefused pins finding (2): a
// service authoring the identical domain twice in one file is a
// stable-identity collision, refused the same way a duplicate mcp.servers
// name or ports sandbox port is refused — never silently deduplicated or
// silently emitting two identical key paths.
func TestTree_BindingDomainDuplicateWithinOneFileRefused(t *testing.T) {
	m := mustMergeOne(t, `schemaVersion: "1"
bindings:
  anthropic:
    apiKey:
      domains:
        - api.anthropic.com
        - api.anthropic.com
`)
	_, err := envinfo.BuildTree(m)
	if !errors.Is(err, envinfo.ErrDuplicateIdentity) {
		t.Fatalf("BuildTree error = %v, want errors.Is ErrDuplicateIdentity", err)
	}
}

// TestTree_BindingDomainDuplicateAcrossMergedFilesRefused pins the harder
// half of finding (2): merge.go concatenates apiKey.domains lists across
// files (docs/design/environments.md §4), so a duplicate can be introduced
// purely by composition even though neither file alone repeats itself.
// BuildTree must still refuse it as a stable-identity collision, not emit
// two BindingDomainNode entries sharing one key path.
func TestTree_BindingDomainDuplicateAcrossMergedFilesRefused(t *testing.T) {
	base := mustParseBytes(t, `schemaVersion: "1"
bindings:
  anthropic:
    apiKey:
      domains:
        - api.anthropic.com
`, "base.yaml", "/envs/home")
	overlay := mustParseBytes(t, `schemaVersion: "1"
bindings:
  anthropic:
    apiKey:
      domains:
        - api.anthropic.com
`, "overlay.yaml", "/envs/home")
	merged, err := envinfo.Merge(base, overlay)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	_, err = envinfo.BuildTree(merged)
	if !errors.Is(err, envinfo.ErrDuplicateIdentity) {
		t.Fatalf("BuildTree error = %v, want errors.Is ErrDuplicateIdentity (list-concatenation-induced collision)", err)
	}
}

// TestTree_BindingWithZeroDomainsRefused pins finding (3): a binding whose
// effective, merged domain list is empty must not silently evaporate from
// the tree (BuildTree's loop over an empty slice would otherwise just emit
// nothing and a reviewer would never learn the service was declared at
// all). It is refused during validation instead, naming the exact service
// and key path — see doc.go/BuildTree's rationale comment for why refusal
// beats a zero-domain tree node: upstream sbx already treats a zero-domain
// binding as a functionless no-op (docs/upstream/sbx-0.37-binding-warning.md:
// "no domains allowed by your bindings; not injecting"), so it is never a
// valid declaration to carry forward silently.
func TestTree_BindingWithZeroDomainsRefused(t *testing.T) {
	m := mustMergeOne(t, `schemaVersion: "1"
bindings:
  anthropic:
    apiKey: {}
`)
	_, err := envinfo.BuildTree(m)
	if !errors.Is(err, envinfo.ErrEmptyBindingDomains) {
		t.Fatalf("BuildTree error = %v, want errors.Is ErrEmptyBindingDomains", err)
	}
	if err == nil || !strings.Contains(err.Error(), "bindings.anthropic.apiKey.domains") {
		t.Errorf("error = %v, want it to name bindings.anthropic.apiKey.domains", err)
	}
}

// TestTree_BindingWithZeroDomainsRefused_EmptyList covers the explicit
// `domains: []` spelling as well as the omitted-field spelling above — both
// must refuse identically, since both merge to the same empty slice.
func TestTree_BindingWithZeroDomainsRefused_EmptyList(t *testing.T) {
	m := mustMergeOne(t, `schemaVersion: "1"
bindings:
  anthropic:
    apiKey:
      domains: []
`)
	_, err := envinfo.BuildTree(m)
	if !errors.Is(err, envinfo.ErrEmptyBindingDomains) {
		t.Fatalf("BuildTree error = %v, want errors.Is ErrEmptyBindingDomains", err)
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
