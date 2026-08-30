// effective.go — the wiring for `pix env [NAME] --effective` (docs/design/
// pix-v2-surface.md §3.4: "`--effective` uses the current directory as the
// project workspace and prints the exact native sbx environment Pix would
// use for a new sandbox without creating one") to envinfo's ONE stable
// effective-document producer, envinfo.RenderEffective. This file is the
// ONLY place this module calls envinfo.RenderEffective from a PREVIEW path
// (F17; see services/host/arch_effective_test.go's
// TestArchitecture_ExactlyOneEffectiveDocumentProducer) — cmd/pix reaches
// it exclusively through RenderEffectiveDocument below, never by importing
// envinfo directly for this purpose. workflow/launch's real launch
// composition is the renderer's OTHER, richer call site; both call the
// SAME function so a preview and a real create can never silently diverge.
//
// ComputeEffective composes a DETERMINISTIC PREVIEW: it never invokes
// `sbx` (no create, no live probe of any kind) and never materializes or
// writes a generated kit directory — that split (RenderEffective emits
// document bytes only; a generated kit dir stays launch-owned) is
// render.go's own "Renderer/materializer split" section. It still adds
// Pix's two RESERVED built-in MCP declarations (pix-memory, pix-session:
// docs/design/pix-v2-architecture.md §10) exactly as a real launch does
// (cmd/pix's runEffectiveInput/builtinMCPFacts), so `--effective` never
// shows a shape a real create would then silently add to.
package env

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"pix/host/config"
	"pix/host/envinfo"
	"pix/host/mcp"
	"pix/host/pixhome"
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
//
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

// effectiveMemoryHostPort mirrors cmd/pix/homeadapters.go's own
// memoryHostPort: the fixed loopback port pix-memory publishes on. Same
// sibling-import ban as effectiveTemplateRepo above — cmd/pix is a HIGHER
// layer this package may never import — so the value is duplicated with
// this comment as the tripwire: if that port ever becomes configurable,
// both copies must change together, or a preview's pix-memory URL would
// silently disagree with what a real launch composes.
const effectiveMemoryHostPort = 18080

// effectiveSessionSubcommandArg mirrors cmd/pix/run_env.go's
// mcpSessionSubcommand: pix-session's reserved argv[1]. Kept as a
// duplicated literal for the same reason as effectiveMemoryHostPort —
// cmd/pix cannot be imported from here.
const effectiveSessionSubcommandArg = "mcp-session"

// resolveEffectiveName is ComputeEffective's/`env [NAME]`'s shared name
// resolution: an explicit positional wins; otherwise the machine default
// (pixhome.LoadMachine); an empty result is D17's `none` state, not an
// error.
func resolveEffectiveName(home pixhome.Paths, explicit string) (string, bool, error) {
	name := strings.TrimSpace(explicit)
	if name != "" {
		return name, true, nil
	}
	m, err := pixhome.LoadMachine(home)
	if err != nil {
		return "", false, err
	}
	name = strings.TrimSpace(m.DefaultEnvironment)
	return name, name != "", nil
}

// ComputeEffective composes envinfo.RuntimeFacts for `pix env [NAME]
// --effective`: name resolves exactly as the rest of `pix env` does
// (explicit positional, else the machine default, else Pix's own
// built-in-defaults document), and every fact beyond that is derived from
// this HOST's own, already-loaded filesystem/PATH state — never a live
// sandbox call.
func ComputeEffective(home pixhome.Paths, explicit string) (envinfo.RuntimeFacts, error) {
	name, ok, err := resolveEffectiveName(home, explicit)
	if err != nil {
		return envinfo.RuntimeFacts{}, err
	}

	var (
		doc     *envinfo.Document
		sidecar *envinfo.Sidecar
	)
	if ok {
		sel, err := ResolveIn(home, name)
		if err != nil {
			return envinfo.RuntimeFacts{}, err
		}
		loaded, err := LoadHome(sel, nil, nil)
		if err != nil {
			return envinfo.RuntimeFacts{}, err
		}
		doc, sidecar = loaded.Document, loaded.Sidecar
	} else {
		// D17's "none" state: Pix's own built-in defaults, never an error
		// (docs/design/environments.md §6.2: "With environment selection
		// resolved to none, the effective file is generated from Pix's
		// built-in defaults").
		doc = &envinfo.Document{SchemaVersion: envinfo.SchemaVersionV1}
	}

	// The PRIMARY workspace is always this PROCESS's own project directory
	// (pix-v2-surface.md §3.4: "--effective uses the current directory as
	// the project workspace"), exactly as a real launch's own
	// PrimaryWorkspaceFact(o.Workspace) uses the run's project directory —
	// never the selected environment's own source root, which holds
	// declarations, not code to work on.
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
			Path: cwd,
		},
		PersonalContextWorkspace: envinfo.WorkspaceFact{
			Path: config.ContextDir(),
		},
		// MixinKit names no real path here: `env --effective` is a preview
		// and never materializes a kit directory (this file's own doc
		// comment) — a real reference is a launch-time fact this unit does
		// not invent.
		MixinKit:   "",
		PixEnvVars: map[string]string{},
		MCPServers: envinfo.WithBuiltinMCPServers(mcpWrapperFacts(doc, sidecar), builtinMCPFacts(home)),
	}
	return facts, nil
}

// RenderEffectiveDocument is `pix env [NAME] --effective`'s ONE call site
// into envinfo.RenderEffective (F17, this file's own doc comment).
func RenderEffectiveDocument(home pixhome.Paths, explicit string) ([]byte, error) {
	facts, err := ComputeEffective(home, explicit)
	if err != nil {
		return nil, err
	}
	return envinfo.RenderEffective(facts)
}

// builtinMCPFacts resolves docs/design/pix-v2-architecture.md §10's two
// reserved built-ins for THIS host, mirroring cmd/pix/run_env.go's own
// builtinMCPFacts exactly (same URL shape, same reserved argv) so a
// preview never disagrees with what a real launch composes. Either half
// degrades to "omit that built-in" rather than failing the preview: an
// unresolvable running executable is a `pix doctor`-shaped gap, not a
// reason `env --effective` should refuse to render anything at all.
func builtinMCPFacts(home pixhome.Paths) envinfo.BuiltinMCPFacts {
	var facts envinfo.BuiltinMCPFacts
	facts.MemoryURL = fmt.Sprintf("http://127.0.0.1:%d/mcp", effectiveMemoryHostPort)
	if exe, err := os.Executable(); err == nil {
		if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			exe = resolved
		}
		facts.SessionCommand = exe
		facts.SessionArgs = []string{effectiveSessionSubcommandArg}
	}
	return facts
}

// mcpWrapperFacts composes the already-reviewed MCP wrapper facts §9.2
// describes: a local-command server whose pix.toml [host.mcp.<name>]
// declares env_keys is wrapped with the ONE existing op-run grammar this
// module has, package mcp's OpRunWrap — never a second, hand-built copy of
// it (arch_effective_test.go's TestArchitecture_NoDuplicateOpRunGrammar).
// A server with no declared env_keys, or when `op` / the refs file are not
// present on this host, renders its bare argv unchanged — OpRunWrap's own
// no-op behavior for that case, reused rather than reimplemented here.
func mcpWrapperFacts(doc *envinfo.Document, sidecar *envinfo.Sidecar) []envinfo.MCPWrapperFact {
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
