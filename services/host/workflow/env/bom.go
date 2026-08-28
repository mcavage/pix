// bom.go — E1.8's host bill of materials: the canonical, PURE-FUNCTION-OF-
// THE-DOCUMENT enumeration of everything an environment would run on, or
// hand a credential to on, this host. It is built from E1.7's *Environment
// aggregate (its already-parsed Document/Sidecar/Tree, never a re-read of
// the filesystem) and is the ONE input both review.go's renderer and its
// fingerprint hash from — so nothing can be fingerprinted that was never
// shown, and nothing shown was never fingerprinted (AC-66's "every shown
// summary fact is fingerprinted" pairing with bom_test.go's reverse check).
//
// Mirrors workflow/pack's hostBoM/ComputeHostBoM (trust.go) in shape and
// intent — same "everything you are consenting to, fingerprinted as
// structured JSON, never a delimiter-joined string" discipline — but this
// is environments' OWN type, not a shared one: an environment's host-exec
// surface (native secrets/registries/MCP/ports/kits plus the pix.toml
// sidecar's host.mcp/host.services/inference) has no overlap with a pack
// manifest's integrations/proxies/bins/services.
package env

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"pix/host/envinfo"
	"pix/host/hosttrust"
)

// WorkspaceMount is one additional, non-kit writable workspace this
// environment would mount if launched: docs/design/environments.md §5.1
// restriction 4 already refuses a root resolving INSIDE one of these
// (RefuseContainment, resolve.go); this is that SAME caller-supplied list,
// reused here as the "N new mount" review facet. It is supplied by the
// caller — this package derives no workspace list of its own, exactly as
// resolve.go's RefuseContainment doc comment already establishes: "that
// composition belongs to whichever later unit builds the effective
// declaration (E1.8's bill of materials, E2.1's renderer)".
type WorkspaceMount struct {
	Path     string
	ReadOnly bool
}

// HostCommand is one local-command MCP server: docs/design/environments.md
// §5.1's `mcp.servers[].command` — sbx spawns Argv[0] with Argv[1:] on this
// host. Name is the server's own stable identity (tree.go's
// `mcp.servers[<name>]`).
type HostCommand struct {
	Name string
	Argv []string
}

// HostServiceItem is one pix.toml `[[host.services]]` entry: a non-MCP
// process the host must run alongside the sandbox. SHA is the resolved
// executable's content hash (hosttrust.HashFile through the SAME
// ResolveLocalCommand a bare-or-relative-or-absolute command reference
// resolves through elsewhere in this package), empty when the command
// could not be resolved to a concrete local path at all (a `pix
// doctor`-shaped gap, not a review-time refusal — see ResolveLocalCommand's
// own doc comment).
type HostServiceItem struct {
	Name    string
	Command string
	Args    []string
	Port    int64
	Probe   string
	SHA     string
}

// CredentialTarget is one credential handed to one destination: a secret's
// authored reference (an `op://...` string — never a resolved value) or
// command bound to the domain(s) `bindings.<service>.apiKey.domains`
// injects it into, a registry's own reference bound to its own host, or an
// `[host.mcp.<name>].env_keys` entry naming a credential a local-command
// MCP server receives. Source is always something SAFE to display — an
// authored reference string or an environment-variable NAME — never a
// resolved secret value.
type CredentialTarget struct {
	Source      string
	Destination string
}

// SecretFact is the complete fingerprinted (not necessarily displayed)
// record of one `secrets.<name>` entry: whether it has a ref, the ref text
// itself (safe — an authored `op://...` locator, never a value), whether it
// has a command, and the command argv (host execution — restriction 2).
type SecretFact struct {
	Name       string
	Ref        string
	HasCommand bool
	Command    []string
}

// RegistryFact is the complete fingerprinted record of one
// `registries.<host>` entry, including NoVerify — restriction 3:
// "`noVerify` is fingerprinted and visible in review; it never silently
// weakens a proof."
type RegistryFact struct {
	Host       string
	Ref        string
	HasCommand bool
	Command    []string
	NoVerify   bool
}

// BindingFact is one `bindings.<service>.apiKey.domains` record.
type BindingFact struct {
	Service string
	Domains []string
}

