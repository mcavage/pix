// render.go — E2.1's ONE stable effective `.sbxenv.yaml` producer (AC-54,
// docs/design/environments.md §6.2). `pix env show --effective` and every
// future launch path (E2.5) must produce the byte-identical document by
// calling RenderEffective, never a second, independently-shaped renderer —
// see services/host/arch_effective_test.go's
// TestArchitecture_ExactlyOneEffectiveDocumentProducer (F17).
//
// RenderEffective is a PURE function of RuntimeFacts: no filesystem read,
// no process exec, and — per this package's own sibling-isolation rule
// (doc.go, "It imports no sibling capability") — no import of
// pix/host/mcp, pix/host/sandbox, or any pix/host/workflow/* package.
// Every fact it renders is caller-supplied and already resolved; this file
// only serializes. That keeps envinfo an L1 leaf exactly as parse.go/
// merge.go/tree.go already are, and it is what makes RenderEffective
// reusable, unchanged, from both `env show` (a caller-composed PREVIEW,
// workflow/env's ComputeEffective) and a future real launch (a caller-
// composed set of facts a live `sbx` run actually used) without either
// caller depending on the other's shape.
//
// # What the effective document may contain
//
// The rendered document is UPSTREAM's schema plus nothing: sbx's loader
// "rejects unknown fields and unsupported schema versions" (§4, confirmed
// by Story 0), so an invented top-level key is not a cosmetic liberty, it
// is a create that fails. Two consequences are load-bearing here and are
// pinned by testdata/render/*.yaml:
//
//   - The pinned template and pull policy render as `sandboxOptions.
//     template` / `sandboxOptions.pullPolicy`, the exact placement Story 0
//     created a real sandbox with (docs/upstream/sbx-0.39-environments.md
//     §7; uatenvmatrix/check_local_image.go's live fixture) — never as
//     top-level `template:`/`pullPolicy:` keys, which upstream never
//     accepted.
//   - Sidecar (pix.toml) facts never appear in this document at all. §6.2
//     enumerates the Pix-owned runtime facts the effective file adds
//     (pinned template/pullPolicy, workspaces in object form, the
//     unconditional personal-context workspace, the generated Pi mixin
//     kit, a `--dev` checkout kit, Pix-required env vars, reviewed
//     local-MCP credential wrappers) and inference/roster is not among
//     them: §7 puts the model roster and every custom backend in ONE
//     generated artifact, the mixin kit's `~/.pi/agent/inference.json`
//     ("There is no second generated routing artifact that can disagree
//     with provider registration"). Rendering an `inference:` block into
//     the native document would be both that forbidden second artifact and
//     an unknown field to sbx's loader.
//
// # Renderer/materializer split
//
// RenderEffective renders exactly the REFERENCE to a generated kit
// (MixinKit / DevKit.Kit — a directory path or a kit URL string) that
// RuntimeFacts already carries; it never creates, materializes, or writes
// that directory itself, and it performs no cleanup of one either.
// Materializing a kit directory (and reusing launch's EXISTING cleanup for
// it — this unit does not invent a second one) stays whichever caller
// actually launches something; `env show --effective`'s own caller
// (workflow/env's ComputeEffective) composes a deterministic PREVIEW
// precisely so it never has to materialize or clean up anything at all.
//
// # Determinism
//
// The same RuntimeFacts value always produces the same bytes: every
// map-valued section (env, sandboxOptions, secrets, registries, bindings)
// is serialized through gopkg.in/yaml.v3, which emits map keys in sorted
// order, and every list keeps its caller-supplied order. Nothing here
// reads the clock, the environment, or the filesystem.
package envinfo

import (
	"bytes"
	"errors"

	"gopkg.in/yaml.v3"
)

// WorkspaceFact is one workspace mount in "object form" — docs/design/
// environments.md §6.2's phrase for the effective document's richer
// workspace shape, which also carries the clone choice a native
// `workspace: <path>` string cannot. Path == "" means this fact is unset;
// RenderEffective renders no mount for it then (PersonalContextWorkspace
// is the one field where a caller found no personal context configured at
// all — never a fabricated mount).
type WorkspaceFact struct {
	Path     string
	ReadOnly bool
	// Clone reports whether this workspace is a fresh git clone of Path
	// (true) rather than a direct bind mount of it (false) — the
	// "including clone choice" half of §6.2's workspace object.
	Clone bool
}

