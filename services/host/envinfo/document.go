package envinfo

import "gopkg.in/yaml.v3"

// SchemaVersionV1 is the only native `.sbxenv.yaml` schema version this
// package accepts. docs/design/environments.md §4: "The loader rejects
// unknown fields and unsupported schema versions" — the environment schema
// version is unrelated to Pix's own strict kit-spec v2.
const SchemaVersionV1 = "1"

// Document is one authored `.sbxenv.yaml` file, strictly decoded. Every
// field here is exactly what docs/design/environments.md §5.1's example
// declares; gopkg.in/yaml.v3's KnownFields(true) decoder (see parse.go)
// refuses any key not modeled here, at every nesting level, so this struct
// IS the schema, not a partial reading of it.
type Document struct {
	// Source identifies where this document came from — an authored file's
	// path, or a caller-supplied label for in-memory bytes (ParseBytes). It
	// is never part of the YAML itself (yaml:"-"): decoding a document with
	// a literal top-level `source:` key is refused as an unknown field, the
	// same as any other typo.
	Source string `yaml:"-"`

	SchemaVersion string `yaml:"schemaVersion"`
	Agent         string `yaml:"agent,omitempty"`
	// Name is normally omitted by a registered environment (Pix computes
	// the workspace-specific `pix-*` name) but Story 0's own fixtures
	// author it directly, so the field is real and this package accepts
	// it — it does not decide sandbox naming, it only carries the byte.
	Name string `yaml:"name,omitempty"`

	// Kits is the raw authored `kits:` list, each entry a local relative
	// path, an absolute path, or a remote (git/URL) reference. Parse
	// resolves the local ones against the source file's own directory; see
	// KitEntry.
	Kits []KitEntry `yaml:"kits,omitempty"`

	// SandboxOptions carries sbx sizing knobs (docs example: `memory:
	// 16g`) as literal strings — this package does not interpret units.
	SandboxOptions map[string]string `yaml:"sandboxOptions,omitempty"`

	// Env is sandbox-side environment variables. Values may contain
	// `${VAR}` / `${VAR:-default}` host-interpolation expressions; this
	// package surfaces them (see tree.go) and never resolves them.
	Env map[string]string `yaml:"env,omitempty"`

	Secrets    map[string]SecretRef   `yaml:"secrets,omitempty"`
	Registries map[string]RegistryRef `yaml:"registries,omitempty"`
	Bindings   map[string]Binding     `yaml:"bindings,omitempty"`
	MCP        MCPBlock               `yaml:"mcp,omitempty"`
	Ports      []Port                 `yaml:"ports,omitempty"`
}

// KitEntry is one `kits:` list entry. Raw is exactly the authored string;
// Resolved and Local are filled in by Parse's local-path resolution step
// (paths.go) and are zero/false immediately after decode.
type KitEntry struct {
	Raw      string `yaml:"-"`
	Resolved string `yaml:"-"`
	Local    bool   `yaml:"-"`
}

// UnmarshalYAML accepts the plain scalar string `kits:` entries actually
// are; KitEntry only grows Resolved/Local after Parse's own resolution
// pass, never from the document itself.
func (k *KitEntry) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return err
	}
	k.Raw = raw
	return nil
}

// MarshalYAML round-trips a KitEntry back to its authored scalar form.
func (k KitEntry) MarshalYAML() (interface{}, error) {
	return k.Raw, nil
}

// SecretRef is one `secrets.<name>` record. Pix refuses a literal Value at
// parse time (docs/design/environments.md §5.1, restriction 1) — the field
// exists on this type only so the strict decoder can accept the key long
// enough for that refusal to name it, never so a value can flow through.
type SecretRef struct {
	Ref     string   `yaml:"ref,omitempty"`
	Value   string   `yaml:"value,omitempty"`
	Command []string `yaml:"command,omitempty"`
}

// RegistryRef is one `registries.<host>` record. Like SecretRef, Value is
// modeled only so it can be named and refused. NoVerify weakens TLS
// verification for this registry and must never do so silently — it is
// fingerprinted and surfaced as a host-exec facet (tree.go).
type RegistryRef struct {
	Ref      string   `yaml:"ref,omitempty"`
	Value    string   `yaml:"value,omitempty"`
	Command  []string `yaml:"command,omitempty"`
	NoVerify bool     `yaml:"noVerify,omitempty"`
}

// Binding is one `bindings.<service>` record.
type Binding struct {
	APIKey APIKeyBinding `yaml:"apiKey,omitempty"`
}

// APIKeyBinding is a binding's `apiKey` block: the destination domains a
// credential may be injected into.
type APIKeyBinding struct {
	Domains []string `yaml:"domains,omitempty"`
}

// MCPBlock is the top-level `mcp:` block.
type MCPBlock struct {
	Servers []MCPServer `yaml:"servers,omitempty"`
}

// MCPServer is one `mcp.servers[]` entry. Name is its stable identity
// (tree.go); Command is host execution when present.
type MCPServer struct {
	Name    string   `yaml:"name"`
	URL     string   `yaml:"url,omitempty"`
	Command string   `yaml:"command,omitempty"`
	Args    []string `yaml:"args,omitempty"`
}

// Port is one `ports[]` entry. Sandbox is its stable identity (tree.go).
type Port struct {
	Sandbox int `yaml:"sandbox"`
	Host    int `yaml:"host,omitempty"`
}
