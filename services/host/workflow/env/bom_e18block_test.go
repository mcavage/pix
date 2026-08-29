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

import (
	"bytes"
	"strings"
	"testing"

	"pix/host/cli"
	"pix/host/hosttrust"
)

// loadMinimalEnv registers and loads an environment from inline sbxenv/
// sidecar content — this file's isolated single-facet tests each need a
// fresh root/registration but not hostexecFixture's full multi-facet
// fixture. sidecar == "" means no pix.toml at all.
func loadMinimalEnv(t *testing.T, sbxenv, sidecar string) *Environment {
	t.Helper()
	tempConfig(t)
	cfg := loadConfig(t)
	root := t.TempDir()
	writeEnvFile(t, root, ".sbxenv.yaml", sbxenv)
	if sidecar != "" {
		writeEnvFile(t, root, "pix.toml", sidecar)
	}
	if _, err := Register(cfg, "iso", root); err != nil {
		t.Fatalf("Register: %v", err)
	}
	env, err := Load(cfg, &hosttrust.AcceptanceStore{}, "iso", nil, noBareLookPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return env
}

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
	env := loadMinimalEnv(t, sbxenv, "")
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
	env := loadMinimalEnv(t, sbxenv, "")
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
	env := loadMinimalEnv(t, sbxenv, "")
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

	var buf bytes.Buffer
	renderCounts(&buf, bom)
	if !strings.Contains(buf.String(), "secret:db") || !strings.Contains(buf.String(), unboundCredentialDestination) {
		t.Fatalf("an unbound secret command must still render by default, got:\n%s", buf.String())
	}
}

// ── finding 1: inference key_env is a credential target ──────────────────

func TestComputeBoM_InferenceKeyEnvIsCredentialTargetAndTier1(t *testing.T) {
	sidecar := "schema = 1\n\n[inference.backends.zai]\nbase_url = \"https://api.z.ai\"\nauth = \"1password\"\nkey_env = \"ZAI_API_KEY\"\n"
	env := loadMinimalEnv(t, minimalSbxenv, sidecar)
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

	var buf bytes.Buffer
	renderCounts(&buf, bom)
	if !strings.Contains(buf.String(), "ZAI_API_KEY") || !strings.Contains(buf.String(), "https://api.z.ai") {
		t.Fatalf("key_env credential target must render by default, got:\n%s", buf.String())
	}
}

// no base_url: destination falls back to the backend's own name rather
// than an empty string.
func TestComputeBoM_InferenceKeyEnvWithNoBaseURLFallsBackToName(t *testing.T) {
	sidecar := "schema = 1\n\n[inference.backends.zai]\nauth = \"1password\"\nkey_env = \"ZAI_API_KEY\"\n"
	env := loadMinimalEnv(t, minimalSbxenv, sidecar)
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
	env := loadMinimalEnv(t, sbxenv, sidecar)
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

	var buf bytes.Buffer
	renderCounts(&buf, bom)
	if !strings.Contains(buf.String(), "probe-mcp (probe)") {
		t.Fatalf("probe must be named and visible by default, got:\n%s", buf.String())
	}
	var vbuf bytes.Buffer
	renderBill(&vbuf, "iso", bom, true)
	if !strings.Contains(vbuf.String(), "argv: --health") {
		t.Fatalf("verbose output must show the probe's full argv, got:\n%s", vbuf.String())
	}
}

// ── finding 5: a reviewed mount expansion is ITSELF Tier1 ─────────────────

