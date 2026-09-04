package envinfo

import (
	"fmt"
	"sort"
)

// Tree is the PRE-COMPOSITION semantic tree BuildTree derives from a
// Merged document: every node addressed by a stable key path (see doc.go,
// "Stable identity"), every authored `${VAR}` interpolation reference
// (never resolved), and every host-execution facet a reviewer must see
// before this environment runs anything.
type Tree struct {
	// Workspace is the authored primary `workspace:` node, or nil when no
	// composed file declared one. It is a REVIEW fact: a Pix launch renders
	// its own run workspace as the effective primary (envinfo/render.go's
	// effectiveWorkspaces), so this node exists so a reviewer can SEE what
	// the file asked for and so RefuseContainment can check the environment
	// root against it — not because it becomes a mount.
	Workspace *WorkspaceNode
	// AdditionalWorkspaces are the authored `additionalWorkspaces[<i>]`
	// nodes. Unlike Workspace these DO reach the effective document as
	// mounts, so they are host access a reviewer is consenting to.
	AdditionalWorkspaces []AdditionalWorkspaceNode

	Kits           []KitNode
	SandboxOptions []ScalarNode
	Env            []ScalarNode
	Secrets        []SecretNode
	Registries     []RegistryNode
	BindingDomains []BindingDomainNode
	MCPServers     []MCPServerNode
	Ports          []PortNode
	Interpolations []Interpolation
	HostExecFacets []HostExecFacet
}

// WorkspaceNode is the singleton `workspace` node. Raw is the authored
// path text, Resolved its source-directory-resolved form (equal to Raw
// when the path still carries an authored ${VAR}: parse.go resolves no
// interpolation). Object records which of the two authored forms was used.
type WorkspaceNode struct {
	KeyPath  string
	Raw      string
	Resolved string
	Clone    bool
	Object   bool
	Source   string
}

// AdditionalWorkspaceNode is one index-addressed
// `additionalWorkspaces[<i>]` node. It is index-addressed rather than
// path-addressed for the same reason kits are: two files may legitimately
// mount the same tree, and merge.go concatenates rather than deduping, so
// a path is not a stable identity here.
type AdditionalWorkspaceNode struct {
	KeyPath  string
	Raw      string
	Resolved string
	ReadOnly bool
	Source   string
}

// KitNode is one index-addressed `kits[<i>]` node.
type KitNode struct {
	KeyPath  string
	Raw      string
	Resolved string
	Local    bool
	Source   string
}

// ScalarNode is one map-addressed `sandboxOptions.<key>` or `env.<key>`
// node.
type ScalarNode struct {
	KeyPath string
	Value   string
	Source  string
}

// SecretNode is one `secrets.<name>` node. The literal Value never reaches
// here — refuseLiteralValues (parse.go) already refused it before Merge
// could ever see it.
type SecretNode struct {
	KeyPath    string
	Name       string
	HasRef     bool
	HasCommand bool
	Source     string
}

// RegistryNode is one `registries.<host>` node.
type RegistryNode struct {
	KeyPath    string
	Host       string
	HasRef     bool
	HasCommand bool
	NoVerify   bool
	Source     string
}

// BindingDomainNode is one identity-addressed
// `bindings.<service>.apiKey.domains[<domain>]` node.
type BindingDomainNode struct {
	KeyPath string
	Service string
	Domain  string
	Source  string
}

// MCPServerNode is one identity-addressed `mcp.servers[<name>]` node.
type MCPServerNode struct {
	KeyPath string
	Name    string
	URL     string
	Command string
	Args    []string
	Source  string
}

// PortNode is one identity-addressed `ports[<sandboxPort>]` node.
type PortNode struct {
	KeyPath string
	Sandbox int
	Host    int
	Source  string
}

// Interpolation is one authored `${VAR}` (or `${VAR:-default}`) reference,
// surfaced by source variable name and destination key path only. There is
// deliberately no field carrying a resolved value anywhere on this type —
// see doc.go's "Interpolation is surfaced, never resolved" and
// docs/design/environments.md §9.1.
type Interpolation struct {
	Var     string
	Default *string
	KeyPath string
}

// HostExecFacet flags one host-execution-relevant fact so it cannot pass
// review unnoticed: a secret or registry `command` (docs/design/
// environments.md §5.1 restriction 2, "A secret or registry `command` is
// host execution"), a local MCP server `command`, or a registry `noVerify`
// (restriction 3, "fingerprinted and visible in review; it never silently
// weakens a proof").
type HostExecFacet struct {
	KeyPath string
	Kind    string // "secret-command" | "registry-command" | "registry-no-verify" | "mcp-command"
}