// DevKitFact carries `--dev`'s development-checkout kit path plus the
// live skill directories pi is told to load from it (§6.2: "development
// checkout kit and live skill arguments when --dev"; workflow/launch's
// BuildPiInvocation is the existing, non-preview analog this fact
// mirrors). The zero value (Kit == "") means an ordinary, non-dev launch:
// RenderEffective renders no dev kit then.
//
// Kit renders as one more `kits:` entry. LiveSkills deliberately does NOT
// render into this document: a live skill directory is a `pi --skill <dir>`
// ARGUMENT, not an environment declaration — the same MountDirs-is-not-
// LiveSkillDirs split the harness already runs on (AGENTS.md: "pi gets
// --skill <context>/skills, sbx gets <context>"). The caller that composes
// the pi invocation owns those arguments, and the caller that needs a
// skill tree readable inside the sandbox declares it as a workspace fact.
type DevKitFact struct {
	Kit        string
	LiveSkills []string
}

// MCPWrapperFact is one already-composed, already-reviewed MCP server
// entry: docs/design/environments.md §9.2's "reviewed local-MCP
// credential wrappers". Command/Args are the FINAL argv a caller already
// resolved — including whatever credential-wrapper argv package mcp's
// OpRunWrap (the ONE such grammar this module has; see
// arch_effective_test.go's TestArchitecture_NoDuplicateOpRunGrammar)
// already applied. RenderEffective renders each server's fields in
// isolation and never merges one server's Args/EnvKeys into another's
// (render_test.go's TestRenderEffective_MCPServerIsolation): "a server
// declaring A must not observe configured ref B" (§9.2) is a per-server
// invariant this renderer must hold, not merely the wrapper that produced
// Command/Args upstream.
type MCPWrapperFact struct {
	Name    string
	URL     string
	Command string
	Args    []string
}

// RuntimeFacts is the caller-supplied, already-composed set of Pix-owned
// runtime decisions RenderEffective needs to produce one deterministic
// effective `.sbxenv.yaml` (AC-54). Every field is a typed VALUE, never a
// live handle — no *config.Config, no *sandbox.Entry, no mcp.McpRegistrar
// — so a caller (workflow/env's ComputeEffective today; a future launch
// composition) must resolve every fact from its OWN state before calling
// RenderEffective. This package never re-derives, re-probes, or
// re-validates any of them.
type RuntimeFacts struct {
	// Document is the caller's already Parse'd (and, for multiple
	// authored files, already Merge'd — folded back into one *Document by
	// the caller) native `.sbxenv.yaml`. RenderEffective never re-parses,
	// re-resolves a kit path, or re-runs upstream merge semantics on it.
	// A nil Document is a caller bug, not a "no environment" state: D17's
	// `none` state is Pix's own built-in-defaults *Document*, which the
	// caller constructs (workflow/env's ComputeEffective).
	Document *Document

	// Sidecar is the caller's already-parsed optional pix.toml, or nil
	// when the environment has none (envinfo.Sidecar's own "optional"
	// contract, sidecar.go). It is carried on RuntimeFacts because a
	// caller composes ONE fact value for a launch, but NOTHING in it
	// reaches this document: every sidecar concept is either a host fact
	// ([[host.services]], [host.mcp] env_keys — already folded into
	// MCPServers by the caller), a pi-invocation fact ([pi].skills), or a
	// generated-mixin-kit fact ([models], [agents], [inference.*] → §7's
	// single `inference.json`). See this file's "What the effective
	// document may contain"; render_test.go's
	// TestRenderEffective_ExclusiveCustomBackend pins the non-leak.
	Sidecar *Sidecar

	// SandboxName is the caller-computed `pix-*` sandbox name (docs/
	// design/environments.md §6.2: "Sandbox identity is attributed before
	// composition"). This package never derives a name of its own — the
	// same discipline doc.go already states for envinfo as a whole. When
	// it is empty, the authored document's own `name:` (Story 0's
	// fixtures author one; a registered environment omits it) renders
	// unchanged rather than being replaced by a fabricated value.
	SandboxName string

	// Template/PullPolicy are the pinned image reference and pull policy
	// (§6.2: "pinned Pix template and `pullPolicy: missing`"), rendered
	// as `sandboxOptions.template` / `sandboxOptions.pullPolicy` — the
	// placement Story 0 proved against a live `sbx env create`. Either
	// being "" means the caller declared none explicitly:
	// RenderEffective renders no key for it and never fabricates a
	// default value.
	Template   string
	PullPolicy string

	// PrimaryWorkspace is the environment's own root workspace, in object
	// form (§6.2). PersonalContextWorkspace is the UNCONDITIONALLY
	// mounted personal-context workspace (workflow/launch's MountDirs
	// precedent: "mount it UNCONDITIONALLY, creating it if absent"); Path
	// == "" here specifically means the caller found no personal context
	// directory configured at all, not that mounting it is conditional.
	PrimaryWorkspace         WorkspaceFact
	PersonalContextWorkspace WorkspaceFact

	// MixinKit is the generated Pi mixin kit's REFERENCE ONLY — a
	// directory path or a kit URL string, never its content. This package
	// never creates, writes, or cleans up whatever that reference points
	// at; see this file's own "Renderer/materializer split" section.
	MixinKit string

	// DevKit is populated only for a `--dev` launch. Zero value means an
	// ordinary launch — see DevKitFact's own doc comment.
	DevKit DevKitFact

	// PixEnvVars are Pix-owned sandbox environment variables. They are
	// merged over the authored `env:` block (a Pix-required variable is
	// required, so it wins a key collision) and, like every other map
	// here, render in sorted-key order. A secret value must never appear
	// here — a secret stays a `secrets.<name>` reference on Document,
	// never a resolved value threaded through RuntimeFacts
	// (render_test.go's TestRenderEffective_NoSecretValues).
	PixEnvVars map[string]string

	// MCPServers is the caller's already-composed, already-reviewed MCP
	// server set (see MCPWrapperFact). A NON-nil value is authoritative
	// and replaces the authored `mcp.servers` list wholesale — it IS that
	// list, already credential-wrapped. A nil value means the caller
	// composed no MCP facts at all, and the authored document's own
	// servers render verbatim (unwrapped), so a document's stable
	// identities can never be silently dropped by a caller that only
	// wanted a document rendering.
	MCPServers []MCPWrapperFact
}

