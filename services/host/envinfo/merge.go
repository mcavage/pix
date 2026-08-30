package envinfo

// Merge composes one or more already-Parse'd documents using the exact
// semantics docs/design/environments.md §4 documents for upstream `sbx
// env`: "Multiple files compose in argument order. Nested maps merge by
// key, lists concatenate, and later files replace scalar values." docs
// live at:
//
//   - top-level scalars (schemaVersion, agent, name): later non-empty
//     value replaces.
//   - kits, additionalWorkspaces, mcp.servers, ports: plain lists —
//     concatenate in argument order.
//   - workspace: a later file that AUTHORS one replaces the earlier one
//     WHOLESALE, in whichever form it authored (scalar or object). It is
//     the schema's one scalar-or-object union, and half-merging it (a
//     `clone:` from one file onto a `path:` from another) would
//     manufacture a primary workspace neither file declared — the same
//     reading secrets/registries already get below. A file that omits
//     `workspace:` contributes nothing and never clears an earlier one.
//   - sandboxOptions, env: maps of scalars — merge by key, later value
//     replaces.
//   - secrets, registries: maps merge by key, but a key collision replaces
//     the WHOLE record rather than merging sub-fields. This is a
//     deliberate, documented departure from generic recursive map-merge:
//     ref/value/command are mutually exclusive ways to source one secret,
//     and deep-merging a `ref` from one file with a `command` from
//     another would silently manufacture an ambiguous record neither file
//     ever authored. Two documents naming the same secret is already an
//     unusual composition; replacing it wholesale is the safe reading.
//   - bindings: maps merge by key (service name); WITHIN one service, the
//     one nested list field (apiKey.domains) concatenates rather than
//     replacing, since domains are additive credential-injection targets
//     with no exclusivity concern.
//
// Every contributed value keeps its source document's Source string
// (Merged's *Source/Source fields), so BuildTree can attribute every node
// in the semantic tree back to the exact file that introduced it — stable
// under further composition, since growth only ever appends (see
// merge_test.go's list-growth attribution case).
func Merge(docs ...*Document) (*Merged, error) {
	if len(docs) == 0 {
		return nil, errNoDocuments
	}
	m := &Merged{
		SandboxOptions: map[string]MergedScalar{},
		Env:            map[string]MergedScalar{},
		Secrets:        map[string]MergedSecret{},
		Registries:     map[string]MergedRegistry{},
		Bindings:       map[string]MergedBinding{},
	}
	for _, d := range docs {
		if d.SchemaVersion != "" {
			m.SchemaVersion, m.SchemaVersionSource = d.SchemaVersion, d.Source
		}
		if d.Agent != "" {
			m.Agent, m.AgentSource = d.Agent, d.Source
		}
		if d.Name != "" {
			m.Name, m.NameSource = d.Name, d.Source
		}
		if d.Workspace.Present {
			m.Workspace, m.WorkspaceSource = d.Workspace, d.Source
		}
		for _, ws := range d.AdditionalWorkspaces {
			m.AdditionalWorkspaces = append(m.AdditionalWorkspaces, MergedAdditionalWorkspace{AdditionalWorkspace: ws, Source: d.Source})
		}
		for _, k := range d.Kits {
			m.Kits = append(m.Kits, MergedKit{KitEntry: k, Source: d.Source})
		}
		for key, v := range d.SandboxOptions {
			m.SandboxOptions[key] = MergedScalar{Value: v, Source: d.Source}
		}
		for key, v := range d.Env {
			m.Env[key] = MergedScalar{Value: v, Source: d.Source}
		}
		for name, v := range d.Secrets {
			m.Secrets[name] = MergedSecret{SecretRef: v, Source: d.Source}
		}
		for host, v := range d.Registries {
			m.Registries[host] = MergedRegistry{RegistryRef: v, Source: d.Source}
		}
		for svc, b := range d.Bindings {
			existing := m.Bindings[svc]
			for _, domain := range b.APIKey.Domains {
				existing.Domains = append(existing.Domains, MergedBindingDomain{Domain: domain, Source: d.Source})
			}
			m.Bindings[svc] = existing
		}
		for _, srv := range d.MCP.Servers {
			m.MCPServers = append(m.MCPServers, MergedMCPServer{MCPServer: srv, Source: d.Source})
		}
		for _, p := range d.Ports {
			m.Ports = append(m.Ports, MergedPort{Port: p, Source: d.Source})
		}
	}
	if m.SchemaVersion == "" {
		return nil, errNoSchemaVersion
	}
	return m, nil
}

// Merged is the composed result of one or more Documents, with per-node
// provenance. It is the input to BuildTree.
type Merged struct {
	SchemaVersion       string
	SchemaVersionSource string
	Agent               string
	AgentSource         string
	Name                string
	NameSource          string

	// Workspace is the last AUTHORED primary workspace, and
	// WorkspaceSource the file that authored it. Both are zero when no
	// composed file declared one — never upstream's "first file's
	// directory" default, which this package deliberately does not
	// materialize (Document.Workspace).
	Workspace       Workspace
	WorkspaceSource string
	// AdditionalWorkspaces is every authored entry from every file, in
	// argument order, each annotated with the file that contributed it.
	AdditionalWorkspaces []MergedAdditionalWorkspace

	Kits           []MergedKit
	SandboxOptions map[string]MergedScalar
	Env            map[string]MergedScalar
	Secrets        map[string]MergedSecret
	Registries     map[string]MergedRegistry
	Bindings       map[string]MergedBinding
	MCPServers     []MergedMCPServer
	Ports          []MergedPort
}

// MergedAdditionalWorkspace is one additionalWorkspaces[i] entry annotated
// with the source file that contributed it.
type MergedAdditionalWorkspace struct {
	AdditionalWorkspace
	Source string
}

// MergedKit is one Kits[i] entry annotated with the source file that
// contributed it.
type MergedKit struct {
	KitEntry
	Source string
}

// MergedScalar is one map value (sandboxOptions/env) annotated with the
// source file whose later-wins write is currently in effect.
type MergedScalar struct {
	Value  string
	Source string
}

// MergedSecret is one secrets[name] record annotated with its source file.
type MergedSecret struct {
	SecretRef
	Source string
}

// MergedRegistry is one registries[host] record annotated with its source
// file.
type MergedRegistry struct {
	RegistryRef
	Source string
}

// MergedBindingDomain is one domain contributed to a service's binding.
type MergedBindingDomain struct {
	Domain string
	Source string
}

// MergedBinding is one bindings[service] record: its concatenated,
// per-item-attributed domain list.
type MergedBinding struct {
	Domains []MergedBindingDomain
}

// MergedMCPServer is one mcp.servers[] entry annotated with its source
// file.
type MergedMCPServer struct {
	MCPServer
	Source string
}

// MergedPort is one ports[] entry annotated with its source file.
type MergedPort struct {
	Port
	Source string
}