// BuildTree derives the semantic tree from a Merged document. It refuses a
// duplicate identity-addressed key path (an unnamed mcp.servers entry, two
// mcp.servers sharing a name, two ports sharing a sandbox port, or two
// domains within one service's apiKey.domains — including a duplicate
// introduced only by merge.go's list-concatenation across files, never
// just one file repeating itself) rather than letting merge order silently
// decide a winner. It also refuses a binding whose effective, post-merge
// domain list is empty (ErrEmptyBindingDomains): upstream sbx already
// treats a zero-domain binding as a functionless no-op (docs/upstream/
// sbx-0.37-binding-warning.md: "no domains allowed by your bindings; not
// injecting"), so silently carrying it forward as an empty node list would
// make the declared service evaporate from the tree with no trace a
// reviewer could see — a precise refusal is preferred over inventing a
// tree node upstream gives no meaning to.
//
// Every authored `${VAR}` (docs/design/environments.md §9.1) is scanned on
// every string-bearing node this function builds, not only the fields an
// earlier pass happened to cover: sandboxOptions/env values, secret and
// registry ref/command, a kit's raw authored path, an mcp server's
// url/command AND each of its args (index-addressed, e.g.
// "mcp.servers[x].args[0]"), and a binding's destination domain. The two
// identity-derived exceptions are deliberate, not gaps: a map/list key used
// only for addressing (a secret/registry/binding-service name, an mcp
// server name, a kit's derived Resolved path) is never itself an authored
// value a caller would want interpolated — it is the address the
// interpolated value's own KeyPath is built from.
func BuildTree(m *Merged) (*Tree, error) {
	t := &Tree{}

	if m.Workspace.Present {
		t.Workspace = &WorkspaceNode{
			KeyPath:  "workspace",
			Raw:      m.Workspace.Raw,
			Resolved: m.Workspace.Resolved,
			Clone:    m.Workspace.Clone,
			Object:   m.Workspace.Object,
			Source:   m.WorkspaceSource,
		}
		t.Interpolations = append(t.Interpolations, scanInterpolations(m.Workspace.Raw, "workspace")...)
	}

	for i, ws := range m.AdditionalWorkspaces {
		kp := fmt.Sprintf("additionalWorkspaces[%d]", i)
		t.AdditionalWorkspaces = append(t.AdditionalWorkspaces, AdditionalWorkspaceNode{
			KeyPath:  kp,
			Raw:      ws.Path,
			Resolved: ws.Resolved,
			ReadOnly: ws.ReadOnly,
			Source:   ws.Source,
		})
		t.Interpolations = append(t.Interpolations, scanInterpolations(ws.Path, kp)...)
	}

	for i, k := range m.Kits {
		kp := fmt.Sprintf("kits[%d]", i)
		t.Kits = append(t.Kits, KitNode{
			KeyPath:  kp,
			Raw:      k.Raw,
			Resolved: k.Resolved,
			Local:    k.Local,
			Source:   k.Source,
		})
		t.Interpolations = append(t.Interpolations, scanInterpolations(k.Raw, kp)...)
	}

	for _, key := range sortedStringKeys(m.SandboxOptions) {
		v := m.SandboxOptions[key]
		kp := "sandboxOptions." + key
		t.SandboxOptions = append(t.SandboxOptions, ScalarNode{KeyPath: kp, Value: v.Value, Source: v.Source})
		t.Interpolations = append(t.Interpolations, scanInterpolations(v.Value, kp)...)
	}

	for _, key := range sortedStringKeys(m.Env) {
		v := m.Env[key]
		kp := "env." + key
		t.Env = append(t.Env, ScalarNode{KeyPath: kp, Value: v.Value, Source: v.Source})
		t.Interpolations = append(t.Interpolations, scanInterpolations(v.Value, kp)...)
	}

	for _, name := range sortedSecretKeys(m.Secrets) {
		s := m.Secrets[name]
		kp := "secrets." + name
		hasCommand := len(s.Command) > 0
		t.Secrets = append(t.Secrets, SecretNode{
			KeyPath:    kp,
			Name:       name,
			HasRef:     s.Ref != "",
			HasCommand: hasCommand,
			Source:     s.Source,
		})
		if hasCommand {
			t.HostExecFacets = append(t.HostExecFacets, HostExecFacet{KeyPath: kp, Kind: "secret-command"})
		}
		t.Interpolations = append(t.Interpolations, scanInterpolations(s.Ref, kp+".ref")...)
		for i, c := range s.Command {
			t.Interpolations = append(t.Interpolations, scanInterpolations(c, fmt.Sprintf("%s.command[%d]", kp, i))...)
		}
	}

	for _, host := range sortedRegistryKeys(m.Registries) {
		r := m.Registries[host]
		kp := "registries." + host
		hasCommand := len(r.Command) > 0
		t.Registries = append(t.Registries, RegistryNode{
			KeyPath:    kp,
			Host:       host,
			HasRef:     r.Ref != "",
			HasCommand: hasCommand,
			NoVerify:   r.NoVerify,
			Source:     r.Source,
		})
		if hasCommand {
			t.HostExecFacets = append(t.HostExecFacets, HostExecFacet{KeyPath: kp, Kind: "registry-command"})
		}
		if r.NoVerify {
			t.HostExecFacets = append(t.HostExecFacets, HostExecFacet{KeyPath: kp, Kind: "registry-no-verify"})
		}
		t.Interpolations = append(t.Interpolations, scanInterpolations(r.Ref, kp+".ref")...)
		for i, c := range r.Command {
			t.Interpolations = append(t.Interpolations, scanInterpolations(c, fmt.Sprintf("%s.command[%d]", kp, i))...)
		}
	}

	// A service's effective, post-merge domain list feeds two checks a
	// plain range loop cannot enforce on its own: it must never be empty
	// (finding 3 — see the ErrEmptyBindingDomains doc comment for why
	// refusal beats a zero-domain tree node) and it must never repeat one
	// domain twice under the SAME identity-addressed key path (finding 2).
	// The duplicate can be authored directly in one file or introduced
	// purely by merge.go's documented list-concatenation across files
	// (merge.go: "WITHIN one service, the one nested list field
	// (apiKey.domains) concatenates"); both are the same collision from
	// this function's point of view, since both would otherwise silently
	// emit two BindingDomainNode entries sharing one key path — the exact
	// "which entry wins" ambiguity doc.go's "Stable identity" section
	// refuses for mcp.servers and ports.
	for _, svc := range sortedBindingKeys(m.Bindings) {
		domains := m.Bindings[svc].Domains
		if len(domains) == 0 {
			return nil, fmt.Errorf("%w: bindings.%s.apiKey.domains", ErrEmptyBindingDomains, svc)
		}
		seenDomains := map[string]struct{}{}
		for _, d := range domains {
			kp := fmt.Sprintf("bindings.%s.apiKey.domains[%s]", svc, d.Domain)
			if _, dup := seenDomains[d.Domain]; dup {
				return nil, fmt.Errorf("%w: %s", ErrDuplicateIdentity, kp)
			}
			seenDomains[d.Domain] = struct{}{}
			t.BindingDomains = append(t.BindingDomains, BindingDomainNode{
				KeyPath: kp,
				Service: svc,
				Domain:  d.Domain,
				Source:  d.Source,
			})
			t.Interpolations = append(t.Interpolations, scanInterpolations(d.Domain, kp)...)
		}
	}

	seenServers := map[string]string{} // name -> first key path it was seen at
	for i, srv := range m.MCPServers {
		if srv.Name == "" {
			return nil, fmt.Errorf("envinfo: mcp.servers[%d]: missing required name", i)
		}
		kp := fmt.Sprintf("mcp.servers[%s]", srv.Name)
		if _, dup := seenServers[srv.Name]; dup {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateIdentity, kp)
		}
		seenServers[srv.Name] = kp
		t.MCPServers = append(t.MCPServers, MCPServerNode{
			KeyPath: kp,
			Name:    srv.Name,
			URL:     srv.URL,
			Command: srv.Command,
			Args:    srv.Args,
			Source:  srv.Source,
		})
		if srv.Command != "" {
			t.HostExecFacets = append(t.HostExecFacets, HostExecFacet{KeyPath: kp, Kind: "mcp-command"})
		}
		t.Interpolations = append(t.Interpolations, scanInterpolations(srv.URL, kp+".url")...)
		t.Interpolations = append(t.Interpolations, scanInterpolations(srv.Command, kp+".command")...)
		for i, a := range srv.Args {
			t.Interpolations = append(t.Interpolations, scanInterpolations(a, fmt.Sprintf("%s.args[%d]", kp, i))...)
		}
	}

	seenPorts := map[int]string{}
	for _, p := range m.Ports {
		kp := fmt.Sprintf("ports[%d]", p.Sandbox)
		if _, dup := seenPorts[p.Sandbox]; dup {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateIdentity, kp)
		}
		seenPorts[p.Sandbox] = kp
		t.Ports = append(t.Ports, PortNode{
			KeyPath: kp,
			Sandbox: p.Sandbox,
			Host:    p.Host,
			Source:  p.Source,
		})
	}

	return t, nil
}

func sortedStringKeys(m map[string]MergedScalar) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedSecretKeys(m map[string]MergedSecret) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedRegistryKeys(m map[string]MergedRegistry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedBindingKeys(m map[string]MergedBinding) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
