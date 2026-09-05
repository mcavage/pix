package envinfo

// upstream_schema_test.go — the PLANTED upstream schema.
//
// Every other test in this package checks Pix against Pix: the renderer
// against a golden Pix itself wrote, the parser against a fixture Pix
// itself authored. That is exactly how an invented top-level `workspaces:`
// key survived a full green suite and then failed a real host run:
//
//	run-20260829-161325-d3c9a7be, candidate 54152f2b
//	$ sbx env create .../effective.sbxenv.yaml
//	field workspaces not found in type sbxenv.Config   (line 9)
//
// The types below are therefore transcribed field-for-field from Docker's
// OFFICIAL reference for the file format — not from any Pix type, and not
// from a Pix test fixture:
//
//	https://docs.docker.com/ai/sandboxes/configuration/environment-files/
//	("File reference" → "Top-level fields", "workspace",
//	 "additionalWorkspaces", "sandboxOptions", "secrets", "bindings",
//	 "registries", "mcp", "ports"), sbx 0.39.
//
// They stand in for `sbxenv.Config`, the upstream Go type whose strict
// loader ("The loader rejects unknown fields and unsupported schema
// versions") produced the failure above. A rendered document that does not
// strict-decode into them is a create that fails on a host, whatever the
// Pix-owned goldens say.

import (
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// upstreamConfig is the top-level field set of the official reference
// table. Field names, YAML keys and nesting are upstream's; nothing here
// may be adjusted to match what Pix happens to emit.
type upstreamConfig struct {
	SchemaVersion        string                        `yaml:"schemaVersion"`
	Name                 string                        `yaml:"name"`
	Agent                string                        `yaml:"agent"`
	Kits                 []string                      `yaml:"kits"`
	Workspace            *upstreamWorkspace            `yaml:"workspace"`
	AdditionalWorkspaces []upstreamAdditionalWorkspace `yaml:"additionalWorkspaces"`
	Env                  map[string]string             `yaml:"env"`
	SandboxOptions       *upstreamSandboxOptions       `yaml:"sandboxOptions"`
	Secrets              map[string]upstreamSecret     `yaml:"secrets"`
	Bindings             map[string]upstreamBinding    `yaml:"bindings"`
	Registries           map[string]upstreamRegistry   `yaml:"registries"`
	MCP                  *upstreamMCP                  `yaml:"mcp"`
	Ports                []upstreamPort                `yaml:"ports"`
}

// upstreamWorkspace is the `workspace` field: "string or object". The
// object form carries exactly `path` and `clone` — there is NO `readOnly`
// on the primary workspace anywhere in upstream's table.
type upstreamWorkspace struct {
	Path  string
	Clone bool
	// Scalar records that this workspace was authored in the string form,
	// so a test can assert BOTH accepted forms rather than only the one
	// Pix renders.
	Scalar bool
}

// UnmarshalYAML models upstream's own strictness by hand: yaml.v3's
// KnownFields(true) does not reach through a custom unmarshaler, so an
// unknown key inside the object form is refused explicitly here. Without
// that, this planted type would be LOOSER than the loader it stands in
// for, and would happily accept a `readOnly:` under the primary workspace
// that a real `sbx env create` rejects.
func (w *upstreamWorkspace) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		w.Scalar = true
		return node.Decode(&w.Path)
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: workspace must be a string or an object", node.Line)
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, val := node.Content[i].Value, node.Content[i+1]
		switch key {
		case "path":
			if err := val.Decode(&w.Path); err != nil {
				return err
			}
		case "clone":
			if err := val.Decode(&w.Clone); err != nil {
				return err
			}
		default:
			return fmt.Errorf("line %d: field %s not found in type sbxenv.Workspace", node.Content[i].Line, key)
		}
	}
	return nil
}

// upstreamAdditionalWorkspace is one `additionalWorkspaces[]` entry:
// `path` (required) and `readOnly` (default false). There is NO `clone`
// here — upstream mounts additional workspaces directly "even when the
// primary workspace uses clone mode". Plain struct fields, so the parent
// decoder's KnownFields(true) refuses an unknown key on its own.
type upstreamAdditionalWorkspace struct {
	Path     string `yaml:"path"`
	ReadOnly bool   `yaml:"readOnly"`
}