func TestComputeBoM_EffectiveMountAloneIsTier1(t *testing.T) {
	env := loadMinimalEnv(t, minimalSbxenv, "")
	bom, err := ComputeBoM(env, EffectiveMounts{{Path: "/workspaces/extra", ReadOnly: true}}, noBareLookPath)
	if err != nil {
		t.Fatalf("ComputeBoM: %v", err)
	}
	if !bom.Tier1() {
		t.Fatal("a reviewed workspace-mount expansion alone must raise Tier1 — it expands host access (docs/design/environments.md §9.1)")
	}
	var buf bytes.Buffer
	renderCounts(&buf, bom)
	if !strings.Contains(buf.String(), "/workspaces/extra") {
		t.Fatalf("mount expansion must be visible by default, got:\n%s", buf.String())
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

func TestReview_EffectiveMountAloneRefusesNonTTY(t *testing.T) {
	tempConfigAndState(t)
	cfg := loadConfig(t)
	root := t.TempDir()
	writeEnvFile(t, root, ".sbxenv.yaml", minimalSbxenv)
	if _, err := Register(cfg, "home", root); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	res, err := Review(cfg, "home", nil, EffectiveMounts{{Path: "/workspaces/extra"}}, noBareLookPath, ReviewOptions{Out: &out, TTY: false, Yes: false})
	if err == nil {
		t.Fatal("a new mount expansion must gate review even with no other host-exec facet")
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("cli.ExitCode(err) = %d, want 2", got)
	}
	if res != nil {
		t.Errorf("result = %+v, want nil", res)
	}
	if !strings.Contains(out.String(), "/workspaces/extra") {
		t.Errorf("non-TTY output must still print the mount, got:\n%s", out.String())
	}
}

// ── exhaustive classification matrix ──────────────────────────────────────

// TestClassificationMatrix_EachFacetAloneIsTier1DefaultVisibleAndFingerprints
// proves, for every E1.8 host-exec/credential facet in isolation (no other
// facet present to "carry" it): Tier1 alone, visible in the default
// (non-verbose) render, present in the fingerprint (differs from a Tier0
// baseline), and — where the facet carries argv — shown in --verbose, plus
// a non-TTY review refusal (exit 2, fails closed, writes nothing).
func TestClassificationMatrix_EachFacetAloneIsTier1DefaultVisibleAndFingerprints(t *testing.T) {
	type tc struct {
		name        string
		sbxenv      string
		sidecar     string
		mounts      EffectiveMounts
		wantDefault []string
		wantVerbose []string // nil when the facet carries no argv to show
	}
	cases := []tc{
		{
			name:        "secret command (unbound)",
			sbxenv:      "schemaVersion: \"1\"\nsecrets:\n  db:\n    command: [\"db-secret-tool\"]\n",
			wantDefault: []string{"secret:db", unboundCredentialDestination},
			wantVerbose: []string{"argv: db-secret-tool"},
		},
		{
			name:        "secret command (bound)",
			sbxenv:      "schemaVersion: \"1\"\nsecrets:\n  db:\n    command: [\"db-secret-tool\"]\nbindings:\n  db:\n    apiKey:\n      domains:\n        - db.example.com\n",
			wantDefault: []string{"secret:db", "db.example.com"},
			wantVerbose: []string{"argv: db-secret-tool"},
		},
		{
			name:        "secret ref (unbound)",
			sbxenv:      "schemaVersion: \"1\"\nsecrets:\n  db:\n    ref: op://Personal/DB/token\n",
			wantDefault: []string{"op://Personal/DB/token", unboundCredentialDestination},
		},
		{
			name:        "registry command",
			sbxenv:      "schemaVersion: \"1\"\nregistries:\n  reg.example.com:\n    command: [\"reg-tool\"]\n",
			wantDefault: []string{"registry:reg.example.com", "reg.example.com"},
			wantVerbose: []string{"argv: reg-tool"},
		},
		{
			name:        "registry noVerify",
			sbxenv:      "schemaVersion: \"1\"\nregistries:\n  reg.example.com:\n    ref: op://Personal/Registry/token\n    noVerify: true\n",
			wantDefault: []string{"no-verify registry", "reg.example.com"},
		},
		{
			name:        "mcp server command",
			sbxenv:      "schemaVersion: \"1\"\nmcp:\n  servers:\n    - name: worker-mcp\n      command: worker-mcp-server\n",
			wantDefault: []string{"worker-mcp"},
			wantVerbose: []string{"argv: worker-mcp-server"},
		},
		{
			name:        "host.mcp probe_args",
			sbxenv:      "schemaVersion: \"1\"\nmcp:\n  servers:\n    - name: probe-mcp\n      command: probe-mcp-server\n",
			sidecar:     "schema = 1\n\n[host.mcp.probe-mcp]\nprobe_args = [\"--health\"]\n",
			wantDefault: []string{"probe-mcp (probe)"},
			wantVerbose: []string{"argv: --health"},
		},
		{
			name:        "host.mcp env_keys",
			sbxenv:      "schemaVersion: \"1\"\nmcp:\n  servers:\n    - name: warehouse-mcp\n      command: warehouse-mcp-server\n",
			sidecar:     "schema = 1\n\n[host.mcp.warehouse-mcp]\nenv_keys = [\"WAREHOUSE_TOKEN\"]\n",
			wantDefault: []string{"WAREHOUSE_TOKEN", "warehouse-mcp (host)"},
			wantVerbose: []string{"argv: warehouse-mcp-server"}, // the server's own command, not the (absent) probe
		},
		{
			name:        "inference key_env",
			sidecar:     "schema = 1\n\n[inference.backends.zai]\nbase_url = \"https://api.z.ai\"\nkey_env = \"ZAI_API_KEY\"\n",
			wantDefault: []string{"ZAI_API_KEY", "https://api.z.ai"},
		},
		{
			name:        "reviewed mount expansion",
			mounts:      EffectiveMounts{{Path: "/workspaces/extra", ReadOnly: true}},
			wantDefault: []string{"/workspaces/extra", "(ro)"},
		},
	}

	baseEnv := loadMinimalEnv(t, minimalSbxenv, "")
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

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sbxenv := c.sbxenv
			if sbxenv == "" {
				sbxenv = minimalSbxenv
			}
			env := loadMinimalEnv(t, sbxenv, c.sidecar)
			bom, err := ComputeBoM(env, c.mounts, noBareLookPath)
			if err != nil {
				t.Fatalf("ComputeBoM: %v", err)
			}

			if !bom.Tier1() {
				t.Fatal("facet alone must raise Tier1")
			}

			var buf bytes.Buffer
			renderCounts(&buf, bom)
			for _, want := range c.wantDefault {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("default render missing %q, got:\n%s", want, buf.String())
				}
			}

			if c.wantVerbose != nil {
				var vbuf bytes.Buffer
				renderVerboseDetails(&vbuf, bom)
				for _, want := range c.wantVerbose {
					if !strings.Contains(vbuf.String(), want) {
						t.Errorf("verbose render missing %q, got:\n%s", want, vbuf.String())
					}
				}
			}

			fp, err := Fingerprint(bom)
			if err != nil {
				t.Fatal(err)
			}
			if fp == baseFP {
				t.Error("facet alone must change the fingerprint from the Tier0 baseline")
			}

			var out bytes.Buffer
			err = gate(nil, &out, false, false, "iso", bom, false)
			if err == nil {
				t.Fatal("facet alone must refuse non-TTY review without --yes")
			}
			if got := cli.ExitCode(err); got != 2 {
				t.Errorf("cli.ExitCode(err) = %d, want 2", got)
			}
		})
	}
}
