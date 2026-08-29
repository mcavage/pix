// effective.go — E2.1's wiring of `pix env show --effective` (AC-54,
// docs/design/environments.md §6.2, §8.1) to envinfo's ONE stable
// effective-document producer, envinfo.RenderEffective. This file is the
// ONLY place this module calls envinfo.RenderEffective (F17; see
// services/host/arch_effective_test.go's
// TestArchitecture_ExactlyOneEffectiveDocumentProducer) — cmd/pix reaches
// it exclusively through RenderEffectiveDocument below, never by importing
// envinfo directly for this purpose.
//
// ComputeEffective composes a DETERMINISTIC PREVIEW: it never invokes
// `sbx` (no create, no live probe of any kind) and never materializes or
// writes a generated kit directory — that split (RenderEffective emits
// document bytes only; a generated kit dir stays launch-owned) is
// render.go's own "Renderer/materializer split" section. A future E2.5
// launch composition is expected to build its OWN, richer RuntimeFacts
// value (a live workspace decision, an actually-materialized mixin kit)
// and feed it through the SAME RenderEffective — never a second renderer —
// but building that value is explicitly out of this unit's scope (E2.5 is
// not implemented here).
package env

import (
	"os"
	"os/exec"

	"pix/host/config"
	"pix/host/envinfo"
	"pix/host/mcp"
	"pix/host/sandbox"
)

// effectiveTemplateRepo/effectivePullPolicyMissing are docs/design/
// environments.md §6.2's Pix template and `pullPolicy: missing`, rendered
// (per envinfo/render.go) as `sandboxOptions.template` /
// `sandboxOptions.pullPolicy` — the placement Story 0 created a real
// sandbox with (docs/upstream/sbx-0.39-environments.md §7).
//
// The REPO is all a preview can honestly name: which exact tag a launch
// pins is a launch-time decision (workflow/launch's BuildSbxArgs pins
// `<repo>:<out/.local-image-tag>` only when a resolved checkout carries
// one, and otherwise leaves the kit's own pinned `image:` to select it),
// and `env show --effective` makes no launch decision. A future E2.5
// composition supplies the exact resolved reference it actually used as
// RuntimeFacts.Template; this preview never invents a tag it cannot
// prove.
// This is a deliberate, small duplication of workflow/launch's
// DockerImageRepo constant, not an import of it: workflow/env may not
// import a sibling workflow package (this package's own doc comment,
// "no other workflow/* package"; arch_test.go's
// TestArchitecture_SiblingWorkflowRuleIsEnforced) — the same precedent
// hosttrust/sandbox already set by duplicating a few of packinfo's lines
// rather than importing across an equivalent boundary (arch_test.go's
// packinfo placement comment).
const (
	effectiveTemplateRepo      = "docker.io/mcavage/pix"
	effectivePullPolicyMissing = "missing"
)

// ComputeEffective composes envinfo.RuntimeFacts for `pix env show
// --effective`: name resolves exactly as ComputeShow's does (explicit
// positional, else the machine default, else Pix's own built-in-defaults
// document), and every fact beyond that is derived from this HOST's own,
// already-loaded config/filesystem/PATH state — never a live sandbox
// call.
func ComputeEffective(cfg *config.Config, explicit string) (envinfo.RuntimeFacts, error) {
	name, ok := resolvedShowName(cfg, explicit)

	var (
		doc     *envinfo.Document
		sidecar *envinfo.Sidecar
		root    string
	)
	if ok {
		ts, err := loadEnvironmentTrustStore()
		if err != nil {
			return envinfo.RuntimeFacts{}, err
		}
		loaded, err := Load(cfg, &ts.AcceptanceStore, name, nil, nil)
		if err != nil {
			return envinfo.RuntimeFacts{}, err
		}
		doc, sidecar, root = loaded.Document, loaded.Sidecar, loaded.Root
	} else {
		// D17's "none" state: Pix's own built-in defaults, never an error
		// (docs/design/environments.md §6.2: "With environment selection
		// resolved to none, the effective file is generated from Pix's
		// built-in defaults").
		doc = &envinfo.Document{SchemaVersion: envinfo.SchemaVersionV1}
	}

	cwd, err := os.Getwd()
	if err != nil {
		return envinfo.RuntimeFacts{}, err
	}

	facts := envinfo.RuntimeFacts{
		Document:    doc,
		Sidecar:     sidecar,
		SandboxName: sandbox.Name(cwd),
		Template:    effectiveTemplateRepo,
		PullPolicy:  effectivePullPolicyMissing,
		PrimaryWorkspace: envinfo.WorkspaceFact{
			Path: root,
		},
		PersonalContextWorkspace: envinfo.WorkspaceFact{
			Path: config.ContextDir(),
		},
		// MixinKit names no real path here: `show --effective` is a preview
		// and never materializes a kit directory (this file's own doc
		// comment) — a real reference is a launch-time fact this unit does
		// not invent.
		MixinKit:   "",
		PixEnvVars: map[string]string{},
		MCPServers: mcpWrapperFacts(cfg, doc, sidecar),
	}
	return facts, nil
}

// RenderEffectiveDocument is `pix env show --effective`'s ONE call site
// into envinfo.RenderEffective (F17, this file's own doc comment).
func RenderEffectiveDocument(cfg *config.Config, explicit string) ([]byte, error) {
	facts, err := ComputeEffective(cfg, explicit)
	if err != nil {
		return nil, err
	}
	return envinfo.RenderEffective(facts)
}

// mcpWrapperFacts composes the already-reviewed MCP wrapper facts §9.2
// describes: a local-command server whose pix.toml [host.mcp.<name>]
// declares env_keys is wrapped with the ONE existing op-run grammar this
// module has, package mcp's OpRunWrap — never a second, hand-built copy of
// it (arch_effective_test.go's TestArchitecture_NoDuplicateOpRunGrammar).
// A server with no declared env_keys, or when `op` / the refs file are not
// present on this host, renders its bare argv unchanged — OpRunWrap's own
// no-op behavior for that case, reused rather than reimplemented here.
func mcpWrapperFacts(cfg *config.Config, doc *envinfo.Document, sidecar *envinfo.Sidecar) []envinfo.MCPWrapperFact {
	if doc == nil {
		return nil
	}
	var hostMCP map[string]envinfo.HostMCPEntry
	if sidecar != nil {
		hostMCP = sidecar.Host.MCP
	}
	var out []envinfo.MCPWrapperFact
	for _, srv := range doc.MCP.Servers {
		fact := envinfo.MCPWrapperFact{Name: srv.Name, URL: srv.URL}
		if srv.Command == "" {
			out = append(out, fact)
			continue
		}
		argv := append([]string{srv.Command}, srv.Args...)
		if entry, ok := hostMCP[srv.Name]; ok && len(entry.EnvKeys) > 0 {
			argv = opRunWrapIfAvailable(argv)
		}
		fact.Command = argv[0]
		fact.Args = argv[1:]
		out = append(out, fact)
	}
	return out
}

// opRunWrapIfAvailable calls mcp.OpRunWrap with this host's own resolved
// `op` binary path and op-refs.env location, or returns argv unchanged
// when either is absent (1Password remains optional — mcp.OpRunWrap's own
// no-op behavior for opPath == "" || opRefs == "").
func opRunWrapIfAvailable(argv []string) []string {
	opPath, err := exec.LookPath("op")
	if err != nil {
		return argv
	}
	refs := config.OpRefsPath()
	if _, err := os.Stat(refs); err != nil {
		return argv
	}
	return mcp.OpRunWrap(opPath, refs, argv)
}
