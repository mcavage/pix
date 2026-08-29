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
// # Renderer/materializer split
//
// docs/design/environments.md §6.2 lists the Pix-owned runtime facts a
// caller composes: a pinned template/pullPolicy, the primary workspace in
// object form (including clone choice), an unconditional personal-context
// workspace, a generated Pi mixin kit, a dev-checkout kit and live skill
// arguments when `--dev`, Pix-required environment variables, and
// reviewed local-MCP credential wrappers. RenderEffective renders exactly
// the REFERENCE to a generated kit (MixinKit / DevKit.Kit — a directory
// path or a kit URL string) that RuntimeFacts already carries; it never
// creates, materializes, or writes that directory itself, and it performs
// no cleanup of one either. Materializing a kit directory (and reusing
// launch's EXISTING cleanup for it — this unit does not invent a second
// one) stays whichever caller actually launches something; `env show
// --effective`'s own caller (workflow/env's ComputeEffective) composes a
// deterministic PREVIEW precisely so it never has to materialize or clean
// up anything at all.
//
// # Current status: E2.1 RED checkpoint
//
// RenderEffective's typed contract (this file) and render_test.go's
// golden/isolation/Story-0-cross-check assertions are written against the
// FINAL behavior this function must have. The body below intentionally
// does not implement it yet — every caller (workflow/env's
// ComputeEffective/RenderEffectiveDocument, effective.go) and every test
// in render_test.go already compile and run against the real signature,
// and fail for exactly this one, named reason. That is this unit's
// deliverable: the stable typed API a follow-up unit implements against,
// proven by tests that currently fail for the right reason, not a
// half-finished renderer nobody asked for yet.
package envinfo

import "errors"

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
// RenderEffective renders neither the dev kit nor any live-skill argument
// then.
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
	Document *Document

	// Sidecar is the caller's already-parsed optional pix.toml, or nil
	// when the environment has none (envinfo.Sidecar's own "optional"
	// contract, sidecar.go). Models/Agents/Pi.Skills/Host/Inference all
	// flow into the effective document from here — RenderEffective is
	// where "resolve model/agents/mcp/skills facts" (this unit's scope)
	// becomes concrete rendering, never a second, independent resolution.
	Sidecar *Sidecar

	// SandboxName is the caller-computed `pix-*` sandbox name (docs/
	// design/environments.md §6.2: "Sandbox identity is attributed before
	// composition"). This package never derives a name of its own — the
	// same discipline doc.go already states for envinfo as a whole.
	SandboxName string

	// Template/PullPolicy are the pinned image reference and pull policy
	// (§6.2: "pinned Pix template and `pullPolicy: missing`"). PullPolicy
	// == "" means the caller declared none explicitly — RenderEffective
	// never fabricates a default value for it.
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

	// PixEnvVars are Pix-owned sandbox environment variables. A secret
	// value must never appear here — a secret stays a `secrets.<name>`
	// reference on Document, never a resolved value threaded through
	// RuntimeFacts (render_test.go's TestRenderEffective_NoSecretValues).
	PixEnvVars map[string]string

	// MCPServers is the caller's already-composed, already-reviewed MCP
	// server set (see MCPWrapperFact).
	MCPServers []MCPWrapperFact
}

// ErrRenderEffectiveNotImplemented is RenderEffective's current answer.
// This is the E2.1 RED checkpoint this file's own package doc comment
// describes: the typed contract exists and every caller already compiles
// and runs against it, but the deterministic byte-serialization body does
// not exist yet. The message deliberately says only "not yet available" —
// the same phrase workflow/env's pre-existing ErrEffectiveNotAvailable
// already used at the CLI boundary — and never names this internal unit
// (cmd/pix/env_cmd_test.go's TestEnvShow_EffectiveNotYetAvailable already
// pins "never name E2.1" at that boundary; this is the same discipline one
// layer down).
var ErrRenderEffectiveNotImplemented = errors.New(
	"envinfo: the effective document renderer is not yet available; RenderEffective's typed contract is defined but its rendering body is not implemented")

// RenderEffective renders facts into the byte-identical effective
// `.sbxenv.yaml` document Pix would hand to `sbx env create` (§6.2). It
// is deterministic: the same RuntimeFacts value must always produce the
// same bytes, with no dependency on wall-clock time, map iteration order,
// or any other unstable input.
//
// See this file's package doc comment for why the body below is not yet
// implemented.
func RenderEffective(facts RuntimeFacts) ([]byte, error) {
	return nil, ErrRenderEffectiveNotImplemented
}