// MCPServerFact is the complete fingerprinted record of one
// `mcp.servers[]` entry: identity (Name), transport (URL or Command), and
// the ORDERED argv (order is semantic — restriction from the unit scope:
// "ordered args").
type MCPServerFact struct {
	Name    string
	URL     string
	Command string
	Args    []string
}

// PortFact is one `ports[]` entry.
type PortFact struct {
	Sandbox int
	Host    int
}

// KitFact is one `kits[]` entry: its authored form, its resolved local
// path or remote reference, and — for a LOCAL kit only — a content hash
// (restriction 5: "Local and Git kit sources... must be pinned or
// content-fingerprinted before host review succeeds"). A remote kit
// reference has no content to hash here; its pin is the reference string
// itself, already carried by Raw/Resolved.
type KitFact struct {
	Raw      string
	Resolved string
	Local    bool
	SHA      string
}

// HostMCPFact is one pix.toml `[host.mcp.<name>]` sidecar annotation: env
// var NAMES a local-command MCP server receives (never values) and its
// health-probe argv (host execution — `pix doctor` runs it).
type HostMCPFact struct {
	Name      string
	EnvKeys   []string
	ProbeArgs []string
}

// InferenceFact is one pix.toml `[inference.backends.<name>]` entry: the
// custom endpoint, auth mode, and credential wiring a launched Pi session
// would use.
type InferenceFact struct {
	Name     string
	Driver   string
	Protocol string
	BaseURL  string
	Auth     string
	KeyEnv   string
}

// BillOfMaterials is the environment's complete host-exec/security-relevant
// surface: every field here is BOTH a fingerprint input (Fingerprint) and,
// where docs/design/environments.md §5.8/AC-66 names it a review line item,
// a rendered summary/verbose fact (review.go). Nothing is fingerprinted
// that renderBill/renderVerboseDetails cannot reach, and nothing rendered
// is left out of Fingerprint — bom_test.go proves both directions.
type BillOfMaterials struct {
	HostCommands      []HostCommand
	HostServices      []HostServiceItem
	CredentialTargets []CredentialTarget
	Mounts            []WorkspaceMount
	Secrets           []SecretFact
	Registries        []RegistryFact
	Bindings          []BindingFact
	MCPServers        []MCPServerFact
	Ports             []PortFact
	Kits              []KitFact
	HostMCP           []HostMCPFact
	Inference         []InferenceFact
	Interpolations    []envinfo.Interpolation
}

// Tier1 reports whether this environment executes anything on the host or
// hands out a credential at all — the review gate (Review, review.go) never
// prompts, and writes no acceptance, unless this is true. A workspace
// mount, an interpolation reference, or a kit alone never raises the tier:
// none of the three is itself host EXECUTION or a credential handoff (the
// same reasoning workflow/pack's hostBoM.Tier1 already applies to egress
// and credential-NAME solicitation alone).
func (b BillOfMaterials) Tier1() bool {
	return len(b.HostCommands) > 0 || len(b.HostServices) > 0 || len(b.CredentialTargets) > 0 || len(b.NoVerifyRegistries()) > 0
}

// NoVerifyRegistries returns the subset of Registries with NoVerify set —
// restriction 3's own review line item, computed on demand rather than
// stored twice.
func (b BillOfMaterials) NoVerifyRegistries() []RegistryFact {
	var out []RegistryFact
	for _, r := range b.Registries {
		if r.NoVerify {
			out = append(out, r)
		}
	}
	return out
}

