package main

import (
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/envinfo"
	"pix/host/sandbox"
	"pix/host/stack"
	"pix/host/workflow/launch"
)

// version_identity_test.go — Wave D's version-identity wiring, proven from
// the REAL launch composition (runEffectiveInput, SessionFingerprint), not
// from a helper invented for the test. The property under test is one
// sentence: the stamped launcher build and this PIX_HOME's stack id travel
// into every launch as Pix-managed facts, and a VERSION BUMP is the one
// env-shaped drift that may be recreated automatically.

// TestRunEffectiveInput_CarriesPixManagedEnvFacts pins the launch side: the
// composition a real `pix run` hands the effective-document renderer carries
// PIX_LAUNCHER_VERSION (from RunOpts.LauncherVersion, the ONE field the
// binary's stamp reaches a launch through) and PIX_STACK_ID (this PIX_HOME's
// derived stack identity), and nothing else Pix-managed.
func TestRunEffectiveInput_CarriesPixManagedEnvFacts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	wantID, err := stack.ID(home)
	if err != nil {
		t.Fatalf("stack.ID: %v", err)
	}

	o := launch.RunOpts{Workspace: t.TempDir(), Name: "pix-" + wantID + "-w", LauncherVersion: "0.1.72-beta.abc1234"}
	in, err := runEffectiveInput(&config.Config{}, o, launch.EnvSelection{}, o.LauncherVersion)
	if err != nil {
		t.Fatalf("runEffectiveInput: %v", err)
	}
	if got := in.PixEnvVars[envinfo.EnvVarLauncherVersion]; got != o.LauncherVersion {
		t.Errorf("%s = %q, want the stamped launcher version %q", envinfo.EnvVarLauncherVersion, got, o.LauncherVersion)
	}
	if got := in.PixEnvVars[envinfo.EnvVarStackID]; got != wantID {
		t.Errorf("%s = %q, want this PIX_HOME's stack id %q", envinfo.EnvVarStackID, got, wantID)
	}
	if len(in.PixEnvVars) != 2 {
		t.Errorf("Pix-managed env block = %v, want exactly the two Pix-managed facts", in.PixEnvVars)
	}
}

// TestRunEffectiveInput_UnstampedBuildOmitsTheVersionFact: an unstamped
// build states no version rather than an empty one. An empty string is a
// claim ("this build has no version"); omission is the honest shape, and it
// also keeps a pre-Wave-D sandbox's recorded document from reading as drift
// against a rendered `PIX_LAUNCHER_VERSION: ""`.
func TestRunEffectiveInput_UnstampedBuildOmitsTheVersionFact(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	o := launch.RunOpts{Workspace: t.TempDir(), Name: "pix-x", LauncherVersion: "  "}
	in, err := runEffectiveInput(&config.Config{}, o, launch.EnvSelection{}, "")
	if err != nil {
		t.Fatalf("runEffectiveInput: %v", err)
	}
	if _, ok := in.PixEnvVars[envinfo.EnvVarLauncherVersion]; ok {
		t.Errorf("an unstamped build must OMIT %s, got %q", envinfo.EnvVarLauncherVersion, in.PixEnvVars[envinfo.EnvVarLauncherVersion])
	}
}

// TestSessionFingerprint_LauncherVersionDrift is the attach half: the same
// build attaches clean, a bumped build diverges on exactly one key, and an
// unstamped build contributes no key at all (so a fingerprint recorded
// before this component existed does not read as drift).
func TestSessionFingerprint_LauncherVersionDrift(t *testing.T) {
	cfg := &config.Config{}
	base := launch.RunOpts{Name: "pix-x", StaticMCP: []string{"slack"}, Template: "repo:tag", LauncherVersion: "0.1.71"}

	stored := launch.SessionFingerprint(cfg, base)
	if stored["launcher_version"] != "0.1.71" {
		t.Fatalf("launcher_version = %q, want the stamped build", stored["launcher_version"])
	}

	same := base
	if d := sandbox.Diff(stored, launch.SessionFingerprint(cfg, same)); len(d) != 0 {
		t.Errorf("an unchanged build must attach clean, got diverged %v", d)
	}

	bumped := base
	bumped.LauncherVersion = "0.1.72"
	if d := sandbox.Diff(stored, launch.SessionFingerprint(cfg, bumped)); strings.Join(d, ",") != "launcher_version" {
		t.Errorf("a version bump must diverge on exactly launcher_version, got %v", d)
	}

	unstamped := base
	unstamped.LauncherVersion = ""
	if _, ok := launch.SessionFingerprint(cfg, unstamped)["launcher_version"]; ok {
		t.Errorf("an unstamped build must contribute no launcher_version component")
	}
}

