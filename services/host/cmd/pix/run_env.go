// run_env.go — L4's half of E2.5: turning `--env NAME` (or the configured
// default) into the ONE resolved environment a launch composes its real
// RuntimeFacts from, and the two lookups only this layer can do (the
// config, the shipped agent roster). The composition itself, the render,
// the fingerprint and the persisted bytes all live in workflow/launch —
// this file never renders an effective document of its own.
package main

import (
	"os"
	"path/filepath"
	"strings"

	"pix/host/config"
	"pix/host/container"
	"pix/host/envinfo"
	"pix/host/pixhome"
	"pix/host/sandbox"
	"pix/host/workflow/launch"
	"pix/host/workflow/models"

	nativeenv "pix/host/workflow/env"
)

// resolveRunEnvironment resolves §3.1's selection order — explicit `--env`,
// else the machine default, else `none` — with ONE hard rule: an explicit
// name is EXACT. An unknown one returns the pixhome resolver's own
// unknown-environment refusal (known names, how to create one) and the
// caller exits non-zero having created nothing; it NEVER degrades to the
// configured default, because a typo silently launching the wrong
// credential set is the worst outcome this feature can produce.
//
// This is the v2 selection model (docs/design/pix-v2-surface.md §3.4): an
// environment is a directory under PIX_HOME/envs, resolved by workflow/env's
// pixhome-based ResolveIn — there is no config.Environments registry left in
// this path, and no fallback to one. Nothing here writes config: `--env`
// selects for this run only (AC-22).
func resolveRunEnvironment(explicit string) (launch.EnvSelection, envTrustSnapshot, error) {
	home, err := pixhome.Resolve()
	if err != nil {
		return launch.EnvSelection{}, envTrustSnapshot{}, err
	}
	machine, err := pixhome.LoadMachine(home)
	if err != nil {
		return launch.EnvSelection{}, envTrustSnapshot{}, err
	}
	name := strings.TrimSpace(explicit)
	if name == "" {
		name = strings.TrimSpace(machine.DefaultEnvironment)
	}
	if name == "" {
		// D17's `none`: no environment registered or selected. The built-in
		// effective document still renders; a launch is never blocked by the
		// absence of an environment.
		return launch.EnvSelection{}, envTrustSnapshot{}, nil
	}
	sel, err := nativeenv.ResolveIn(home, name)
	if err != nil {
		return launch.EnvSelection{}, envTrustSnapshot{}, err
	}
	loaded, err := nativeenv.LoadHome(sel, nil, nil)
	if err != nil {
		return launch.EnvSelection{}, envTrustSnapshot{}, err
	}
	// M1 (security re-review, trust TOCTOU): the snapshot's bom/fingerprint
	// are computed from THIS loaded value, once — the exact bytes/tree
	// runEffectiveInput compiles the launch from below — never a second,
	// independent re-read of the environment directory.
	snap, err := resolveEnvTrustSnapshot(home, sel, loaded)
	if err != nil {
		return launch.EnvSelection{}, envTrustSnapshot{}, err
	}
	reviewed := trustAcceptedForFingerprint(home, sel, snap.fingerprint)
	return launch.EnvSelection{
		Name:     loaded.Name,
		Root:     loaded.Root,
		Document: loaded.Document,
		Sidecar:  loaded.Sidecar,
		Tree:     loaded.Tree,
		Reviewed: reviewed,
	}, snap, nil
}

// runEffectiveInput is the launch's real RuntimeFacts input: the ACTUAL
// pix-* sandbox name (already resolved), the pinned image plus §6.2's
// `pullPolicy: missing`, the primary workspace in object form, the
// unconditional personal-context workspace, the generated mixin kit, and
// `--dev`'s checkout kit with its live skill arguments. Every value is one
// this launch already decided; nothing is re-derived here.
func runEffectiveInput(cfg *config.Config, o launch.RunOpts, sel launch.EnvSelection, version string) (launch.EffectiveInput, error) {
	template := o.Template
	if template == "" {
		if o.LocalImageTag != "" {
			template = launch.DockerImageRepo + ":" + o.LocalImageTag
		} else {
			template = launch.DockerImageRepo
		}
	}
	// The PRIMARY workspace is this run's own project directory — `pix run
	// DIR`, else the current directory. The selected environment's SOURCE
	// ROOT is a different thing and is never substituted for it: an
	// environment root holds declarations, and §5.1 restriction 4 requires
	// it to resolve outside every writable workspace it mounts. The old
	// `sel.Root != "" && primary == ""` fallback could only ever fire on an
	// empty workspace string, which is exactly the accident
	// PrimaryWorkspaceFact now refuses outright.
	primary, err := launch.PrimaryWorkspaceFact(o.Workspace)
	if err != nil {
		return launch.EffectiveInput{}, err
	}
	in := launch.EffectiveInput{
		Selection:        sel,
		SandboxName:      o.Name,
		Template:         template,
		PullPolicy:       launch.EffectivePullPolicy,
		PrimaryWorkspace: primary,
		PersonalContext:  envinfo.WorkspaceFact{Path: config.ContextDir()},
		PixEnvVars:       map[string]string{},
	}
	// `sbx env create` reads ONLY this document, so every mount and kit the
	// pre-cutover `sbx run` argv carried has to travel inside it. Both lists
	// are composed from the SAME producers the old argv used (MountDirs,
	// EnvExtraKits over BuildSbxArgs' own kit order), so an active pack's
	// contributed skills/knowledge and mixin kits reach the sandbox exactly
	// as before rather than being silently dropped at the cutover.
	mounts := launch.MountDirs(cfg, o)
	if o.Dev && o.DevRoot != "" {
		mounts = append(mounts, filepath.Join(o.DevRoot, "skills"))
	}
	in.AdditionalWorkspaces = launch.WorkspaceFacts(mounts)
	in.ExtraKits = launch.EnvExtraKits(cfg, o, version)
	if len(o.PackKits) > 0 {
		// The generated Pi mixin kit is the FIRST kit this launch
		// materialized (inference, then personal context); the rest stack as
		// ordinary extra kits. RenderEffective renders references only.
		in.MixinKit = o.PackKits[0]
	}
	if o.Dev {
		in.DevKit = envinfo.DevKitFact{Kit: o.LocalKit, LiveSkills: launch.LiveSkillDirs(cfg, o)}
	}
	// The environment's OWN declared servers (with their reviewed pix.toml
	// credential wrappers) plus the host-global names this create preloads.
	in.EnvMCPServers = launch.EnvMCPWrapperFacts(sel.Document, sel.Sidecar)
	in.MCPServers = envinfo.WithBuiltinMCPServers(
		launch.ComposeMCPServerFacts(in.EnvMCPServers, o.StaticMCP),
		builtinMCPFacts(),
	)
	return in, nil
}