// ComputeBoM derives env's complete BillOfMaterials. It is a pure function
// of env's already-parsed Document/Sidecar/Tree plus mounts (the caller-
// supplied additional-workspace list, WorkspaceMount) and lookPath (the
// SAME exec.LookPath production seam Load's own symlink checks use, a nil
// value defaulting to the real one) — no other filesystem or process state.
// It returns an error only when a LOCAL kit or resolved host-service
// executable cannot be content-hashed (a symlink introduced since Load ran,
// a permissions error, or the path disappearing): an unfingerprintable
// surface is refused, never silently fingerprinted as absent.
func ComputeBoM(env *Environment, mounts []WorkspaceMount, lookPath func(string) (string, error)) (BillOfMaterials, error) {
	var b BillOfMaterials
	b.Mounts = append([]WorkspaceMount(nil), mounts...)
	sort.Slice(b.Mounts, func(i, j int) bool { return b.Mounts[i].Path < b.Mounts[j].Path })

	if env.Document != nil {
		if err := computeSecretsAndCredentials(env.Document, &b); err != nil {
			return BillOfMaterials{}, err
		}
		computeRegistries(env.Document, &b)
		computeMCP(env.Document, &b)
		computePorts(env.Document, &b)
		if err := computeKits(env.Document, &b); err != nil {
			return BillOfMaterials{}, err
		}
	}
	if env.Sidecar != nil {
		if err := computeHostServices(env.Root, env.Sidecar, &b, lookPath); err != nil {
			return BillOfMaterials{}, err
		}
		computeHostMCP(env.Sidecar, &b)
		computeInference(env.Sidecar, &b)
	}
	if env.Tree != nil {
		b.Interpolations = append([]envinfo.Interpolation(nil), env.Tree.Interpolations...)
		sort.Slice(b.Interpolations, func(i, j int) bool {
			if b.Interpolations[i].KeyPath != b.Interpolations[j].KeyPath {
				return b.Interpolations[i].KeyPath < b.Interpolations[j].KeyPath
			}
			return b.Interpolations[i].Var < b.Interpolations[j].Var
		})
	}
	return b, nil
}

func computeSecretsAndCredentials(doc *envinfo.Document, b *BillOfMaterials) error {
	for _, name := range sortedKeys(doc.Secrets) {
		s := doc.Secrets[name]
		b.Secrets = append(b.Secrets, SecretFact{
			Name: name, Ref: s.Ref, HasCommand: len(s.Command) > 0,
			Command: append([]string(nil), s.Command...),
		})
		if s.Ref == "" && len(s.Command) == 0 {
			continue
		}
		source := s.Ref
		if source == "" {
			source = "command: " + strings.Join(s.Command, " ")
		}
		if bind, ok := doc.Bindings[name]; ok {
			domains := append([]string(nil), bind.APIKey.Domains...)
			sort.Strings(domains)
			for _, d := range domains {
				b.CredentialTargets = append(b.CredentialTargets, CredentialTarget{Source: source, Destination: d})
			}
		}
	}
	for _, svc := range sortedKeys(doc.Bindings) {
		domains := append([]string(nil), doc.Bindings[svc].APIKey.Domains...)
		sort.Strings(domains)
		b.Bindings = append(b.Bindings, BindingFact{Service: svc, Domains: domains})
	}
	return nil
}

func computeRegistries(doc *envinfo.Document, b *BillOfMaterials) {
	for _, host := range sortedKeys(doc.Registries) {
		r := doc.Registries[host]
		b.Registries = append(b.Registries, RegistryFact{
			Host: host, Ref: r.Ref, HasCommand: len(r.Command) > 0,
			Command: append([]string(nil), r.Command...), NoVerify: r.NoVerify,
		})
		if r.Ref == "" && len(r.Command) == 0 {
			continue
		}
		source := r.Ref
		if source == "" {
			source = "command: " + strings.Join(r.Command, " ")
		}
		b.CredentialTargets = append(b.CredentialTargets, CredentialTarget{Source: source, Destination: host})
	}
}

func computeMCP(doc *envinfo.Document, b *BillOfMaterials) {
	for _, srv := range doc.MCP.Servers {
		b.MCPServers = append(b.MCPServers, MCPServerFact{
			Name: srv.Name, URL: srv.URL, Command: srv.Command, Args: append([]string(nil), srv.Args...),
		})
		if srv.Command != "" {
			b.HostCommands = append(b.HostCommands, HostCommand{
				Name: srv.Name, Argv: append([]string{srv.Command}, srv.Args...),
			})
		}
	}
}

func computePorts(doc *envinfo.Document, b *BillOfMaterials) {
	for _, p := range doc.Ports {
		b.Ports = append(b.Ports, PortFact{Sandbox: p.Sandbox, Host: p.Host})
	}
}

