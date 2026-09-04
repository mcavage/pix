package env

// bom_e18block_test.go closes E1.8's five post-ship BLOCK findings: default-
// deny is literal, so every host-executing or credential-disclosing fact
// independently makes Tier1 and appears in the bill/fingerprint, on its
// own, with no other facet present to "carry" it.
//
//  1. custom inference `key_env` is a credential target (env var name ->
//     backend/endpoint), default-rendered and Tier1.
//  2. a `host.mcp` probe is host execution even with zero env_keys: a named
//     HostCommand/probe fact, full argv in verbose, in the fingerprint.
//  3. every secret `command` is a HostCommand/Tier1 whether or not bound;
//     an unbound one still renders its command and a credential/source
//     fact, without inventing a domain.
//  4. every registry `command` likewise a HostCommand, deduplicated only by
//     stable identity (the registry's own host), never by argv coincidence.
//  5. a reviewed mount expansion is ITSELF Tier1 (docs/design/
//     environments.md §9.1: "expand[s] mounted host access" is one of the
//     four things the trust fingerprint gates), through the renamed
//     EffectiveMounts API — a distinct type, not a bare []WorkspaceMount a
//     caller could confuse with an ad hoc flag.
//
// The rendering half of this file's original coverage (renderCounts/
// renderBill/gate byte-exact assertions) tested review.go, which the v2
// four-verb surface (list/show/default/trust — no add/edit/use/review/
// forget) deleted; `pix env trust` (cmd/pix/env_cmd.go) is the new,
// package-main renderer over this SAME ComputeBoM/Fingerprint, and its own
// rendering is covered there. What stays here is every ComputeBoM/
// Fingerprint correctness property this package itself owns.

import (
	"fmt"
	"strings"
	"testing"
)

// ── finding 2 + 4 + reverse-mapping: every envinfo.Tree HostExecFacet maps
// to exactly one BillOfMaterials fact ─────────────────────────────────────

// TestComputeBoM_EveryTreeHostExecFacetMapsExactlyOnce is the reverse test:
// for every host-exec facet envinfo.BuildTree already flags (tree.go's own
// four Kinds), ComputeBoM must produce EXACTLY one corresponding fact — not
// zero (silently dropped) and not more than one (double-counted). The
// `default:` case fails loudly if tree.go ever grows a fifth Kind this test
// was never taught to map.
func TestComputeBoM_EveryTreeHostExecFacetMapsExactlyOnce(t *testing.T) {
	sbxenv := `schemaVersion: "1"
secrets:
  db:
    command: ["db-secret-tool"]
registries:
  no-verify.example.com:
    ref: op://Personal/Registry/token
    noVerify: true
  cmd.example.com:
    command: ["reg-tool"]
mcp:
  servers:
    - name: worker-mcp
      command: worker-mcp-server
`
	env := loadTestEnv(t, "iso", sbxenv, "")
	if len(env.Tree.HostExecFacets) != 4 {
		t.Fatalf("fixture drifted: want exactly 4 HostExecFacets, got %d: %+v", len(env.Tree.HostExecFacets), env.Tree.HostExecFacets)
	}

	bom, err := ComputeBoM(env, nil, noBareLookPath)
	if err != nil {
		t.Fatalf("ComputeBoM: %v", err)
	}

	hostCommandCount := func(name string) int {
		n := 0
		for _, c := range bom.HostCommands {
			if c.Name == name {
				n++
			}
		}
		return n
	}
	noVerifyCount := func(host string) int {
		n := 0
		for _, r := range bom.NoVerifyRegistries() {
			if r.Host == host {
				n++
			}
		}
		return n
	}

	for _, f := range env.Tree.HostExecFacets {
		switch f.Kind {
		case "secret-command":
			name := strings.TrimPrefix(f.KeyPath, "secrets.")
			if got := hostCommandCount("secret:" + name); got != 1 {
				t.Errorf("facet %+v: want exactly one HostCommand %q, got %d", f, "secret:"+name, got)
			}
		case "registry-command":
			host := strings.TrimPrefix(f.KeyPath, "registries.")
			if got := hostCommandCount("registry:" + host); got != 1 {
				t.Errorf("facet %+v: want exactly one HostCommand %q, got %d", f, "registry:"+host, got)
			}
		case "registry-no-verify":
			host := strings.TrimPrefix(f.KeyPath, "registries.")
			if got := noVerifyCount(host); got != 1 {
				t.Errorf("facet %+v: want exactly one no-verify registry %q, got %d", f, host, got)
			}
		case "mcp-command":
			name := strings.TrimSuffix(strings.TrimPrefix(f.KeyPath, "mcp.servers["), "]")
			if got := hostCommandCount(name); got != 1 {
				t.Errorf("facet %+v: want exactly one HostCommand %q, got %d", f, name, got)
			}
		default:
			t.Errorf("unclassified HostExecFacet kind %q at %s — teach this reverse-mapping test about it", f.Kind, f.KeyPath)
		}
	}

	// Reverse direction: no EXTRA HostCommand/no-verify fact beyond the
	// four facets above (one facet is registry-no-verify, which never adds
	// a HostCommand, so 3 HostCommands + 1 no-verify registry = 4 facets).
	if got := len(bom.HostCommands); got != 3 {
		t.Errorf("HostCommands = %+v, want exactly 3 (one per non-noVerify facet)", bom.HostCommands)
	}
	if got := len(bom.NoVerifyRegistries()); got != 1 {
		t.Errorf("NoVerifyRegistries = %+v, want exactly 1", bom.NoVerifyRegistries())
	}
}

