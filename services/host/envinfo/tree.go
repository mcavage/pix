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
// mcp.servers sharing a name, or two ports sharing a sandbox port) rather
// than letting merge order silently decide a winner.
func BuildTree(m *Merged) (*Tree, error) {
	t := &Tree{}

	for i, k := range m.Kits {
		t.Kits = append(t.Kits, KitNode{
			KeyPath:  fmt.Sprintf("kits[%d]", i),
			Raw:      k.Raw,
			Resolved: k.Resolved,
			Local:    k.Local,
			Source:   k.Source,
		})
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

	for _, svc := range sortedBindingKeys(m.Bindings) {
		for _, d := range m.Bindings[svc].Domains {
			kp := fmt.Sprintf("bindings.%s.apiKey.domains[%s]", svc, d.Domain)
			t.BindingDomains = append(t.BindingDomains, BindingDomainNode{
				KeyPath: kp,
				Service: svc,
				Domain:  d.Domain,
				Source:  d.Source,
			})
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
