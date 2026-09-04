package env

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/envinfo"
	"pix/host/pixhome"
)

// hostexecFixture copies testdata/hostexec-fixture into a fresh v2 PIX_HOME
// under envs/<name>/ and loads it via ResolveIn+LoadHome, returning the
// loaded *Environment and its root.
func hostexecFixture(t *testing.T, name string) (*Environment, string) {
	t.Helper()
	home := pixhome.New(t.TempDir())
	root := home.EnvironmentDir(name)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	copyFixture(t, "testdata/hostexec-fixture", root)

	sel, err := ResolveIn(home, name)
	if err != nil {
		t.Fatalf("ResolveIn: %v", err)
	}
	env, err := LoadHome(sel, nil, noBareLookPath)
	if err != nil {
		t.Fatalf("LoadHome: %v", err)
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

func prdMounts() EffectiveMounts {
	return EffectiveMounts{{Path: "/Users/alice/dev/work-pix-env", ReadOnly: false}}
}

// ── Tier0 (no host-exec facet at all): empty bill ─────────────────────────

func TestComputeBoM_NonHostExecutingEnvironmentIsEmpty(t *testing.T) {
	env := loadTestEnv(t, "home", minimalSbxenv, "")

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
		{"mount read-only bit", func(b *BillOfMaterials) { b.EffectiveMounts[0].ReadOnly = true }},
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
			mutated.EffectiveMounts = append(EffectiveMounts(nil), baseBoM.EffectiveMounts...)
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

// ── interpolation metadata present, values never shown ───────────────────

func TestBoM_InterpolationsShowVarAndKeyPathNeverResolvedValue(t *testing.T) {
	t.Setenv("MEMORY_SCOPE", "super-secret-resolved-value")
	env := loadTestEnv(t, "home", "schemaVersion: \"1\"\nenv:\n  PIX_MEMORY_SCOPE: ${MEMORY_SCOPE}\n", "")

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

	fp, err := Fingerprint(bom)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fp, "super-secret-resolved-value") {
		t.Fatal("resolved value leaked into fingerprint")
	}
}

// ── noVerify: displayed and fingerprint-changing ─────────────────────────

func TestBoM_NoVerifyChangesFingerprintAndRaisesTier1(t *testing.T) {
	withoutEnv := loadTestEnv(t, "without", "schemaVersion: \"1\"\nregistries:\n  registry.example.com:\n    ref: op://Personal/Registry/token\n", "")
	withoutBoM, err := ComputeBoM(withoutEnv, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	withoutFP, err := Fingerprint(withoutBoM)
	if err != nil {
		t.Fatal(err)
	}

	withEnv := loadTestEnv(t, "with", "schemaVersion: \"1\"\nregistries:\n  registry.example.com:\n    ref: op://Personal/Registry/token\n    noVerify: true\n", "")
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
	if len(withBoM.NoVerifyRegistries()) != 1 || withBoM.NoVerifyRegistries()[0].Host != "registry.example.com" {
		t.Fatalf("NoVerifyRegistries() = %+v, want exactly one registry.example.com entry", withBoM.NoVerifyRegistries())
	}
	if len(withoutBoM.NoVerifyRegistries()) != 0 {
		t.Fatalf("a registry without noVerify must not appear in NoVerifyRegistries(), got %+v", withoutBoM.NoVerifyRegistries())
	}
}
