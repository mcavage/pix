package env

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/envinfo"
	"pix/host/hosttrust"
)

// hostexecFixture copies testdata/hostexec-fixture into a fresh temp
// directory (never operated on in place — Load/Review mutate nothing, but
// several tests in this file deliberately mutate a COPY to prove
// fingerprint sensitivity) and registers it under name, returning the
// loaded *Environment.
func hostexecFixture(t *testing.T, name string) (*Environment, string) {
	t.Helper()
	tempConfig(t)
	cfg := loadConfig(t)

	root := t.TempDir()
	copyFixture(t, "testdata/hostexec-fixture", root)

	if _, err := Register(cfg, name, root); err != nil {
		t.Fatalf("Register: %v", err)
	}
	env, err := Load(cfg, &hosttrust.AcceptanceStore{}, name, nil, noBareLookPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return env, root
}

// noBareLookPath is a deterministic fixed-fail lookPath: every bare command
// this fixture's MCP servers and host service declare is intentionally not
// on any real PATH, so a test never depends on what happens to be installed
// on the machine running it.
func noBareLookPath(string) (string, error) { return "", errNotFound }

func copyFixture(t *testing.T, src, dst string) {
	t.Helper()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func prdMounts() []WorkspaceMount {
	return []WorkspaceMount{{Path: "/Users/alice/dev/work-pix-env", ReadOnly: false}}
}

// wantPRDBillDefault is byte-exact PRD §5.8's own fixture text (the
// `pix env review work` code block, verbatim) — the golden this test proves
// renderBill reproduces exactly, not merely "close".
const wantPRDBillDefault = `Environment "work" runs code on your host and hands it credentials.

  2 host commands      github-mcp, warehouse-mcp
  1 host service       warehouse-proxy  port 19443
  2 credential targets op://Personal/Anthropic/api-key -> api.anthropic.com
                       WAREHOUSE_TOKEN                 -> warehouse-mcp (host)
  1 new mount          /Users/alice/dev/work-pix-env   (rw)

  full argv and content digests: pix env review work --verbose

Accept this host-execution footprint? [y/N]:`

// ── golden: default tier, byte-exact against PRD §5.8 ────────────────────

func TestRenderBill_DefaultTierByteExactAgainstPRD(t *testing.T) {
	env, _ := hostexecFixture(t, "work")
	bom, err := ComputeBoM(env, prdMounts(), noBareLookPath)
	if err != nil {
		t.Fatalf("ComputeBoM: %v", err)
	}

	var buf bytes.Buffer
	renderBill(&buf, "work", bom, false)

	if buf.String() != wantPRDBillDefault {
		t.Fatalf("default bill mismatch:\n--- got ---\n%s\n--- want ---\n%s", buf.String(), wantPRDBillDefault)
	}
}

// ── golden: verbose tier adds full argv and content digests ──────────────

func TestRenderBill_VerboseAddsArgvAndDigests(t *testing.T) {
	env, _ := hostexecFixture(t, "work")
	bom, err := ComputeBoM(env, prdMounts(), noBareLookPath)
	if err != nil {
		t.Fatalf("ComputeBoM: %v", err)
	}

	var buf bytes.Buffer
	renderBill(&buf, "work", bom, true)
	got := buf.String()

	if !strings.Contains(got, "argv: github-mcp-server --stdio") {
		t.Errorf("verbose output missing full argv for github-mcp, got:\n%s", got)
	}
	if !strings.Contains(got, "argv: warehouse-mcp-server") {
		t.Errorf("verbose output missing full argv for warehouse-mcp, got:\n%s", got)
	}
	if strings.Contains(got, "--verbose") {
		t.Errorf("verbose output must not repeat the --verbose tip line, got:\n%s", got)
	}
	if !strings.HasPrefix(strings.TrimRight(got, "\n"), strings.Split(wantPRDBillDefault, "\n")[0]) {
		t.Errorf("verbose output must keep the same header, got:\n%s", got)
	}
	if !strings.HasSuffix(got, "Accept this host-execution footprint? [y/N]:") {
		t.Errorf("verbose output must still end with the consent prompt, got:\n%s", got)
	}
}

// ── Tier0 (no host-exec facet at all): empty bill ─────────────────────────

func TestComputeBoM_NonHostExecutingEnvironmentIsEmpty(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)
	root := t.TempDir()
	writeEnvFile(t, root, ".sbxenv.yaml", minimalSbxenv)
	if _, err := Register(cfg, "home", root); err != nil {
		t.Fatal(err)
	}
	env, err := Load(cfg, nil, "home", nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	bom, err := ComputeBoM(env, nil, nil)
	if err != nil {
		t.Fatalf("ComputeBoM: %v", err)
	}
	if bom.Tier1() {
		t.Fatalf("Tier1() = true for a non-host-executing environment, want false: %+v", bom)
	}
	if len(bom.HostCommands) != 0 || len(bom.HostServices) != 0 || len(bom.CredentialTargets) != 0 {
		t.Fatalf("bill of materials not empty: %+v", bom)
	}
}

// ── every fingerprinted fact changes the fingerprint ──────────────────────

func TestFingerprint_ChangesOnEveryFingerprintedFact(t *testing.T) {
	base, _ := hostexecFixture(t, "work")
	baseBoM, err := ComputeBoM(base, prdMounts(), noBareLookPath)
	if err != nil {
		t.Fatal(err)
	}
	baseFP, err := Fingerprint(baseBoM)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		mutate func(*BillOfMaterials)
	}{
		{"host command argv", func(b *BillOfMaterials) { b.HostCommands[0].Argv = append(b.HostCommands[0].Argv, "--extra") }},
		{"host service port", func(b *BillOfMaterials) { b.HostServices[0].Port = 9999 }},
		{"credential destination", func(b *BillOfMaterials) { b.CredentialTargets[0].Destination = "other.example.com" }},
		{"mount read-only bit", func(b *BillOfMaterials) { b.Mounts[0].ReadOnly = true }},
		{"secret ref", func(b *BillOfMaterials) { b.Secrets[0].Ref = "op://Personal/Anthropic/other-key" }},
		{"registry noVerify", func(b *BillOfMaterials) {
			b.Registries = append(b.Registries, RegistryFact{Host: "registry.example.com", NoVerify: true})
		}},
		{"mcp server url", func(b *BillOfMaterials) {
			b.MCPServers = append(b.MCPServers, MCPServerFact{Name: "extra", URL: "https://example.com"})
		}},
		{"port mapping", func(b *BillOfMaterials) { b.Ports = append(b.Ports, PortFact{Sandbox: 3000, Host: 3000}) }},
		{"kit content hash", func(b *BillOfMaterials) { b.Kits = append(b.Kits, KitFact{Raw: "./kit", Local: true, SHA: "deadbeef"}) }},
		{"host.mcp env keys", func(b *BillOfMaterials) { b.HostMCP[0].EnvKeys = append(b.HostMCP[0].EnvKeys, "EXTRA") }},
		{"inference backend", func(b *BillOfMaterials) {
			b.Inference = append(b.Inference, InferenceFact{Name: "zai", BaseURL: "https://api.z.ai"})
		}},
		{"interpolation reference", func(b *BillOfMaterials) {
			b.Interpolations = append(b.Interpolations, envinfo.Interpolation{Var: "VAR", KeyPath: "env.FOO"})
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mutated := baseBoM
			mutated.HostCommands = append([]HostCommand(nil), baseBoM.HostCommands...)
			mutated.HostServices = append([]HostServiceItem(nil), baseBoM.HostServices...)
			mutated.CredentialTargets = append([]CredentialTarget(nil), baseBoM.CredentialTargets...)
			mutated.Mounts = append([]WorkspaceMount(nil), baseBoM.Mounts...)
			mutated.Secrets = append([]SecretFact(nil), baseBoM.Secrets...)
			mutated.Registries = append([]RegistryFact(nil), baseBoM.Registries...)
			mutated.MCPServers = append([]MCPServerFact(nil), baseBoM.MCPServers...)
			mutated.Ports = append([]PortFact(nil), baseBoM.Ports...)
			mutated.Kits = append([]KitFact(nil), baseBoM.Kits...)
			mutated.HostMCP = append([]HostMCPFact(nil), baseBoM.HostMCP...)
			for i := range mutated.HostMCP {
				mutated.HostMCP[i].EnvKeys = append([]string(nil), baseBoM.HostMCP[i].EnvKeys...)
			}
			mutated.Inference = append([]InferenceFact(nil), baseBoM.Inference...)
			mutated.Interpolations = append([]envinfo.Interpolation(nil), baseBoM.Interpolations...)
			c.mutate(&mutated)

			got, err := Fingerprint(mutated)
			if err != nil {
				t.Fatal(err)
			}
			if got == baseFP {
				t.Errorf("Fingerprint unchanged after mutating %s", c.name)
			}
		})
	}
}

// ── every SHOWN summary fact is fingerprinted ─────────────────────────────

// TestFingerprint_EveryShownFactIsFingerprinted proves the reverse
// direction from the mutation table above: mutate exactly what
// renderCounts prints for host commands/services/credentials/mounts and
// confirm the fingerprint moves too — the "no cosmetic-only rendering"
// half of AC-66's pairing.
func TestFingerprint_EveryShownFactIsFingerprinted(t *testing.T) {
	env, _ := hostexecFixture(t, "work")
	bom, err := ComputeBoM(env, prdMounts(), noBareLookPath)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	renderCounts(&buf, bom)
	shown := buf.String()

	for _, want := range []string{
		"github-mcp", "warehouse-mcp", "warehouse-proxy", "19443",
		"op://Personal/Anthropic/api-key", "api.anthropic.com",
		"WAREHOUSE_TOKEN", "/Users/alice/dev/work-pix-env", "(rw)",
	} {
		if !strings.Contains(shown, want) {
			t.Fatalf("renderCounts output missing %q; fixture/render drifted:\n%s", want, shown)
		}
	}

	base, err := Fingerprint(bom)
	if err != nil {
		t.Fatal(err)
	}
	bom.HostCommands[0].Name = "renamed"
	changed, err := Fingerprint(bom)
	if err != nil {
		t.Fatal(err)
	}
	if base == changed {
		t.Fatal("renaming a rendered host command name must change the fingerprint")
	}
}

// ── interpolation metadata present, values never shown ───────────────────

func TestBoM_InterpolationsShowVarAndKeyPathNeverResolvedValue(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)
	root := t.TempDir()
	writeEnvFile(t, root, ".sbxenv.yaml", "schemaVersion: \"1\"\nenv:\n  PIX_MEMORY_SCOPE: ${MEMORY_SCOPE}\n")
	if _, err := Register(cfg, "home", root); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MEMORY_SCOPE", "super-secret-resolved-value")

	env, err := Load(cfg, nil, "home", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	bom, err := ComputeBoM(env, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(bom.Interpolations) != 1 {
		t.Fatalf("Interpolations = %+v, want exactly one", bom.Interpolations)
	}
	if bom.Interpolations[0].Var != "MEMORY_SCOPE" || bom.Interpolations[0].KeyPath != "env.PIX_MEMORY_SCOPE" {
		t.Fatalf("Interpolations[0] = %+v, want Var=MEMORY_SCOPE KeyPath=env.PIX_MEMORY_SCOPE", bom.Interpolations[0])
	}

	var buf bytes.Buffer
	renderCounts(&buf, bom)
	got := buf.String()
	if !strings.Contains(got, "${MEMORY_SCOPE}") || !strings.Contains(got, "env.PIX_MEMORY_SCOPE") {
		t.Fatalf("interpolation line missing var/keypath:\n%s", got)
	}
	if strings.Contains(got, "super-secret-resolved-value") {
		t.Fatalf("resolved value leaked into review output:\n%s", got)
	}

	fp, err := Fingerprint(bom)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fp, "super-secret-resolved-value") {
		t.Fatal("resolved value leaked into fingerprint")
	}
}

// ── noVerify: displayed and fingerprint-changing ─────────────────────────

func TestBoM_NoVerifyDisplaysAndChangesFingerprint(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)

	withoutRoot := t.TempDir()
	writeEnvFile(t, withoutRoot, ".sbxenv.yaml", "schemaVersion: \"1\"\nregistries:\n  registry.example.com:\n    ref: op://Personal/Registry/token\n")
	if _, err := Register(cfg, "without", withoutRoot); err != nil {
		t.Fatal(err)
	}
	withoutEnv, err := Load(cfg, nil, "without", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	withoutBoM, err := ComputeBoM(withoutEnv, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	withoutFP, err := Fingerprint(withoutBoM)
	if err != nil {
		t.Fatal(err)
	}

	withRoot := t.TempDir()
	writeEnvFile(t, withRoot, ".sbxenv.yaml", "schemaVersion: \"1\"\nregistries:\n  registry.example.com:\n    ref: op://Personal/Registry/token\n    noVerify: true\n")
	if _, err := Register(cfg, "with", withRoot); err != nil {
		t.Fatal(err)
	}
	withEnv, err := Load(cfg, nil, "with", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	withBoM, err := ComputeBoM(withEnv, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	withFP, err := Fingerprint(withBoM)
	if err != nil {
		t.Fatal(err)
	}

	if withFP == withoutFP {
		t.Fatal("noVerify must change the fingerprint")
	}
	if !withBoM.Tier1() {
		t.Fatal("a noVerify registry must raise Tier1")
	}

	var buf bytes.Buffer
	renderCounts(&buf, withBoM)
	if !strings.Contains(buf.String(), "no-verify registry") || !strings.Contains(buf.String(), "registry.example.com") {
		t.Fatalf("noVerify must be displayed, got:\n%s", buf.String())
	}
	var bufWithout bytes.Buffer
	renderCounts(&bufWithout, withoutBoM)
	if strings.Contains(bufWithout.String(), "no-verify") {
		t.Fatalf("a registry without noVerify must not render a no-verify line, got:\n%s", bufWithout.String())
	}
}