// ErrNoDocument is RenderEffective's answer to a nil RuntimeFacts.Document.
// There is no "render without a document" mode: even D17's `none` state has
// a document (Pix's built-in defaults), so a nil one is a caller bug, and
// fabricating an empty document in its place would silently render an
// environment nobody declared.
var ErrNoDocument = errors.New(
	"envinfo: cannot render an effective document without a parsed native document")

// RenderEffective renders facts into the byte-identical effective
// `.sbxenv.yaml` document Pix would hand to `sbx env create` (§6.2). It
// is deterministic: the same RuntimeFacts value always produces the same
// bytes, with no dependency on wall-clock time, map iteration order, or
// any other unstable input.
func RenderEffective(facts RuntimeFacts) ([]byte, error) {
	doc := facts.Document
	if doc == nil {
		return nil, ErrNoDocument
	}

	out := effectiveDocument{
		SchemaVersion:  effectiveSchemaVersion(doc),
		Agent:          doc.Agent,
		Name:           effectiveName(facts),
		Workspaces:     effectiveWorkspaces(facts),
		Kits:           effectiveKits(facts),
		SandboxOptions: effectiveSandboxOptions(facts),
		Env:            effectiveEnv(facts),
		Secrets:        effectiveSecrets(doc),
		Registries:     effectiveRegistries(doc),
		Ports:          doc.Ports,
	}
	if len(doc.Bindings) > 0 {
		out.Bindings = doc.Bindings
	}
	if servers := effectiveMCPServers(facts); len(servers) > 0 {
		out.MCP = &MCPBlock{Servers: servers}
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(out); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// effectiveSchemaVersion keeps the authored version when there is one. An
// unset version can only reach here from a caller-constructed document
// (Parse rejects an unsupported one), and a document with no
// `schemaVersion` is not loadable at all, so the one accepted version is
// rendered rather than an empty key.
func effectiveSchemaVersion(doc *Document) string {
	if doc.SchemaVersion != "" {
		return doc.SchemaVersion
	}
	return SchemaVersionV1
}

// effectiveName prefers the caller's pre-composition `pix-*` identity
// (§6.2: "composition never determines identity"), falling back to an
// authored `name:` only when the caller supplied none.
func effectiveName(facts RuntimeFacts) string {
	if facts.SandboxName != "" {
		return facts.SandboxName
	}
	return facts.Document.Name
}

// effectiveWorkspaces renders the primary workspace followed by the
// personal-context workspace, each in object form, skipping any fact
// whose Path is unset.
func effectiveWorkspaces(facts RuntimeFacts) []effectiveWorkspace {
	var out []effectiveWorkspace
	for _, ws := range []WorkspaceFact{facts.PrimaryWorkspace, facts.PersonalContextWorkspace} {
		if ws.Path == "" {
			continue
		}
		out = append(out, effectiveWorkspace{Path: ws.Path, ReadOnly: ws.ReadOnly, Clone: ws.Clone})
	}
	return out
}

// effectiveKits renders the authored kits first (already resolved against
// their own source directory by Parse — this package never re-resolves a
// kit path), then the generated Pi mixin kit, then a `--dev` checkout kit.
// Order is fixed and caller-independent so the same facts always produce
// the same list.
func effectiveKits(facts RuntimeFacts) []string {
	var out []string
	for _, k := range facts.Document.Kits {
		if k.Resolved != "" {
			out = append(out, k.Resolved)
			continue
		}
		if k.Raw != "" {
			out = append(out, k.Raw)
		}
	}
	if facts.MixinKit != "" {
		out = append(out, facts.MixinKit)
	}
	if facts.DevKit.Kit != "" {
		out = append(out, facts.DevKit.Kit)
	}
	return out
}

// effectiveSandboxOptions merges the authored `sandboxOptions:` scalars
// with Pix's own pinned template/pullPolicy. Pix's pinned values win a key
// collision: the pinned template is the image Pix is contracted to run,
// not a preference an authored file may quietly redirect.
func effectiveSandboxOptions(facts RuntimeFacts) map[string]string {
	opts := map[string]string{}
	for k, v := range facts.Document.SandboxOptions {
		opts[k] = v
	}
	if facts.Template != "" {
		opts["template"] = facts.Template
	}
	if facts.PullPolicy != "" {
		opts["pullPolicy"] = facts.PullPolicy
	}
	if len(opts) == 0 {
		return nil
	}
	return opts
}

// effectiveEnv merges the authored `env:` block with Pix-required
// variables, the Pix values winning. The result is always non-nil: an
// environment with no variables renders `env: {}`, an explicit empty
// declaration, rather than a missing key a later reader could confuse
// with "not composed yet". Authored values may still be `${VAR}`
// expressions — this package surfaces them and never resolves one
// (doc.go's "Interpolation is surfaced, never resolved").
func effectiveEnv(facts RuntimeFacts) map[string]string {
	env := map[string]string{}
	for k, v := range facts.Document.Env {
		env[k] = v
	}
	for k, v := range facts.PixEnvVars {
		env[k] = v
	}
	return env
}

// effectiveSecrets carries every authored `secrets.<name>` locator through
// unchanged — ref or command, never a value (effectiveSecret has no value
// field to carry one).
func effectiveSecrets(doc *Document) map[string]effectiveSecret {
	if len(doc.Secrets) == 0 {
		return nil
	}
	out := make(map[string]effectiveSecret, len(doc.Secrets))
	for name, s := range doc.Secrets {
		out[name] = effectiveSecret{Ref: s.Ref, Command: s.Command}
	}
	return out
}

// effectiveRegistries is effectiveSecrets' registry twin, plus noVerify.
func effectiveRegistries(doc *Document) map[string]effectiveRegistry {
	if len(doc.Registries) == 0 {
		return nil
	}
	out := make(map[string]effectiveRegistry, len(doc.Registries))
	for host, r := range doc.Registries {
		out[host] = effectiveRegistry{Ref: r.Ref, Command: r.Command, NoVerify: r.NoVerify}
	}
	return out
}

// effectiveMCPServers renders each server independently: one server's argv
// is copied into its own entry and never appended to, or read from,
// another's (§9.2's per-server isolation). A non-nil caller-composed set is
// authoritative; a nil one falls back to the authored servers verbatim (see
// RuntimeFacts.MCPServers).
func effectiveMCPServers(facts RuntimeFacts) []MCPServer {
	if facts.MCPServers == nil {
		return facts.Document.MCP.Servers
	}
	out := make([]MCPServer, 0, len(facts.MCPServers))
	for _, srv := range facts.MCPServers {
		entry := MCPServer{Name: srv.Name, URL: srv.URL, Command: srv.Command}
		if len(srv.Args) > 0 {
			entry.Args = append([]string(nil), srv.Args...)
		}
		out = append(out, entry)
	}
	return out
}
