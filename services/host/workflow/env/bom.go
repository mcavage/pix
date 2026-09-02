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
	"pix/host/envsetup"
	"pix/host/hosttrust"
)

// WorkspaceMount is one additional, non-kit writable workspace this
// environment would mount if launched: docs/design/environments.md §5.1
// restriction 4 already refuses a root resolving INSIDE one of these
// (RefuseContainment, resolve.go). It is supplied by the caller — this
// package derives no workspace list of its own, exactly as resolve.go's
// RefuseContainment doc comment already establishes: "that composition
// belongs to whichever later unit builds the effective declaration (E1.8's
// bill of materials, E2.1's renderer)". A mount expands what the host makes
// available to the sandbox, so — unlike an interpolation reference or a
// kit — it is itself Tier1 (docs/design/environments.md §9.1: the trust
// fingerprint covers whatever "expand[s] mounted host access"); see
// EffectiveMounts below for the typed collection this package actually
// accepts.
type WorkspaceMount struct {
	Path     string
	ReadOnly bool
}

// EffectiveMounts is the ONE typed effective workspace set that flows
// end-to-end through Load, Review and ComputeShow (workflow/env's own
// composition, load.go/review.go), and the reviewed set of additional
// workspace mounts ComputeBoM fingerprints/renders here. It is a DISTINCT
// named type, not a bare []WorkspaceMount or []string, precisely so
// Load's, Review's and ComputeBoM's signatures cannot be satisfied by an
// unrelated slice a caller happened to have lying around (an ad hoc CLI
// flag, a workspace list built for some other purpose): a caller must
// consciously construct an EffectiveMounts value from whatever launch-time
// composition actually decided the effective mount set is, exactly the same
// discipline this package already holds every other host-exec fact to.
// Before this type existed as Load's own parameter, Load and Review took a
// SECOND, independent `workspaces []string` alongside it — two unrelated
// lists a caller was free to let diverge, so a mount that should have
// refused containment could reach the reviewed bill having never been
// checked (E1.9's BLOCK finding). There is now exactly one parameter of
// this type on each of Load/Review, so no caller can pass workspaces
// separately from effective mounts at all.
//
// A caller still supplies the RUNTIME mounts (a launch's MountDirs: skill
// trees, a pack's contributed directories, `--dev`'s repo skills); this
// package derives none of those, since they depend on the calling
// process's cwd and flags. What it DOES derive, now that envinfo models
// the native `workspace:`/`additionalWorkspaces:` keys, is the set the
// DOCUMENT declares (load.go's AuthoredMounts / AuthoredAdditionalMounts):
// those are not a caller's assertion, they are a parsed fact, and a
// reviewer must see them. EffectiveMounts stays a required, typed
// parameter on Load/Review — not an optional bare string list — so a
// launch composition still cannot smuggle a writable runtime mount past
// review without constructing a genuine value of this type.
type EffectiveMounts []WorkspaceMount

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

// SetupHookFact is one pix.toml `[[setup]]` entry, resolved to the exact
// executable `pix setup --env NAME` would run on this host — the v2
// replacement for a pack's install/auth hook, fingerprinted the way a pack
// manifest's bin/service entries were: identity, the RESOLVED executable's
// content hash, both argv lists in order, its kind, and whether a failure
// is fatal.
//
// It is a type ALIAS for envsetup.Hook, not a copy: the value a human
// accepts in the trust bill is the identical value the runner executes, so
// no conversion can let the reviewed argv and the executed argv drift
// apart. envsetup (L2) owns the type and the executable proof;
// this workflow owns the fingerprint and the render.
type SetupHookFact = envsetup.Hook

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
	// CredentialService/Header/Format are the sbx-session injection wiring
	// (envinfo.InferenceBackend). They name a host credential service and
	// the request shape its token is spent in, so they are host-affecting
	// review facts in exactly the way base_url and key_env already are:
	// fingerprinted here, rendered by renderBill, re-gated on edit.
	CredentialService string
	CredentialHeader  string
	CredentialFormat  string
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
	SetupHooks        []SetupHookFact
	CredentialTargets []CredentialTarget
	EffectiveMounts   EffectiveMounts
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

