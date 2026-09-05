package envinfo

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

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

	// Workspace is the authored `workspace:` — upstream's PRIMARY workspace,
	// authored either as a plain path string or as an object carrying
	// `path`/`clone`. There is no `readOnly` on it: upstream's reference
	// table for `workspace` lists exactly those two fields.
	//
	// An OMITTED `workspace:` is left zero (Present == false) and is NOT
	// filled in with upstream's documented default ("first file's
	// directory"). Pix's effective document always declares its own primary
	// run workspace, so upstream's default never applies to a document Pix
	// renders; materializing it here would instead invent a mount of the
	// environment's own source directory — the exact shape §5.1 restriction
	// 4 refuses.
	Workspace Workspace `yaml:"workspace,omitempty"`

	// AdditionalWorkspaces is the authored `additionalWorkspaces:` list:
	// extra host directories mounted after the primary one, each `path`
	// (required) plus `readOnly` (default false). There is no `clone` on an
	// additional workspace — upstream mounts them directly "even when the
	// primary workspace uses clone mode".
	AdditionalWorkspaces []AdditionalWorkspace `yaml:"additionalWorkspaces,omitempty"`

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

// Workspace is the authored `workspace:` in either accepted form. Raw is
// exactly the authored path text; Resolved is filled in by Parse's own
// resolution step against the source file's directory and is empty
// immediately after decode — the same authored-vs-resolved split KitEntry
// already uses, for the same reason (a reviewer must be able to see what
// was written, not only what it became).
type Workspace struct {
	// Present reports that the key was authored at all. It is the ONLY way
	// to tell `workspace: ""` from an omitted `workspace:`, and the
	// omitted case must never become a mount (see Document.Workspace).
	Present bool `yaml:"-"`
	// Object reports the authored form: true for the `{path, clone}`
	// mapping, false for the plain string. Round-tripping needs it, and a
	// review that reprints the authored file must not silently rewrite one
	// form into the other.
	Object   bool   `yaml:"-"`
	Raw      string `yaml:"-"`
	Resolved string `yaml:"-"`
	Clone    bool   `yaml:"-"`
}

// UnmarshalYAML accepts upstream's documented union — a scalar path or an
// object with exactly `path` and `clone` — and refuses any other key
// itself. The explicit key loop is load-bearing: gopkg.in/yaml.v3's
// KnownFields(true) does NOT reach through a custom unmarshaler (yaml.Node's
// own Decode always runs with knownFields off, the same limitation parse.go
// already documents for its schemaVersion probe), so without this loop this
// one subtree would be the single place in the schema where a typo — most
// consequentially a `readOnly:` upstream does not accept on the primary
// workspace — decoded silently.
func (w *Workspace) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if err := node.Decode(&w.Raw); err != nil {
			return err
		}
		w.Present = true
		return nil
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, val := node.Content[i], node.Content[i+1]
			switch key.Value {
			case "path":
				if err := val.Decode(&w.Raw); err != nil {
					return err
				}
			case "clone":
				if err := val.Decode(&w.Clone); err != nil {
					return err
				}
			default:
				return fmt.Errorf("line %d: field %s not found in type envinfo.Workspace (workspace accepts only path, clone)", key.Line, key.Value)
			}
		}
		w.Present, w.Object = true, true
		return nil
	default:
		return fmt.Errorf("line %d: workspace must be a path string or an object with path/clone", node.Line)
	}
}

// MarshalYAML round-trips a Workspace back to the form it was authored in.
func (w Workspace) MarshalYAML() (interface{}, error) {
	if !w.Present {
		return nil, nil
	}
	if !w.Object {
		return w.Raw, nil
	}
	return struct {
		Path  string `yaml:"path,omitempty"`
		Clone bool   `yaml:"clone,omitempty"`
	}{Path: w.Raw, Clone: w.Clone}, nil
}