// ── finding 4: dedup by stable identity, never by argv coincidence ────────

func TestComputeBoM_IdenticalArgvAcrossDifferentRegistriesNotDeduped(t *testing.T) {
	sbxenv := `schemaVersion: "1"
registries:
  one.example.com:
    command: ["shared-tool", "--login"]
  two.example.com:
    command: ["shared-tool", "--login"]
`
	env := loadTestEnv(t, "iso", sbxenv, "")
	bom, err := ComputeBoM(env, nil, noBareLookPath)
	if err != nil {
		t.Fatalf("ComputeBoM: %v", err)
	}
	if len(bom.HostCommands) != 2 {
		t.Fatalf("HostCommands = %+v, want 2 distinct entries even though argv coincides", bom.HostCommands)
	}
	names := map[string]bool{}
	for _, c := range bom.HostCommands {
		names[c.Name] = true
	}
	if !names["registry:one.example.com"] || !names["registry:two.example.com"] {
		t.Fatalf("HostCommands = %+v, want both registry identities present", bom.HostCommands)
	}
}

// ── finding 3: unbound secret command still renders + is Tier1 ───────────

func TestComputeBoM_UnboundSecretCommandStillHostCommandAndCredentialFact(t *testing.T) {
	sbxenv := `schemaVersion: "1"
secrets:
  db:
    command: ["db-secret-tool", "--json"]
`
	env := loadTestEnv(t, "iso", sbxenv, "")
	bom, err := ComputeBoM(env, nil, noBareLookPath)
	if err != nil {
		t.Fatalf("ComputeBoM: %v", err)
	}
	if !bom.Tier1() {
		t.Fatal("an unbound secret command must still raise Tier1 — it is host execution regardless of binding")
	}
	found := false
	for _, c := range bom.HostCommands {
		if c.Name == "secret:db" {
			found = true
			if strings.Join(c.Argv, " ") != "db-secret-tool --json" {
				t.Errorf("HostCommand argv = %v, want the secret's full command", c.Argv)
			}
		}
	}
	if !found {
		t.Fatalf("HostCommands = %+v, want a secret:db entry", bom.HostCommands)
	}
	var target *CredentialTarget
	for i, c := range bom.CredentialTargets {
		if c.Destination == unboundCredentialDestination {
			target = &bom.CredentialTargets[i]
		}
	}
	if target == nil {
		t.Fatalf("CredentialTargets = %+v, want one with Destination %q (never an invented domain)", bom.CredentialTargets, unboundCredentialDestination)
	}
	if target.Source != "command: db-secret-tool --json" {
		t.Errorf("CredentialTarget.Source = %q, want the command text", target.Source)
	}
}

// ── finding 1: inference key_env is a credential target ──────────────────