type upstreamSandboxOptions struct {
	Template   string `yaml:"template"`
	Memory     string `yaml:"memory"`
	CPUs       int    `yaml:"cpus"`
	PullPolicy string `yaml:"pullPolicy"`
	Profile    string `yaml:"profile"`
}

type upstreamSecret struct {
	Value    string `yaml:"value"`
	Ref      string `yaml:"ref"`
	Command  string `yaml:"command"`
	Refresh  string `yaml:"refresh"`
	Backend  string `yaml:"backend"`
	NoVerify bool   `yaml:"noVerify"`
}

type upstreamBinding struct {
	APIKey *upstreamBindingBlock `yaml:"apiKey"`
	OAuth  *upstreamBindingBlock `yaml:"oauth"`
}

type upstreamBindingBlock struct {
	Domains []string `yaml:"domains"`
}

type upstreamRegistry struct {
	Secret   *upstreamSecret `yaml:"secret"`
	Username *upstreamSecret `yaml:"username"`
}

type upstreamMCP struct {
	Servers []upstreamMCPServer `yaml:"servers"`
}

type upstreamMCPServer struct {
	Name    string   `yaml:"name"`
	URL     string   `yaml:"url"`
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
}

type upstreamPort struct {
	Sandbox  int    `yaml:"sandbox"`
	Host     int    `yaml:"host"`
	Protocol string `yaml:"protocol"`
	HostIP   string `yaml:"hostIP"`
}