// builtinMCPFacts resolves docs/design/pix-v2-architecture.md §10's two
// reserved built-ins for THIS host: pix-memory, the loopback Streamable
// HTTP endpoint `pix setup` reconciles and registers with the sbx Gateway
// (the SAME URL homeContainerSpec/container.MemoryMCPURL compose for that
// registration — never a second, independently-derived one that could
// silently disagree), and pix-session, the Gateway-launched host stdio
// command that names this SAME running `pix` binary.
//
// Either half degrades to "omit that built-in" rather than failing the
// launch: an unresolved PIX_HOME or an unresolvable running executable is
// an environment problem doctor already surfaces, not a reason to refuse
// every `pix run` outright. pix-session's actual in-sandbox behavior is not
// yet implemented — see this repo's host-UAT tracking for that gap; this
// function only emits the reserved declaration a future implementation
// fills in.
func builtinMCPFacts() envinfo.BuiltinMCPFacts {
	var facts envinfo.BuiltinMCPFacts
	if home, err := pixhome.Resolve(); err == nil {
		// Read-only: a launch never GENERATES the token (that is `pix setup`'s
		// job alone, container.EnsureMemoryAuthToken) — a missing token here
		// degrades the same way an unresolvable session command already does,
		// by omitting the built-in rather than failing the launch.
		token, _ := container.ReadMemoryAuthToken(home)
		facts.MemoryURL = container.MemoryMCPURL(homeContainerSpec(home), token)
	}
	if exe, err := os.Executable(); err == nil {
		if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			exe = resolved
		}
		facts.SessionCommand = exe
		facts.SessionArgs = []string{mcpSessionSubcommand}
	}
	return facts
}

// mcpSessionSubcommand is the pix-session built-in's argv[1]: the SAME
// constant sessionctl.go's dispatch intercepts before kong ever sees argv
// (hiddenSessionMCPVerb, "__pix-session-mcp"), not an independent literal.
// A Gateway declaration naming any other token would preload a command
// that starts, falls straight through kong's ordinary verb parser as an
// unknown command, and never reaches runSessionMCP at all — exactly the
// bug this alias exists to make unrepeatable.
const mcpSessionSubcommand = hiddenSessionMCPVerb

// validateRunRoster runs E3.3's roster validation over the environment
// this run actually selected (not merely the configured default), so a
// roster reference that names no defined model refuses BEFORE anything is
// created, naming the source file and key.
func validateRunRoster(cfg *config.Config, sel launch.EnvSelection, shipped []string) error {
	if !sel.Selected() || sel.Sidecar == nil {
		return nil
	}
	facts := models.EnvironmentRosterFacts{
		Name:        sel.Name,
		Root:        sel.Root,
		Exclusive:   sel.Sidecar.Models.Exclusive,
		Roster:      launch.RosterInputFor(sel.Sidecar, shipped),
		LocalModels: map[string]string{},
	}
	for _, m := range sel.Sidecar.Inference.Models {
		facts.LocalModels[m.ID] = m.Backend
	}
	return models.ValidateRoster(cfg, facts)
}

// configDirOrEmpty is the launcher config dir the creation HMAC key record
// lives in. An unresolvable one degrades to "": the ATTACH resolver then
// reports the key as missing, which is the reset-invalidated state, and the
// CREATE resolver refuses outright — fail closed either way, never a
// silently unkeyed fingerprint.
func configDirOrEmpty() string {
	return filepath.Dir(config.Path())
}

// currentCreationFingerprint is the ATTACH half of §10.2's third condition:
// recompute the creation fingerprint from the environment as it is NOW,
// under the stored launcher key, for comparison against the one recorded at
// create. reset is true when that key is gone (post-`pix reset`), which
// attributes as exactly ONE drift record.
func currentCreationFingerprint(cfg *config.Config, o launch.RunOpts, sel launch.EnvSelection, version string) (sandbox.Fingerprint, bool, error) {
	in, err := runEffectiveInput(cfg, o, sel, version)
	if err != nil {
		return nil, false, err
	}
	return launch.CreationFingerprint(launch.CreationFactsFor(in), launch.AttachHMACResolver(configDirOrEmpty(), nil))
}

// envHolderProbe and envSandboxState were removed: both supported an env
// "forget" verb and a live-sandbox-state column on `pix env show`, and
// neither is part of the accepted v2 four-verb surface (list/show/
// default/trust, docs/design/pix-v2-surface.md §3.4 — there is no
// unregister verb at all). Both were already unreachable dead code (no call
// site) before this comment; they named nativeenv.HolderProbe, a v1
// registry-era type that has itself been deleted along with forget.go.
