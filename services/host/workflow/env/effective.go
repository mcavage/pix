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
// (cmd/pix's runEffectiveInput calls the same BuiltinMCPFacts below), so
// `--effective` never shows a shape a real create would then silently add to.
package env

import (
	"os"
	"path/filepath"
	"strings"

	"pix/host/config"
	"pix/host/container"
	"pix/host/envinfo"
	"pix/host/pixhome"
	"pix/host/sandbox"
	"pix/host/stack"
)

// Leave the template absent so sbx uses the selected kit's pinned image.
// A bare image repository would override that pin with a cached :latest.
const effectivePullPolicyMissing = "missing"

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
func ComputeEffective(home pixhome.Paths, explicit, launcherVersion string) (envinfo.RuntimeFacts, error) {
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
	sandboxName, err := sandbox.Name(cwd)
	if err != nil {
		return envinfo.RuntimeFacts{}, err
	}

	servers, err := EnvironmentFacts(doc, sidecar, home.Home)
	if err != nil {
		return envinfo.RuntimeFacts{}, err
	}
	facts := envinfo.RuntimeFacts{
		Document:    doc,
		Sidecar:     sidecar,
		SandboxName: sandboxName,
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
		MixinKit: "",
		// The SAME Pix-managed env block a real launch composes (cmd/pix's
		// runEffectiveInput): the stamped launcher build this preview is
		// running as, and this PIX_HOME's stack id. Composed through the ONE
		// producer, envinfo.PixManagedEnvVars, so `--effective` never shows an
		// env block a real create would then silently add to.
		PixEnvVars: envinfo.PixManagedEnvVars(launcherVersion, HomeStackID(home)),
		MCPServers: envinfo.WithBuiltinMCPServers(servers, BuiltinMCPFacts(home, cwd, sandboxName, false)),
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
func RenderEffectiveDocument(home pixhome.Paths, explicit, launcherVersion string) ([]byte, error) {
	facts, err := ComputeEffective(home, explicit, launcherVersion)
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

// HomeStackID is this PIX_HOME's stack id for the Pix-managed PIX_STACK_ID
// environment fact, degrading to "" (the fact is omitted, never rendered
// empty) on the same terms BuiltinMCPFacts degrades on — never a guessed or
// placeholder id: a wrong stack id in a sandbox's environment is worse than
// an absent one. Both the preview and a real launch call this ONE function.
func HomeStackID(home pixhome.Paths) string {
	id, err := stack.ID(home.Home)
	if err != nil {
		return ""
	}
	return id
}

// BuiltinMCPFacts resolves docs/design/pix-v2-architecture.md §10's two
// reserved built-ins for THIS host: pix-memory, the loopback Streamable HTTP
// endpoint `pix setup` reconciles and registers with the sbx Gateway (the
// SAME URL container.MemoryMCPURL composes for that registration — never a
// second, independently-derived one that could silently disagree), and the
// host-tools server, the Gateway-launched host stdio command that names this
// SAME running `pix` binary with envinfo.WithHostTools' argv for workspace,
// sandbox, and dev authority. `pix env --effective` (ComputeEffective) and a
// real launch (cmd/pix's runEffectiveInput) both call this ONE function, so
// a preview can never show a built-in shape a real create would then
// silently change.
//
// Either half degrades to "omit that built-in" rather than failing the
// caller: a stack id that cannot be derived omits BOTH built-ins (never a
// bare legacy-name fallback), and an unresolvable running executable omits
// the session command — a `pix doctor`-shaped gap, not a reason to refuse a
// preview or every `pix run` outright. The bearer token and port are
// read-only here, never generated or allocated (that is `pix setup`'s job
// alone: container.EnsureMemoryAuthToken / EnsureMemoryPort), so a call
// before `pix setup` omits the token and shows container.DefaultMemoryPort,
// the same "not ready yet" value. PIX_HOST_TOOLS_DISABLED=1 removes the
// host-tools built-in entirely.
func BuiltinMCPFacts(home pixhome.Paths, workspace, sandboxName string, dev bool) envinfo.BuiltinMCPFacts {
	var facts envinfo.BuiltinMCPFacts
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
	}
	// WithHostTools owns the session argv (envinfo.HostToolsSubcommand plus
	// its --home/--workspace/--sandbox context); nothing here pre-fills a
	// different subcommand it would then overwrite.
	return envinfo.WithHostTools(facts, home.Home, workspace, sandboxName, dev, os.Getenv("PIX_HOST_TOOLS_DISABLED") == "1")
}