func computeKits(doc *envinfo.Document, b *BillOfMaterials) error {
	for _, k := range doc.Kits {
		fact := KitFact{Raw: k.Raw, Resolved: k.Resolved, Local: k.Local}
		if k.Local {
			sha, err := hashPath(k.Resolved)
			if err != nil {
				return fmt.Errorf("kit %q: %w (cannot fingerprint the host-exec surface; fail closed)", k.Raw, err)
			}
			fact.SHA = sha
		}
		b.Kits = append(b.Kits, fact)
	}
	return nil
}

func computeHostServices(root string, s *envinfo.Sidecar, b *BillOfMaterials, lookPath func(string) (string, error)) error {
	for _, svc := range s.Host.Services {
		item := HostServiceItem{Name: svc.Name, Command: svc.Command, Args: append([]string(nil), svc.Args...), Port: svc.Port, Probe: svc.Probe}
		if resolved, ok := ResolveLocalCommand(root, svc.Command, lookPath); ok {
			sha, err := hashPath(resolved)
			if err != nil {
				return fmt.Errorf("host service %q: %w (cannot fingerprint the host-exec surface; fail closed)", svc.Name, err)
			}
			item.SHA = sha
		}
		b.HostServices = append(b.HostServices, item)
	}
	return nil
}

func computeHostMCP(s *envinfo.Sidecar, b *BillOfMaterials) {
	for _, name := range sortedHostMCPKeys(s.Host.MCP) {
		e := s.Host.MCP[name]
		envKeys := append([]string(nil), e.EnvKeys...)
		sort.Strings(envKeys)
		b.HostMCP = append(b.HostMCP, HostMCPFact{Name: name, EnvKeys: envKeys, ProbeArgs: append([]string(nil), e.ProbeArgs...)})
		for _, key := range envKeys {
			b.CredentialTargets = append(b.CredentialTargets, CredentialTarget{Source: key, Destination: name + " (host)"})
		}
	}
}

