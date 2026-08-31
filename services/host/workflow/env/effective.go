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
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"pix/host/config"
	"pix/host/container"
	"pix/host/envinfo"
	"pix/host/mcp"
	"pix/host/pixhome"
	"pix/host/sandbox"
	"pix/host/stack"
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

// effectiveSessionSubcommandArg mirrors cmd/pix/run_env.go's
// mcpSessionSubcommand: pix-session's reserved argv[1]. Kept as a
// duplicated literal for the same reason as effectiveMemoryHostPort —
// cmd/pix cannot be imported from here.
const effectiveSessionSubcommandArg = "mcp-session"

// resolveEffectiveName is ComputeEffective's/`env [NAME]`'s shared name
// resolution: an explicit positional wins; otherwise the machine default
// (config.Config.DefaultEnvironment, the sole config.toml schema); an empty
// result is D17's `none` state, not an error.
func resolveEffectiveName(home pixhome.Paths, explicit string) (string, bool, error) {
	name := strings.TrimSpace(explicit)
	if name != "" {
		return name, true, nil
	}
	c, err := config.LoadFrom(config.PathAt(home.Home))
	if err != nil {
		return "", false, err
	}
	name = strings.TrimSpace(c.DefaultEnvironment)
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
// into envinfo.RenderEffective (F17, this file's own doc comment). It is a
// DISPLAY path only — env_cmd.go's --effective is its one caller, and it
// writes the returned bytes straight to a terminal/pipe; a real launch
// composes its own effective document through a completely separate call
// chain (workflow/launch's RenderEffectiveEnvironment) and never reads
// these bytes. L1 (security re-review): the pix-memory bearer token must
// never reach that display, so the pix-memory server's URL is redacted at
// its token VALUE only, after ComputeEffective has resolved every fact
// (including the real token) exactly as a real launch would — the
// redaction is presentation-only, applied to the copy this function
// returns, never to what any other caller computes or persists.
func RenderEffectiveDocument(home pixhome.Paths, explicit string) ([]byte, error) {
	facts, err := ComputeEffective(home, explicit)
	if err != nil {
		return nil, err
	}
	redactBuiltinMemoryToken(&facts)
	return envinfo.RenderEffective(facts)
}

// redactBuiltinMemoryToken replaces the reserved pix-memory MCP server's
// URL with a token-redacted copy, in place, on facts.MCPServers — the ONE
// entry WithBuiltinMCPServers ever names a pix-memory built-in under
// (envinfo.IsMemoryMCPName covers both the bare legacy name and THIS
// PIX_HOME's own scoped one). Every other server (an authored environment's
// own) is left untouched: this function redacts Pix's OWN generated
// credential, never anything a reviewer authored themselves and can
// already see in their own file.
func redactBuiltinMemoryToken(facts *envinfo.RuntimeFacts) {
	for i := range facts.MCPServers {
		if envinfo.IsMemoryMCPName(facts.MCPServers[i].Name) {
			facts.MCPServers[i].URL = container.RedactMemoryURLToken(facts.MCPServers[i].URL)
		}
	}
}

// builtinMCPFacts resolves docs/design/pix-v2-architecture.md §10's two
// reserved built-ins for THIS host, mirroring cmd/pix/run_env.go's own
// builtinMCPFacts exactly (same URL shape — container.ReadMemoryPort is the
// ONE canonical per-PIX_HOME port both copies now read, QA F4: two
// independent PIX_HOME instances no longer share one fixed value — same
// reserved argv) so a preview never disagrees with what a real launch
// composes. Either half degrades to "omit that built-in" rather than
// failing the preview: an unresolvable running executable is a `pix
// doctor`-shaped gap, not a reason `env --effective` should refuse to
// render anything at all. The bearer token (security re-review HIGH
// finding) is read-only here, never generated: a preview run before `pix
// setup` simply omits it, same as an unresolvable session command. The port
// is read-only too — never allocated here, only `pix setup`'s
// container.EnsureMemoryPort does that — so a preview before `pix setup`
// shows container.DefaultMemoryPort, the same "not ready yet" display value.
func builtinMCPFacts(home pixhome.Paths) envinfo.BuiltinMCPFacts {
	var facts envinfo.BuiltinMCPFacts
	// This PIX_HOME's own scoped built-in names (Wave B coexistence): a
	// stack id that cannot be derived degrades to omitting BOTH built-ins
	// entirely, the same "unresolvable yet" posture an unresolvable running
	// executable already gets below — never a bare legacy-name fallback.
	if id, err := stack.ID(home.Home); err == nil {
		facts.MemoryName, _ = stack.MCPMemoryName(id)
		facts.SessionName, _ = stack.MCPSessionName(id)
	}
	token, _ := container.ReadMemoryAuthToken(home)
	port := container.DefaultMemoryPort
	if p, err := container.ReadMemoryPort(home); err == nil {
		port = p
	}
	facts.MemoryURL = container.MemoryMCPURL(container.Spec{HostPort: port}, token)
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