func TestComputeBoM_InferenceKeyEnvIsCredentialTargetAndTier1(t *testing.T) {
	sidecar := "schema = 1\n\n[inference.backends.zai]\nbase_url = \"https://api.z.ai\"\nauth = \"1password\"\nkey_env = \"ZAI_API_KEY\"\n"
	env := loadTestEnv(t, "iso", minimalSbxenv, sidecar)
	bom, err := ComputeBoM(env, nil, noBareLookPath)
	if err != nil {
		t.Fatalf("ComputeBoM: %v", err)
	}
	if !bom.Tier1() {
		t.Fatal("a custom inference backend's key_env must raise Tier1 — it is a credential handoff")
	}
	var target *CredentialTarget
	for i, c := range bom.CredentialTargets {
		if c.Source == "ZAI_API_KEY" {
			target = &bom.CredentialTargets[i]
		}
	}
	if target == nil {
		t.Fatalf("CredentialTargets = %+v, want a ZAI_API_KEY source", bom.CredentialTargets)
	}
	if target.Destination != "https://api.z.ai" {
		t.Errorf("CredentialTarget.Destination = %q, want the backend's base_url", target.Destination)
	}
}

// no base_url: destination falls back to the backend's own name rather
// than an empty string.
func TestComputeBoM_InferenceKeyEnvWithNoBaseURLFallsBackToName(t *testing.T) {
	sidecar := "schema = 1\n\n[inference.backends.zai]\nauth = \"1password\"\nkey_env = \"ZAI_API_KEY\"\n"
	env := loadTestEnv(t, "iso", minimalSbxenv, sidecar)
	bom, err := ComputeBoM(env, nil, noBareLookPath)
	if err != nil {
		t.Fatalf("ComputeBoM: %v", err)
	}
	for _, c := range bom.CredentialTargets {
		if c.Source == "ZAI_API_KEY" && c.Destination == "zai (inference)" {
			return
		}
	}
	t.Fatalf("CredentialTargets = %+v, want ZAI_API_KEY -> \"zai (inference)\"", bom.CredentialTargets)
}

// ── finding 2: host.mcp probe_args is host execution even with zero env_keys ──

func TestComputeBoM_HostMCPProbeArgsIsHostCommandEvenWithNoEnvKeys(t *testing.T) {
	sbxenv := `schemaVersion: "1"
mcp:
  servers:
    - name: probe-mcp
      command: probe-mcp-server
`
	sidecar := "schema = 1\n\n[host.mcp.probe-mcp]\nprobe_args = [\"--health\"]\n"
	env := loadTestEnv(t, "iso", sbxenv, sidecar)
	bom, err := ComputeBoM(env, nil, noBareLookPath)
	if err != nil {
		t.Fatalf("ComputeBoM: %v", err)
	}
	if len(bom.HostMCP) != 1 || len(bom.HostMCP[0].EnvKeys) != 0 {
		t.Fatalf("test setup error: fixture must declare zero env_keys, got %+v", bom.HostMCP)
	}
	if !bom.Tier1() {
		t.Fatal("a host.mcp probe must raise Tier1 even with zero env_keys — it still runs on this host")
	}
	found := false
	for _, c := range bom.HostCommands {
		if c.Name == "probe-mcp (probe)" {
			found = true
			if strings.Join(c.Argv, " ") != "--health" {
				t.Errorf("probe HostCommand argv = %v, want the full probe_args", c.Argv)
			}
		}
	}
	if !found {
		t.Fatalf("HostCommands = %+v, want a named probe-mcp (probe) entry", bom.HostCommands)
	}
}

// ── finding 5: a reviewed mount expansion is ITSELF Tier1 ─────────────────

func TestComputeBoM_EffectiveMountAloneIsTier1(t *testing.T) {
	env := loadTestEnv(t, "iso", minimalSbxenv, "")
	bom, err := ComputeBoM(env, EffectiveMounts{{Path: "/workspaces/extra", ReadOnly: true}}, noBareLookPath)
	if err != nil {
		t.Fatalf("ComputeBoM: %v", err)
	}
	if !bom.Tier1() {
		t.Fatal("a reviewed workspace-mount expansion alone must raise Tier1 — it expands host access (docs/design/environments.md §9.1)")
	}

	base, err := ComputeBoM(env, nil, noBareLookPath)
	if err != nil {
		t.Fatal(err)
	}
	if base.Tier1() {
		t.Fatal("test setup error: the same environment with no mount must be Tier0")
	}
	fpWith, err := Fingerprint(bom)
	if err != nil {
		t.Fatal(err)
	}
	fpWithout, err := Fingerprint(base)
	if err != nil {
		t.Fatal(err)
	}
	if fpWith == fpWithout {
		t.Fatal("a mount expansion must change the fingerprint")
	}
}