func computeInference(s *envinfo.Sidecar, b *BillOfMaterials) {
	for _, name := range sortedInferenceKeys(s.Inference.Backends) {
		be := s.Inference.Backends[name]
		b.Inference = append(b.Inference, InferenceFact{
			Name: name, Driver: be.Driver, Protocol: be.Protocol, BaseURL: be.BaseURL, Auth: be.Auth, KeyEnv: be.KeyEnv,
		})
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedHostMCPKeys(m map[string]envinfo.HostMCPEntry) []string { return sortedKeys(m) }

func sortedInferenceKeys(m map[string]envinfo.InferenceBackend) []string { return sortedKeys(m) }

// hashPath content-hashes a single path: a regular file directly
// (hosttrust.HashFile, symlink-refused), or every regular file under a
// directory (hashDir). Either way an unhashable surface — a symlink
// anywhere in scope, a permissions error, a path that has disappeared — is
// an ERROR, never silently skipped: hosttrust.HashFile's own doc comment
// applies here too: "it can be neither accepted nor installed, so a caller
// fails closed rather than fingerprinting a partial surface."
func hashPath(path string) (string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%s: is a symlink; refusing", path)
	}
	if fi.IsDir() {
		return hashDir(path)
	}
	return hosttrust.HashFile(path, path)
}

// hashDir hashes every regular file under dir, keyed by its dir-relative
// path, in sorted order — a pure function of file paths and contents, so a
// reproducible bit-identical directory always hashes identically regardless
// of directory-entry iteration order. Mirrors packinfo's dirHasSymlink
// blanket-rejection reasoning: filepath.WalkDir does not descend into a
// symlinked DIRECTORY, so the walk itself surfaces that entry (as a
// symlink), which this function refuses outright rather than silently
// walking past it into whatever it points at.
func hashDir(dir string) (string, error) {
	var files []string
	walkErr := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s: symlink inside kit content; refusing", p)
		}
		if d.IsDir() {
			return nil
		}
		files = append(files, p)
		return nil
	})
	if walkErr != nil {
		return "", walkErr
	}
	sort.Strings(files)
	h := sha256.New()
	for _, f := range files {
		rel, err := filepath.Rel(dir, f)
		if err != nil {
			return "", err
		}
		data, err := os.ReadFile(f)
		if err != nil {
			return "", err
		}
		h.Write([]byte(rel))
		h.Write([]byte{0})
		h.Write(data)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fpDoc is the canonical, versioned JSON shape Fingerprint hashes. Field
// names, order, and omitempty are load-bearing exactly as workflow/pack's
// analogous fpDoc documents: changing one re-gates every already-accepted
// environment. V bumps whenever this shape changes.
type fpDoc struct {
	V                 int                     `json:"v"`
	HostCommands      []HostCommand           `json:"host_commands,omitempty"`
	HostServices      []HostServiceItem       `json:"host_services,omitempty"`
	CredentialTargets []CredentialTarget      `json:"credential_targets,omitempty"`
	Mounts            []WorkspaceMount        `json:"mounts,omitempty"`
	Secrets           []SecretFact            `json:"secrets,omitempty"`
	Registries        []RegistryFact          `json:"registries,omitempty"`
	Bindings          []BindingFact           `json:"bindings,omitempty"`
	MCPServers        []MCPServerFact         `json:"mcp_servers,omitempty"`
	Ports             []PortFact              `json:"ports,omitempty"`
	Kits              []KitFact               `json:"kits,omitempty"`
	HostMCP           []HostMCPFact           `json:"host_mcp,omitempty"`
	Inference         []InferenceFact         `json:"inference,omitempty"`
	Interpolations    []envinfo.Interpolation `json:"interpolations,omitempty"`
}

// Fingerprint hashes b's ENTIRE surface — every field this file's doc
// comment promises is both fingerprinted and (where §5.8 names it)
// rendered. Every list is sorted canonically before hashing so a pure
// authoring reorder never re-gates; HostCommands/HostServices/Ports/etc.
// are already produced in a stable (sorted-key or identity) order by
// ComputeBoM, so this function only imposes the ordering ComputeBoM does
// not already guarantee (none, currently — kept explicit rather than
// assumed, the same discipline pack's sortedByKey applies at the
// fingerprint boundary rather than trusting an upstream producer's order).
func Fingerprint(b BillOfMaterials) (string, error) {
	doc := fpDoc{
		V:                 1,
		HostCommands:      sortedByField(b.HostCommands, func(v HostCommand) string { return v.Name }),
		HostServices:      sortedByField(b.HostServices, func(v HostServiceItem) string { return v.Name }),
		CredentialTargets: sortedByField(b.CredentialTargets, func(v CredentialTarget) string { return v.Source + "\x00" + v.Destination }),
		Mounts:            sortedByField(b.Mounts, func(v WorkspaceMount) string { return v.Path }),
		Secrets:           sortedByField(b.Secrets, func(v SecretFact) string { return v.Name }),
		Registries:        sortedByField(b.Registries, func(v RegistryFact) string { return v.Host }),
		Bindings:          sortedByField(b.Bindings, func(v BindingFact) string { return v.Service }),
		MCPServers:        sortedByField(b.MCPServers, func(v MCPServerFact) string { return v.Name }),
		Ports:             sortedByField(b.Ports, func(v PortFact) string { return fmt.Sprintf("%d", v.Sandbox) }),
		Kits:              append([]KitFact(nil), b.Kits...),
		HostMCP:           sortedByField(b.HostMCP, func(v HostMCPFact) string { return v.Name }),
		Inference:         sortedByField(b.Inference, func(v InferenceFact) string { return v.Name }),
		Interpolations:    append([]envinfo.Interpolation(nil), b.Interpolations...),
	}
	canonical, err := hosttrust.Canonicalize(doc)
	if err != nil {
		return "", fmt.Errorf("encoding environment host-exec surface: %v", err)
	}
	return hosttrust.Fingerprint(canonical)
}

// sortedByField returns a sorted COPY of in — the canonical ordering every
// fingerprint section needs, so a pure authoring reorder never re-gates.
func sortedByField[T any](in []T, key func(T) string) []T {
	out := append([]T(nil), in...)
	sort.Slice(out, func(i, j int) bool { return key(out[i]) < key(out[j]) })
	return out
}