// AdditionalWorkspace is one authored `additionalWorkspaces[]` entry. It
// is a plain struct with ordinary yaml tags — no custom unmarshaler — so
// the parent decoder's KnownFields(true) refuses an unknown nested key
// (`clone:`, say) on its own, with no hand-written loop to keep in sync.
type AdditionalWorkspace struct {
	Path     string `yaml:"path"`
	ReadOnly bool   `yaml:"readOnly,omitempty"`
	// Resolved is Parse's absolute form of Path, resolved against the
	// source file's own directory. Zero immediately after decode.
	Resolved string `yaml:"-"`
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

// ── the ENCODE half of the same schema (envinfo/render.go's output) ──────
//
// These types live HERE, beside Document, for the reason
// TestOnlyEnvinfoDecodesNativeEnvYAML (arch_test.go) exists: ONE file in
// this module declares the native `.sbxenv.yaml` field set, so a shape
// Pix WRITES can never quietly drift from the shape Pix READS. They are
// unexported because only RenderEffective builds one.

// effectiveDocument is the rendered shape: upstream's `.sbxenv.yaml`
// schema, in one fixed field order. It is deliberately a SEPARATE type
// from Document — Document is the strict decoder for an AUTHORED file
// (document.go: "this struct IS the schema"), and the effective file
// carries Pix-owned facts (a primary workspace Pix chose, the runtime
// mounts it adds) an authored file does not declare.
//
// The workspace fields are upstream's SINGULAR `workspace:` plus
// `additionalWorkspaces:` — never a top-level `workspaces:` list. An
// earlier version of this type invented that list and every golden in
// this package agreed with it, right up until a real `sbx env create`
// answered `field workspaces not found in type sbxenv.Config` (run
// run-20260829-161325-d3c9a7be, line 9 of the generated file). See
// upstream_schema_test.go, which strict-decodes this renderer's output
// through a schema transcribed from Docker's own reference rather than
// from anything in this repository.
type effectiveDocument struct {
	SchemaVersion string `yaml:"schemaVersion"`
	Agent         string `yaml:"agent,omitempty"`
	Name          string `yaml:"name,omitempty"`
	// Workspace is a pointer so "no primary workspace fact at all" renders
	// no key, rather than an empty `workspace: {path: ""}` object a loader
	// would read as a mount of the current directory.
	Workspace            *effectiveWorkspace            `yaml:"workspace,omitempty"`
	AdditionalWorkspaces []effectiveAdditionalWorkspace `yaml:"additionalWorkspaces,omitempty"`

	Kits           []string                     `yaml:"kits,omitempty"`
	SandboxOptions map[string]string            `yaml:"sandboxOptions,omitempty"`
	Env            map[string]string            `yaml:"env"`
	Secrets        map[string]effectiveSecret   `yaml:"secrets,omitempty"`
	Registries     map[string]effectiveRegistry `yaml:"registries,omitempty"`
	Bindings       map[string]Binding           `yaml:"bindings,omitempty"`
	MCP            *MCPBlock                    `yaml:"mcp,omitempty"`
	Ports          []Port                       `yaml:"ports,omitempty"`
}

// effectiveWorkspace is the PRIMARY workspace in upstream's object form:
// `path` plus `clone`, and nothing else. `readOnly` is deliberately
// absent — upstream's `workspace` table has no such field, so a read-only
// primary workspace is not expressible at all and RenderEffective refuses
// one outright (ErrReadOnlyPrimaryWorkspace) rather than dropping the bit
// and mounting the tree writable.
//
// clone is rendered even when false: this document is the declaration a
// create is fingerprinted against (E2.2), so an omitted-because-false
// field would make "direct mount" and "unset" indistinguishable to a
// later reader.
type effectiveWorkspace struct {
	Path  string `yaml:"path"`
	Clone bool   `yaml:"clone"`
}

// effectiveAdditionalWorkspace is one `additionalWorkspaces[]` entry:
// `path` plus `readOnly`, and nothing else. `clone` is absent for the
// mirror-image reason — upstream mounts additional workspaces directly
// even under clone mode, so a clone request here has no representation and
// is refused (ErrClonedAdditionalWorkspace) rather than silently becoming a
// direct read-write mount of the host tree.
type effectiveAdditionalWorkspace struct {
	Path     string `yaml:"path"`
	ReadOnly bool   `yaml:"readOnly"`
}

// effectiveSecret/effectiveRegistry mirror SecretRef/RegistryRef with the
// `value` field STRUCTURALLY ABSENT rather than merely omitted-when-empty.
// Pix refuses a literal value at parse time (§5.1, restriction 1); making
// it unrepresentable here means no future edit to this renderer can put a
// resolved value into a file on disk (render_test.go's
// TestRenderEffective_NoSecretValues).
type effectiveSecret struct {
	Ref     string   `yaml:"ref,omitempty"`
	Command []string `yaml:"command,omitempty"`
}

type effectiveRegistry struct {
	Ref     string   `yaml:"ref,omitempty"`
	Command []string `yaml:"command,omitempty"`
	// NoVerify is rendered whenever set; it is never silently dropped —
	// §5.1 restriction 3 requires it stay visible wherever it applies.
	NoVerify bool `yaml:"noVerify,omitempty"`
}