// strictDecodeUpstream decodes data the way sbx's own loader does:
// unknown fields refused at every level.
func strictDecodeUpstream(data []byte) (*upstreamConfig, error) {
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	var cfg upstreamConfig
	if err := dec.Decode(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// conformanceFacts is the widest workspace-bearing document this package
// can render: an authored primary workspace and two authored additional
// workspaces (one read-only), a Pix primary run workspace that overrides
// the authored one, the unconditional personal-context workspace, and a
// runtime mount.
func conformanceFacts(t *testing.T) RuntimeFacts {
	t.Helper()
	doc, err := ParseBytes([]byte(`schemaVersion: "1"
agent: pix
workspace:
  path: ./web-app
  clone: true
additionalWorkspaces:
  - path: ./shared-components
  - path: ./architecture-docs
    readOnly: true
`), "/env/.sbxenv.yaml", "/env")
	if err != nil {
		t.Fatalf("parse authored workspace document: %v", err)
	}
	return RuntimeFacts{
		Document:                 doc,
		SandboxName:              "pix-conformance-00000000",
		Template:                 "docker.io/mcavage/pix:v0.0.0",
		PullPolicy:               "missing",
		PrimaryWorkspace:         WorkspaceFact{Path: "/home/user/project"},
		PersonalContextWorkspace: WorkspaceFact{Path: "/home/user/.local/share/pix/context"},
		AdditionalWorkspaces:     []WorkspaceFact{{Path: "/home/user/pack/skills", ReadOnly: true}},
		MixinKit:                 "/tmp/pix-mixin-00000000",
	}
}

// TestEffectiveDocument_StrictParsesAsUpstreamSchema is the check that was
// missing: render, then feed the bytes to the planted upstream loader.
func TestEffectiveDocument_StrictParsesAsUpstreamSchema(t *testing.T) {
	data, err := RenderEffective(conformanceFacts(t))
	if err != nil {
		t.Fatalf("RenderEffective: %v", err)
	}
	cfg, err := strictDecodeUpstream(data)
	if err != nil {
		t.Fatalf("rendered effective document is not loadable by sbx 0.39's schema: %v\n--- rendered ---\n%s", err, data)
	}
	if cfg.Workspace == nil {
		t.Fatalf("rendered document declares no primary workspace:\n%s", data)
	}
	if cfg.Workspace.Path != "/home/user/project" {
		t.Errorf("primary workspace path = %q, want the run's own project workspace %q", cfg.Workspace.Path, "/home/user/project")
	}
	// The authored `workspace:` is a parse/review fact only. Pix's primary
	// run workspace overrides it by design, and it must never reappear as
	// an additional (writable) mount instead.
	var paths []string
	for _, ws := range cfg.AdditionalWorkspaces {
		paths = append(paths, ws.Path)
		if ws.Path == "/env/web-app" {
			t.Errorf("authored workspace /env/web-app was promoted to a writable additional mount; it is a review fact, not a mount")
		}
	}
	want := []string{
		"/env/shared-components",
		"/env/architecture-docs",
		"/home/user/.local/share/pix/context",
		"/home/user/pack/skills",
	}
	if len(paths) != len(want) {
		t.Fatalf("additionalWorkspaces = %v, want %v", paths, want)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("additionalWorkspaces[%d] = %q, want %q", i, paths[i], want[i])
		}
	}
	if !cfg.AdditionalWorkspaces[1].ReadOnly {
		t.Errorf("authored readOnly on architecture-docs was dropped; a read-only mount must never render writable")
	}
}

// TestEffectiveDocument_NoTopLevelWorkspacesKey is the direct regression
// for the host failure. It asserts the exact absence the host proved was
// required, and names the observed error so a future reader recognizes it.
func TestEffectiveDocument_NoTopLevelWorkspacesKey(t *testing.T) {
	data, err := RenderEffective(conformanceFacts(t))
	if err != nil {
		t.Fatalf("RenderEffective: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "workspaces:") {
			t.Fatalf("rendered a top-level `workspaces:` key; a real `sbx env create` answers "+
				"`field workspaces not found in type sbxenv.Config`\n--- rendered ---\n%s", data)
		}
	}
	if !strings.Contains(string(data), "\nworkspace:\n") {
		t.Errorf("rendered document has no singular `workspace:` block:\n%s", data)
	}
}

// TestUpstreamSchema_AcceptsBothAuthoredWorkspaceForms proves the planted
// type is upstream's union ("string or object"), so the authored-side
// tests below are checking against the real grammar and not against a
// narrowed copy of it.
func TestUpstreamSchema_AcceptsBothAuthoredWorkspaceForms(t *testing.T) {
	scalar, err := strictDecodeUpstream([]byte("schemaVersion: \"1\"\nagent: pix\nworkspace: ./web-app\n"))
	if err != nil {
		t.Fatalf("scalar workspace form rejected by planted upstream schema: %v", err)
	}
	if !scalar.Workspace.Scalar || scalar.Workspace.Path != "./web-app" {
		t.Errorf("scalar workspace decoded as %+v", scalar.Workspace)
	}
	object, err := strictDecodeUpstream([]byte("schemaVersion: \"1\"\nagent: pix\nworkspace:\n  path: ./web-app\n  clone: true\n"))
	if err != nil {
		t.Fatalf("object workspace form rejected by planted upstream schema: %v", err)
	}
	if object.Workspace.Scalar || !object.Workspace.Clone {
		t.Errorf("object workspace decoded as %+v", object.Workspace)
	}
	if _, err := strictDecodeUpstream([]byte("schemaVersion: \"1\"\nworkspace:\n  path: ./x\n  readOnly: true\n")); err == nil {
		t.Errorf("planted upstream schema accepted `readOnly` under the primary workspace; upstream's table has no such field")
	}
}

// TestUpstreamSchema_KnownNestedDivergences is a LEDGER, not a wish. Two
// nested shapes this package renders are still not upstream's, and the
// host run never reached them (no fixture in it declared a secret or a
// registry, so the create failed on line 9's `workspaces:` first). Pinning
// them here keeps them from being mistaken for proven-good on the next
// read, and makes whoever fixes one delete its case deliberately.
//
// Recorded in docs/upstream/sbx-0.39-environments.md §17.
func TestUpstreamSchema_KnownNestedDivergences(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{
			name: "registries take a nested secret source upstream, not a bare ref/noVerify",
			doc:  "schemaVersion: \"1\"\nregistries:\n  ghcr.io:\n    ref: op://Vault/Item/field\n    noVerify: true\n",
			want: "field ref not found",
		},
		{
			name: "a secret command is a single shell string upstream, not an argv list",
			doc:  "schemaVersion: \"1\"\nsecrets:\n  svc:\n    command:\n      - /usr/bin/op\n      - read\n",
			want: "cannot unmarshal !!seq into string",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := strictDecodeUpstream([]byte(tc.doc))
			if err == nil {
				t.Fatalf("this divergence is GONE — upstream now accepts the Pix shape. Delete this case and extend the conformance fixture instead of leaving a stale ledger entry")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("divergence changed shape: got %v, want an error containing %q", err, tc.want)
			}
		})
	}
}
