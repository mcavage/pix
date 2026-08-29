// run_env.go — L4's half of E2.5: turning `--env NAME` (or the configured
// default) into the ONE resolved environment a launch composes its real
// RuntimeFacts from, and the two lookups only this layer can do (the
// config, the shipped agent roster). The composition itself, the render,
// the fingerprint and the persisted bytes all live in workflow/launch —
// this file never renders an effective document of its own.
package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"pix/host/config"
	"pix/host/envinfo"
	"pix/host/workflow/launch"
	"pix/host/workflow/models"

	nativeenv "pix/host/workflow/env"
)

// resolveRunEnvironment resolves §6.1's selection order — explicit `--env`,
// else the machine default, else `none` — with ONE hard rule: an explicit
// name is EXACT. An unknown one returns the registry's own unknown-
// environment refusal (known names, closest match, how to register) and
// the caller exits non-zero having created nothing; it NEVER degrades to
// the configured default, because a typo silently launching the wrong
// credential set is the worst outcome this feature can produce.
//
// Nothing here writes config: `--env` selects for this run only (AC-22).
func resolveRunEnvironment(explicit string) (launch.EnvSelection, error) {
	cfg, err := config.Load()
	if err != nil {
		return launch.EnvSelection{}, err
	}
	name := strings.TrimSpace(explicit)
	if name == "" {
		name = strings.TrimSpace(cfg.Environment)
	}
	if name == "" {
		// D17's `none`: no environment registered or selected. The built-in
		// effective document still renders; a launch is never blocked by the
		// absence of an environment.
		return launch.EnvSelection{}, nil
	}
	loaded, err := nativeenv.LoadForLaunch(cfg, name)
	if err != nil {
		return launch.EnvSelection{}, err
	}
	return launch.EnvSelection{
		Name:     loaded.Name,
		Root:     loaded.Root,
		Document: loaded.Document,
		Sidecar:  loaded.Sidecar,
		Tree:     loaded.Tree,
		Reviewed: loaded.Accepted,
	}, nil
}

// runEffectiveInput is the launch's real RuntimeFacts input: the ACTUAL
// pix-* sandbox name (already resolved), the pinned image plus §6.2's
// `pullPolicy: missing`, the primary workspace in object form, the
// unconditional personal-context workspace, the generated mixin kit, and
// `--dev`'s checkout kit with its live skill arguments. Every value is one
// this launch already decided; nothing is re-derived here.
func runEffectiveInput(cfg *config.Config, o launch.RunOpts, sel launch.EnvSelection) launch.EffectiveInput {
	template := o.Template
	if template == "" {
		if o.LocalImageTag != "" {
			template = launch.DockerImageRepo + ":" + o.LocalImageTag
		} else {
			template = launch.DockerImageRepo
		}
	}
	primary := o.Workspace
	if sel.Root != "" && primary == "" {
		primary = sel.Root
	}
	in := launch.EffectiveInput{
		Selection:        sel,
		SandboxName:      o.Name,
		Template:         template,
		PullPolicy:       launch.EffectivePullPolicy,
		PrimaryWorkspace: envinfo.WorkspaceFact{Path: primary},
		PersonalContext:  envinfo.WorkspaceFact{Path: config.ContextDir()},
		PixEnvVars:       map[string]string{},
	}
	if len(o.PackKits) > 0 {
		// The generated Pi mixin kit is the LAST kit this launch
		// materialized (inference first, then personal context); both are
		// generated mixins, and RenderEffective renders the reference only.
		in.MixinKit = o.PackKits[0]
	}
	if o.Dev {
		in.DevKit = envinfo.DevKitFact{Kit: o.LocalKit, LiveSkills: launch.LiveSkillDirs(cfg, o)}
	}
	return in
}

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
// lives in. An unresolvable one degrades to "": the resolver then reports
// the key as missing, which is the reset-invalidated state — fail closed,
// never a silently unkeyed fingerprint.
func configDirOrEmpty() string {
	return filepath.Dir(config.Path())
}

// envHolderProbe is `pix env forget`'s REAL live-holder check (C7): a
// registered name resolves to its canonical root, and workflow/launch
// answers from the sandboxes this host actually recorded against that
// root, failing closed on any sbx state it cannot read.
func envHolderProbe(cfg *config.Config) nativeenv.HolderProbe {
	probe := launch.EnvironmentHolderProbe(defaultShellEnv(), func(name string) (string, bool) {
		if cfg == nil {
			return "", false
		}
		root, ok := cfg.Environments[name]
		return root, ok
	})
	return nativeenv.HolderProbe(probe)
}

// envSandboxState is `pix env show`'s REAL live state for the selected
// environment, replacing Wave C's placeholder: the live holders this host
// recorded against that root, or an explicit unreadable/not-running
// answer. It never fabricates a state.
func envSandboxState(cfg *config.Config, root string) string {
	if strings.TrimSpace(root) == "" {
		return "not running"
	}
	held, err := launch.EnvironmentHolders(defaultShellEnv(), root)
	if err != nil {
		return "unknown (could not read sbx state)"
	}
	if len(held) == 0 {
		return "not running"
	}
	return fmt.Sprintf("running: %s", strings.Join(held, ", "))
}