// TestPixManagedEnvDrift_IsExactlyRecreationSafe is the classification
// boundary, stated as the code states it: the two Pix-managed keys are
// recreation-safe, and EVERY other env.* key — including one an author
// happened to name PIX_SOMETHING — is substantive and still refuses.
func TestPixManagedEnvDrift_IsExactlyRecreationSafe(t *testing.T) {
	safe := []string{"env." + envinfo.EnvVarLauncherVersion, "env." + envinfo.EnvVarStackID}
	for _, key := range safe {
		if !envinfo.RecreationSafe([]envinfo.Drift{{ComposedKey: key}}) {
			t.Errorf("%s must classify recreation-safe", key)
		}
	}
	if !envinfo.RecreationSafe([]envinfo.Drift{{ComposedKey: safe[0]}, {ComposedKey: "sandboxOptions.template"}}) {
		t.Errorf("a version bump alongside a template pin must stay recreation-safe")
	}
	for _, key := range []string{
		"env.PIX_LAUNCHER_VERSION_EXTRA", // a near-miss must not inherit the exemption
		"env.PIX_ANYTHING_ELSE",          // no PIX_* prefix rule exists
		"env.ANTHROPIC_API_KEY",
		"env.FOO",
	} {
		if envinfo.RecreationSafe([]envinfo.Drift{{ComposedKey: key}}) {
			t.Errorf("%s must classify substantive (an authored env fact is never auto-recreated)", key)
		}
		// ...and one substantive key poisons an otherwise safe set.
		if envinfo.RecreationSafe([]envinfo.Drift{{ComposedKey: safe[0]}, {ComposedKey: key}}) {
			t.Errorf("a set containing %s must classify substantive as a whole", key)
		}
	}
}

// TestVersionBumpRecreatesOnlyWithACompleteProof: classification is not
// authorization. A version-bump drift takes the EXISTING proof-gated
// recreate path — fresh listing, zero holders, no keep, direct workspace —
// and a missing proof refuses with the reason named rather than recreating.
func TestVersionBumpRecreatesOnlyWithACompleteProof(t *testing.T) {
	drifts := []envinfo.Drift{{ComposedKey: "env." + envinfo.EnvVarLauncherVersion}}
	full := launch.RecreateProof{
		FreshListing: true,
		Holders:      launch.KnownHolders(0),
		Workspace:    launch.WorkspaceDirect,
	}
	plan, refusals := launch.PlanSafeRecreate("pix-abc", "inst-1", drifts, full)
	if plan == nil {
		t.Fatalf("a version bump with a complete proof must plan a recreate; refusals=%v", refusals)
	}
	if !strings.Contains(plan.Reason, "env."+envinfo.EnvVarLauncherVersion) {
		t.Errorf("recreate reason %q must name the drifted key", plan.Reason)
	}

	held := full
	held.Holders = launch.KnownHolders(1)
	if plan, refusals := launch.PlanSafeRecreate("pix-abc", "inst-1", drifts, held); plan != nil || len(refusals) == 0 {
		t.Errorf("a live holder must refuse the recreate, got plan=%v refusals=%v", plan, refusals)
	}

	unknownWorkspace := full
	unknownWorkspace.Workspace = launch.WorkspaceUnknown
	if plan, refusals := launch.PlanSafeRecreate("pix-abc", "inst-1", drifts, unknownWorkspace); plan != nil || len(refusals) == 0 {
		t.Errorf("an undetermined workspace must refuse the recreate, got plan=%v refusals=%v", plan, refusals)
	}
}