// ── exhaustive classification matrix ──────────────────────────────────────

// TestClassificationMatrix_EachFacetAloneIsTier1AndFingerprints proves, for
// every E1.8 host-exec/credential facet in isolation (no other facet
// present to "carry" it): Tier1 alone, and present in the fingerprint
// (differs from a Tier0 baseline).
func TestClassificationMatrix_EachFacetAloneIsTier1AndFingerprints(t *testing.T) {
	type tc struct {
		name    string
		sbxenv  string
		sidecar string
		mounts  EffectiveMounts
	}
	cases := []tc{
		{name: "secret command (unbound)", sbxenv: "schemaVersion: \"1\"\nsecrets:\n  db:\n    command: [\"db-secret-tool\"]\n"},
		{name: "secret command (bound)", sbxenv: "schemaVersion: \"1\"\nsecrets:\n  db:\n    command: [\"db-secret-tool\"]\nbindings:\n  db:\n    apiKey:\n      domains:\n        - db.example.com\n"},
		{name: "secret ref (unbound)", sbxenv: "schemaVersion: \"1\"\nsecrets:\n  db:\n    ref: op://Personal/DB/token\n"},
		{name: "registry command", sbxenv: "schemaVersion: \"1\"\nregistries:\n  reg.example.com:\n    command: [\"reg-tool\"]\n"},
		{name: "registry noVerify", sbxenv: "schemaVersion: \"1\"\nregistries:\n  reg.example.com:\n    ref: op://Personal/Registry/token\n    noVerify: true\n"},
		{name: "mcp server command", sbxenv: "schemaVersion: \"1\"\nmcp:\n  servers:\n    - name: worker-mcp\n      command: worker-mcp-server\n"},
		{
			name:    "host.mcp probe_args",
			sbxenv:  "schemaVersion: \"1\"\nmcp:\n  servers:\n    - name: probe-mcp\n      command: probe-mcp-server\n",
			sidecar: "schema = 1\n\n[host.mcp.probe-mcp]\nprobe_args = [\"--health\"]\n",
		},
		{
			name:    "host.mcp env_keys",
			sbxenv:  "schemaVersion: \"1\"\nmcp:\n  servers:\n    - name: warehouse-mcp\n      command: warehouse-mcp-server\n",
			sidecar: "schema = 1\n\n[host.mcp.warehouse-mcp]\nenv_keys = [\"WAREHOUSE_TOKEN\"]\n",
		},
		{name: "inference key_env", sidecar: "schema = 1\n\n[inference.backends.zai]\nbase_url = \"https://api.z.ai\"\nkey_env = \"ZAI_API_KEY\"\n"},
		{name: "reviewed mount expansion", mounts: EffectiveMounts{{Path: "/workspaces/extra", ReadOnly: true}}},
	}

	baseEnv := loadTestEnv(t, "base", minimalSbxenv, "")
	baseBoM, err := ComputeBoM(baseEnv, nil, noBareLookPath)
	if err != nil {
		t.Fatal(err)
	}
	if baseBoM.Tier1() {
		t.Fatal("test setup error: the baseline environment must be Tier0")
	}
	baseFP, err := Fingerprint(baseBoM)
	if err != nil {
		t.Fatal(err)
	}

	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sbxenv := c.sbxenv
			if sbxenv == "" {
				sbxenv = minimalSbxenv
			}
			env := loadTestEnv(t, fmt.Sprintf("case%d", i), sbxenv, c.sidecar)
			bom, err := ComputeBoM(env, c.mounts, noBareLookPath)
			if err != nil {
				t.Fatalf("ComputeBoM: %v", err)
			}

			if !bom.Tier1() {
				t.Fatal("facet alone must raise Tier1")
			}

			fp, err := Fingerprint(bom)
			if err != nil {
				t.Fatal(err)
			}
			if fp == baseFP {
				t.Error("facet alone must change the fingerprint from the Tier0 baseline")
			}
		})
	}
}
