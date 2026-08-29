// render_test.go pins RenderEffective's contract (AC-54) against four golden
// scenarios plus a Story 0 semantic cross-check. The goldens under
// testdata/render/ are BYTE-exact: they carry no explanatory header of their
// own precisely so the file on disk IS the expected output, with nothing a
// reader has to mentally strip. Each scenario's rationale lives on its test
// function below instead.
package envinfo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "render", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return b
}

// ── golden: no environment registered/selected (D17 "none" -> built-in) ──

// TestRenderEffective_NoEnvBuiltin is D17's "none" state: no authored
// .sbxenv.yaml/pix.toml at all, only Pix-owned runtime facts (docs/design/
// environments.md §6.2, "the effective file is generated from Pix's built-in
// defaults").
func TestRenderEffective_NoEnvBuiltin(t *testing.T) {
	facts := RuntimeFacts{
		Document:    &Document{SchemaVersion: SchemaVersionV1},
		SandboxName: "pix-workspace-00000000",
		Template:    "docker.io/mcavage/pix:v0.0.0",
		PullPolicy:  "missing",
		PrimaryWorkspace: WorkspaceFact{
			Path: "/home/user/workspace",
		},
		PersonalContextWorkspace: WorkspaceFact{
			Path: "/home/user/.local/share/pix/context",
		},
		MixinKit: "/tmp/pix-mixin-00000000",
	}
	got, err := RenderEffective(facts)
	if err != nil {
		t.Fatalf("RenderEffective: %v", err)
	}
	want := readGolden(t, "no-env-builtin.yaml")
	if string(got) != string(want) {
		t.Errorf("RenderEffective(no-env-builtin) mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// ── golden: scaffolded `home` environment ────────────────────────────────

// TestRenderEffective_ScaffoldedHome is the scaffolded `home` environment
// (`pix env add home` with no path): runnable and equivalent to the built-in
// default, not an empty stub (§8.1).
func TestRenderEffective_ScaffoldedHome(t *testing.T) {
	facts := RuntimeFacts{
		Document:    &Document{SchemaVersion: SchemaVersionV1, Agent: "pix"},
		SandboxName: "pix-home-11111111",
		Template:    "docker.io/mcavage/pix:v0.0.0",
		PullPolicy:  "missing",
		PrimaryWorkspace: WorkspaceFact{
			Path: "/home/user/.local/share/pix/envs/home",
		},
		PersonalContextWorkspace: WorkspaceFact{
			Path: "/home/user/.local/share/pix/context",
		},
		MixinKit: "/tmp/pix-mixin-11111111",
	}
	got, err := RenderEffective(facts)
	if err != nil {
		t.Fatalf("RenderEffective: %v", err)
	}
	want := readGolden(t, "scaffolded-home.yaml")
	if string(got) != string(want) {
		t.Errorf("RenderEffective(scaffolded-home) mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// ── golden: native remote + local MCP, per-server isolation ─────────────
//
// A native environment declaring both a remote (url) MCP server and a local
// (command) one, the local one already wrapped by the composed, reviewed
// op-run credential wrapper (§9.2). Each server renders in isolation: one
// server's wrapper argv never reaches another's entry.

func nativeRemoteLocalMCPFacts() RuntimeFacts {
	return RuntimeFacts{
		Document:    &Document{SchemaVersion: SchemaVersionV1, Agent: "pix"},
		SandboxName: "pix-work-22222222",
		Template:    "docker.io/mcavage/pix:v0.0.0",
		PullPolicy:  "missing",
		PrimaryWorkspace: WorkspaceFact{
			Path: "/home/user/work",
		},
		PersonalContextWorkspace: WorkspaceFact{
			Path: "/home/user/.local/share/pix/context",
		},
		MixinKit: "/tmp/pix-mixin-22222222",
		MCPServers: []MCPWrapperFact{
			{Name: "remote-mcp", URL: "https://mcp.example.com/sse"},
			{
				Name:    "github-mcp",
				Command: "/usr/bin/op",
				Args: []string{
					"run", "--no-masking",
					"--env-file=/home/user/.config/pix/mcp/github-mcp.env",
					"--", "github-mcp-server", "--stdio",
				},
			},
		},
	}
}

func TestRenderEffective_NativeRemoteAndLocalMCP(t *testing.T) {
	got, err := RenderEffective(nativeRemoteLocalMCPFacts())
	if err != nil {
		t.Fatalf("RenderEffective: %v", err)
	}
	want := readGolden(t, "native-remote-local-mcp.yaml")
	if string(got) != string(want) {
		t.Errorf("RenderEffective(native-remote-local-mcp) mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestRenderEffective_MCPServerIsolation is docs/design/environments.md
// §9.2's per-server invariant ("A server declaring A must not observe
// configured ref B"), asserted at the RENDER boundary: github-mcp's own
// env-file path must appear in exactly one server's rendered argv, and
// remote-mcp's block must carry none of github-mcp's argv tokens.
func TestRenderEffective_MCPServerIsolation(t *testing.T) {
	got, err := RenderEffective(nativeRemoteLocalMCPFacts())
	if err != nil {
		t.Fatalf("RenderEffective: %v", err)
	}
	out := string(got)
	if n := strings.Count(out, "github-mcp.env"); n != 1 {
		t.Errorf("github-mcp's env-file path must appear exactly once in the rendered document, got %d", n)
	}
	// A server's BLOCK runs from its own `- name:` entry to the next one.
	// (An earlier form of this test sliced from "remote-mcp" to
	// "github-mcp.env", which can never exclude the wrapper argv it checks
	// for: mcp.OpRunWrap's grammar emits `--no-masking` BEFORE
	// `--env-file=<path>`, so that span always contains the token whose
	// absence it asserts, for any renderer whatsoever. The entry-to-entry
	// span below is the same claim, made against a boundary the rendered
	// document actually has.)
	remoteIdx := strings.Index(out, "- name: remote-mcp")
	githubEntryIdx := strings.Index(out, "- name: github-mcp")
	githubIdx := strings.Index(out, "github-mcp.env")
	if remoteIdx < 0 || githubEntryIdx < 0 || githubIdx < 0 {
		t.Fatalf("expected both remote-mcp and github-mcp entries in output, got:\n%s", out)
	}
	if githubEntryIdx < remoteIdx {
		t.Fatalf("expected remote-mcp's entry to render before github-mcp's, got:\n%s", out)
	}
	remoteBlock := out[remoteIdx:githubEntryIdx]
	if !strings.Contains(remoteBlock, "https://mcp.example.com/sse") {
		t.Errorf("remote-mcp's own block must carry its own url:\n%s", remoteBlock)
	}
	if strings.Contains(remoteBlock, "--no-masking") {
		t.Errorf("remote-mcp's block must not carry github-mcp's op-run wrapper argv:\n%s", remoteBlock)
	}
}

// ── golden: private exclusive custom backend ─────────────────────────────

// TestRenderEffective_ExclusiveCustomBackend is a private environment whose
// sidecar sets `[models].exclusive = true` and declares one
// `[inference.backends.<name>]` custom endpoint (§6.3, "the narrow
// compliance-grade boundary for a private gateway").
//
// None of that reaches the native `.sbxenv.yaml`: §6.2 enumerates the
// Pix-owned runtime facts the effective file adds and inference is not one of
// them, while §7 puts the roster and every custom backend in exactly ONE
// generated artifact, the mixin kit's `inference.json` ("There is no second
// generated routing artifact that can disagree with provider registration").
// An `inference:` block here would be that forbidden second artifact AND an
// unknown field to sbx's loader, which "rejects unknown fields" (§4). So the
// golden is byte-identical in shape to any other environment's, and the
// explicit non-leak assertions below are the substance: a sidecar fact must
// never appear in this document, least of all a backend's key-env name.
func TestRenderEffective_ExclusiveCustomBackend(t *testing.T) {
	facts := RuntimeFacts{
		Document: &Document{SchemaVersion: SchemaVersionV1, Agent: "pix"},
		Sidecar: &Sidecar{
			Models: ModelsSection{Exclusive: true},
			Inference: InferenceSection{
				Backends: map[string]InferenceBackend{
					"private-gw": {
						Driver: "openai-compatible", Protocol: "https",
						BaseURL: "https://gateway.internal.example.com/v1",
						Auth:    "bearer", KeyEnv: "PRIVATE_GW_API_KEY",
					},
				},
				Models: []InferenceModel{
					{
						ID: "private-gw/house-model", Backend: "private-gw",
						UpstreamID: "house-model-v1", ContextWindow: 128000,
						MaxOutputTokens: 8192,
					},
				},
			},
		},
		SandboxName: "pix-gateway-33333333",
		Template:    "docker.io/mcavage/pix:v0.0.0",
		PullPolicy:  "missing",
		PrimaryWorkspace: WorkspaceFact{
			Path: "/home/user/gateway",
		},
		PersonalContextWorkspace: WorkspaceFact{
			Path: "/home/user/.local/share/pix/context",
		},
		MixinKit: "/tmp/pix-mixin-33333333",
	}
	got, err := RenderEffective(facts)
	if err != nil {
		t.Fatalf("RenderEffective: %v", err)
	}
	want := readGolden(t, "exclusive-custom-backend.yaml")
	if string(got) != string(want) {
		t.Errorf("RenderEffective(exclusive-custom-backend) mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
	for _, leaked := range []string{
		"inference",
		"exclusive",
		"private-gw",
		"openai-compatible",
		"https://gateway.internal.example.com/v1",
		"PRIVATE_GW_API_KEY",
		"house-model",
	} {
		if strings.Contains(string(got), leaked) {
			t.Errorf("sidecar fact %q leaked into the native effective document; it belongs to the generated mixin kit's inference.json (§7), not .sbxenv.yaml", leaked)
		}
	}
}

// ── no secret values ever reach the rendered document ────────────────────

func TestRenderEffective_NoSecretValues(t *testing.T) {
	facts := RuntimeFacts{
		Document: &Document{
			SchemaVersion: SchemaVersionV1,
			Secrets: map[string]SecretRef{
				// Value is what Parse refuses outright (§5.1, restriction 1);
				// it is set HERE, on a caller-constructed document, so the
				// assertion below is a real one. A fixture carrying no value
				// at all could not tell a renderer that drops values from one
				// that would happily write them.
				"anthropic": {Ref: "op://Personal/Anthropic/api-key", Value: "hunter2"},
			},
			Registries: map[string]RegistryRef{
				"registry.example.com": {Ref: "op://Personal/Registry/token", Value: "hunter2"},
			},
		},
		SandboxName: "pix-secrettest-44444444",
		Template:    "docker.io/mcavage/pix:v0.0.0",
		PullPolicy:  "missing",
	}
	got, err := RenderEffective(facts)
	if err != nil {
		t.Fatalf("RenderEffective: %v", err)
	}
	// Never a literal resolved secret value — only the safe `ref:` locator
	// string may appear (envinfo/doc.go's "Interpolation is surfaced, never
	// resolved" carries the identical discipline to secret values).
	if strings.Contains(string(got), "hunter2") {
		t.Errorf("rendered document must never contain a resolved secret value:\n%s", got)
	}
	if strings.Contains(string(got), "value:") {
		t.Errorf("rendered document must never carry a `value:` key at all:\n%s", got)
	}
	// The safe locator DOES survive: dropping the value must not silently
	// drop the whole record, which would produce a document that resolves no
	// credential at all.
	for _, want := range []string{
		"anthropic", "op://Personal/Anthropic/api-key",
		"registry.example.com", "op://Personal/Registry/token",
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("rendered document dropped the safe locator %q:\n%s", want, got)
		}
	}
}

// ── determinism: same facts, same bytes, every call ──────────────────────

func TestRenderEffective_Deterministic(t *testing.T) {
	facts := nativeRemoteLocalMCPFacts()
	a, errA := RenderEffective(facts)
	b, errB := RenderEffective(facts)
	if errA != nil || errB != nil {
		t.Fatalf("RenderEffective: errA=%v errB=%v", errA, errB)
	}
	if string(a) != string(b) {
		t.Errorf("RenderEffective must be deterministic for identical RuntimeFacts")
	}
}

// ── Story 0 semantic cross-check ─────────────────────────────────────────

// TestRenderEffective_Story0SemanticCrossCheck loads Story 0's own
// authoritative fixture (workflow/env/testdata/hostexec-fixture/
// .sbxenv.yaml, copied here so this package takes no dependency on
// workflow/env) and asserts RenderEffective's output preserves every
// stable-identity node envinfo's OWN BuildTree already derives from it —
// name, mcp.servers[<name>], secrets.<name>, bindings.<service>.
//
// This is explicitly a check of PIX'S OWN merge/tree model, not a claim
// about upstream sbx composition semantics: Story 0 proved the LIVE sbx
// contract independently (docs/design/environments.md, Story 0's own
// authoritative UAT), and this test never re-proves that upstream
// behavior — see WB-NONCLAIM-01 in
// .pi-agent/deliver/native-environments/status.json ("envinfo merge tests
// model A6 but do not prove upstream A6") and the Wave C HANDOFF's
// carried-forward non-claim: "A6 remains a non-claim: envinfo tests Pix's
// merge model; Story 0 did not independently prove upstream multi-file
// composition semantics." This test does not lift that non-claim.
func TestRenderEffective_Story0SemanticCrossCheck(t *testing.T) {
	doc, err := ParseBytes([]byte(story0FixtureSbxenvYAML), "story0-fixture", "")
	if err != nil {
		t.Fatalf("ParseBytes(story0 fixture): %v", err)
	}
	merged, err := Merge(doc)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	tree, err := BuildTree(merged)
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}

	facts := RuntimeFacts{
		Document:    doc,
		SandboxName: "pix-story0-55555555",
		Template:    "docker.io/mcavage/pix:v0.0.0",
		PullPolicy:  "missing",
		PrimaryWorkspace: WorkspaceFact{
			Path: "/home/user/story0",
		},
		PersonalContextWorkspace: WorkspaceFact{
			Path: "/home/user/.local/share/pix/context",
		},
		MixinKit: "/tmp/pix-mixin-55555555",
	}
	got, err := RenderEffective(facts)
	if err != nil {
		t.Fatalf("RenderEffective: %v", err)
	}
	out := string(got)
	for _, srv := range tree.MCPServers {
		if !strings.Contains(out, srv.Name) {
			t.Errorf("rendered effective document dropped mcp.servers[%s] present in the pre-composition tree", srv.Name)
		}
	}
	for _, sec := range tree.Secrets {
		if !strings.Contains(out, sec.Name) {
			t.Errorf("rendered effective document dropped secrets.%s present in the pre-composition tree", sec.Name)
		}
	}
	for _, bd := range tree.BindingDomains {
		if !strings.Contains(out, bd.Domain) {
			t.Errorf("rendered effective document dropped bindings.%s.apiKey.domains[%s] present in the pre-composition tree", bd.Service, bd.Domain)
		}
	}
}

// story0FixtureSbxenvYAML is a byte-exact copy of Story 0's own
// authoritative fixture, workflow/env/testdata/hostexec-fixture/
// .sbxenv.yaml. Copied rather than read from that path so this package
// (envinfo) takes no directory dependency on workflow/env's testdata —
// envinfo imports no sibling of any kind, including through a shared
// fixture path.
const story0FixtureSbxenvYAML = `schemaVersion: "1"
agent: pix

mcp:
  servers:
    - name: github-mcp
      command: github-mcp-server
      args:
        - --stdio
    - name: warehouse-mcp
      command: warehouse-mcp-server

secrets:
  anthropic:
    ref: op://Personal/Anthropic/api-key

bindings:
  anthropic:
    apiKey:
      domains:
        - api.anthropic.com
`