// Tier1 reports whether this environment executes anything on the host,
// hands out a credential, or expands what the host mounts into the sandbox
// — the review gate (Review, review.go) never prompts, and writes no
// acceptance, unless this is true. An interpolation reference or a kit
// ALONE never raises the tier: neither is itself host EXECUTION, a
// credential handoff, or a mount expansion (the same reasoning
// workflow/pack's hostBoM.Tier1 already applies to egress and
// credential-NAME solicitation alone). A new EffectiveMounts entry DOES
// raise it: docs/design/environments.md §9.1 names "expand mounted host
// access" as one of the four things the trust fingerprint exists to gate,
// alongside host execution, credential disclosure, and model-traffic
// routing.
func (b BillOfMaterials) Tier1() bool {
	return len(b.HostCommands) > 0 || len(b.HostServices) > 0 || len(b.SetupHooks) > 0 || len(b.CredentialTargets) > 0 || len(b.NoVerifyRegistries()) > 0 || len(b.EffectiveMounts) > 0
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
// of env's already-parsed Document/Sidecar/Tree plus effective (the
// caller-supplied, consciously-constructed EffectiveMounts — never a bare
// []WorkspaceMount some unrelated caller happened to build) and lookPath
// (the SAME exec.LookPath production seam Load's own symlink checks use, a
// nil value defaulting to the real one) — no other filesystem or process
// state. It returns an error only when a LOCAL kit or resolved host-service
// executable cannot be content-hashed (a symlink introduced since Load ran,
// a permissions error, or the path disappearing): an unfingerprintable
// surface is refused, never silently fingerprinted as absent.
func ComputeBoM(env *Environment, effective EffectiveMounts, lookPath func(string) (string, error)) (BillOfMaterials, error) {
	var b BillOfMaterials
	// The bill's mounts are the caller's effective set UNIONED with the
	// additional workspaces the document itself declares. Authored entries
	// go first so that a path declared twice keeps the AUTHORED readOnly
	// bit rather than a runtime duplicate's — the same precedence
	// envinfo's renderer applies when it composes the same two sources into
	// `additionalWorkspaces:`, so what a reviewer consents to here is what
	// gets rendered there.
	var mounts EffectiveMounts
	if env != nil {
		mounts = append(mounts, AuthoredAdditionalMounts(env.Document)...)
	}
	mounts = append(mounts, effective...)
	seenMount := map[string]bool{}
	for _, m := range mounts {
		if seenMount[m.Path] {
			continue
		}
		seenMount[m.Path] = true
		b.EffectiveMounts = append(b.EffectiveMounts, m)
	}
	sort.Slice(b.EffectiveMounts, func(i, j int) bool { return b.EffectiveMounts[i].Path < b.EffectiveMounts[j].Path })

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
		if err := computeSetupHooks(env.Root, env.Sidecar, &b); err != nil {
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

// unboundCredentialDestination is the Destination a secret's command/ref
// renders with when it exists but nothing in `bindings.<name>` names a
// domain for it (restriction 3: "still render its command and a
// credential/source fact... without inventing a domain") — a literal,
// unambiguous placeholder, never a fabricated or guessed hostname.
const unboundCredentialDestination = "(unbound)"

func computeSecretsAndCredentials(doc *envinfo.Document, b *BillOfMaterials) error {
	for _, name := range sortedKeys(doc.Secrets) {
		s := doc.Secrets[name]
		b.Secrets = append(b.Secrets, SecretFact{
			Name: name, Ref: s.Ref, HasCommand: len(s.Command) > 0,
			Command: append([]string(nil), s.Command...),
		})
		// Restriction 3: a secret `command` is host execution whether or not
		// it is ever bound to a domain — an unbound secret's command still
		// runs on this host every time the secret is resolved.
		if len(s.Command) > 0 {
			b.HostCommands = append(b.HostCommands, HostCommand{
				Name: "secret:" + name, Argv: append([]string(nil), s.Command...),
			})
		}
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
		} else {
			// No binding names a destination domain — the secret's ref/command
			// is still a credential-bearing source and must still render, but
			// with an explicit unbound marker rather than an invented domain.
			b.CredentialTargets = append(b.CredentialTargets, CredentialTarget{Source: source, Destination: unboundCredentialDestination})
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
		// Restriction 4: a registry `command` is host execution exactly like a
		// secret's — named by the registry's OWN stable identity (its host),
		// so two registries that happen to author the identical argv are
		// still two distinct HostCommand entries, never collapsed into one.
		if len(r.Command) > 0 {
			b.HostCommands = append(b.HostCommands, HostCommand{
				Name: "registry:" + host, Argv: append([]string(nil), r.Command...),
			})
		}
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

// computeSetupHooks resolves and content-hashes every `[[setup]]` entry,
// including its declared `inputs`. The command is resolved WITHOUT a PATH
// lookup on purpose — envinfo's parse already refused a bare name, so the
// only two shapes here are a relative path against the environment root
// and an absolute path, and neither depends on the calling process's cwd
// or environment. A RELATIVE command additionally has its resolved parent
// directory's symlink chain proven contained inside the environment root
// (envsetup.ProveContained): a lexically-inside-root path can still be a
// symlink alias of somewhere else if an ancestor directory is itself a
// symlink, exactly the containment gap workflow/env's own
// RefuseContainment already closes for a registered environment root. An
// ABSOLUTE command has no such containment to prove — it is allowed only
// because it is fingerprinted the same way a relative one is — but it is
// still snapshotted byte-for-byte at execution time exactly like a
// relative one.
//
// Every declared input resolves and is proven contained the identical way
// a relative command is, REGARDLESS of whether the command itself is
// relative or absolute: an input is always a companion living inside this
// environment, never something reached relative to wherever an absolute
// command happens to live outside it. A hook (or an input) that cannot be
// proven to be a regular, non-symlink file inside the environment root with
// a readable content hash fails the whole bill closed: an unfingerprintable
// host-exec surface is never silently reviewed as absent.
func computeSetupHooks(root string, s *envinfo.Sidecar, b *BillOfMaterials) error {
	for _, h := range s.Setup {
		var resolved string
		if filepath.IsAbs(h.Command) {
			resolved = envsetup.Resolve(root, h.Command)
		} else {
			var err error
			resolved, err = envsetup.ProveContained(root, h.Command)
			if err != nil {
				return fmt.Errorf("setup hook %q: %w (cannot fingerprint the host-exec surface; fail closed)", h.ID, err)
			}
		}
		sha, err := envsetup.HashSetupExecutable(resolved)
		if err != nil {
			return fmt.Errorf("setup hook %q: %w (cannot fingerprint the host-exec surface; fail closed)", h.ID, err)
		}
		var inputs []envsetup.HookInput
		for _, in := range h.Inputs {
			resolvedIn, err := envsetup.ProveContained(root, in)
			if err != nil {
				return fmt.Errorf("setup hook %q: input %q: %w (cannot fingerprint the host-exec surface; fail closed)", h.ID, in, err)
			}
			// Setup snapshots copy inputs as files. Refuse a directory here,
			// before trust review, rather than rendering a directory hash the
			// executor can never materialize as one declared input.
			inSHA, err := hosttrust.HashFile(resolvedIn, resolvedIn)
			if err != nil {
				return fmt.Errorf("setup hook %q: input %q: %w (cannot fingerprint the host-exec surface; fail closed)", h.ID, in, err)
			}
			inputs = append(inputs, envsetup.HookInput{Path: in, SHA: inSHA})
		}
		b.SetupHooks = append(b.SetupHooks, SetupHookFact{
			ID:        h.ID,
			Command:   h.Command,
			Resolved:  resolved,
			SHA:       sha,
			CheckArgs: append([]string(nil), h.CheckArgs...),
			ApplyArgs: append([]string(nil), h.ApplyArgs...),
			Kind:      h.EffectiveKind(),
			Required:  h.Required,
			Inputs:    inputs,
		})
	}
	sort.Slice(b.SetupHooks, func(i, j int) bool { return b.SetupHooks[i].ID < b.SetupHooks[j].ID })
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
		// Restriction 2: `pix doctor` runs probe_args on this host — that is
		// host execution regardless of whether this entry names any
		// env_keys at all, so it gets its own named HostCommand fact rather
		// than only ever being described alongside a credential grant.
		if len(e.ProbeArgs) > 0 {
			b.HostCommands = append(b.HostCommands, HostCommand{
				Name: name + " (probe)", Argv: append([]string(nil), e.ProbeArgs...),
			})
		}
	}
}

func computeInference(s *envinfo.Sidecar, b *BillOfMaterials) {
	for _, name := range sortedInferenceKeys(s.Inference.Backends) {
		be := s.Inference.Backends[name]
		b.Inference = append(b.Inference, InferenceFact{
			Name: name, Driver: be.Driver, Protocol: be.Protocol, BaseURL: be.BaseURL, Auth: be.Auth, KeyEnv: be.KeyEnv,
			CredentialService: be.CredentialService,
			CredentialHeader:  be.CredentialHeader,
			CredentialFormat:  be.CredentialFormat,
		})
		// An sbx-session backend spends the named host credential SERVICE's
		// token against the declared endpoint. That is a credential
		// destination in the same sense key_env's is, and a reviewer who is
		// not shown it cannot tell which host session an environment is
		// about to spend.
		if be.CredentialService != "" {
			dest := be.BaseURL
			if dest == "" {
				dest = name + " (inference)"
			}
			b.CredentialTargets = append(b.CredentialTargets, CredentialTarget{Source: be.CredentialService + " (sbx credential service)", Destination: dest})
		}
		// Restriction 1: `key_env` names a host environment variable a
		// launched Pi session hands this backend as a credential — a
		// credential target exactly like a bound secret's, source is the
		// env var NAME (never a value), destination is the backend's own
		// endpoint (falling back to its name when it declares no base_url).
		if be.KeyEnv != "" {
			dest := be.BaseURL
			if dest == "" {
				dest = name + " (inference)"
			}
			b.CredentialTargets = append(b.CredentialTargets, CredentialTarget{Source: be.KeyEnv, Destination: dest})
		}
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
// environment. V bumps whenever this shape changes — v2 renames the
// `mounts` key to `effective_mounts` (BillOfMaterials.EffectiveMounts).
type fpDoc struct {
	V                 int                     `json:"v"`
	HostCommands      []HostCommand           `json:"host_commands,omitempty"`
	HostServices      []HostServiceItem       `json:"host_services,omitempty"`
	SetupHooks        []SetupHookFact         `json:"setup_hooks,omitempty"`
	CredentialTargets []CredentialTarget      `json:"credential_targets,omitempty"`
	EffectiveMounts   EffectiveMounts         `json:"effective_mounts,omitempty"`
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
// V bumped 2 -> 3 when SetupHooks joined the surface: an already-accepted
// environment is re-gated once, which is the correct outcome for a schema
// that can now carry host execution it could not express before.
// HostCommands/HostServices/Ports/etc. are already produced in a stable
// (sorted-key or identity) order by
// ComputeBoM, so this function only imposes the ordering ComputeBoM does
// not already guarantee (none, currently — kept explicit rather than
// assumed, the same discipline pack's sortedByKey applies at the
// fingerprint boundary rather than trusting an upstream producer's order).
func Fingerprint(b BillOfMaterials) (string, error) {
	doc := fpDoc{
		V:                 3,
		HostCommands:      sortedByField(b.HostCommands, func(v HostCommand) string { return v.Name }),
		HostServices:      sortedByField(b.HostServices, func(v HostServiceItem) string { return v.Name }),
		SetupHooks:        sortedByField(b.SetupHooks, func(v SetupHookFact) string { return v.ID }),
		CredentialTargets: sortedByField(b.CredentialTargets, func(v CredentialTarget) string { return v.Source + "\x00" + v.Destination }),
		EffectiveMounts:   EffectiveMounts(sortedByField([]WorkspaceMount(b.EffectiveMounts), func(v WorkspaceMount) string { return v.Path })),
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
