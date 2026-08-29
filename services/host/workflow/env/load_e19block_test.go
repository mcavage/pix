package env

// load_e19block_test.go closes E1.9's post-ship BLOCK finding: Load,
// Review and ComputeShow used to take a bare, ambiguous `workspaces
// []string` (fed to RefuseContainment AND envinfo.ValidateSkillWorkspaces)
// alongside a SEPARATE, independently-typed `effective EffectiveMounts`
// (fed only to ComputeBoM). The two lists could diverge — a caller free to
// hand Load one set and ComputeBoM another meant a mount that should have
// refused containment could reach the reviewed bill having never been
// checked at all. There is now exactly ONE typed effective workspace set
// (EffectiveMounts, {Path, ReadOnly}) flowing end-to-end through
// Load/Review/ComputeShow (load.go/review.go's own doc comments):
//
//  1. Load validates sidecar skills against every resolved workspace path,
//     INCLUDING an implicit environment-root read-only source workspace no
//     caller has to supply — a LOCAL skill living right under the
//     environment's own directory must resolve, from any cwd, whether or
//     not any writable mount was ever declared.
//  2. A skill that escapes every resolved workspace — root included —
//     still fails closed.
//  3. Root-containment refusal (AC-11) applies ONLY to writable entries: a
//     writable mount containing the root refuses (both paths named,
//     preserving the existing Tier1/AC-11 semantics for genuine
//     expansions); the identical path declared read-only never
//     self-refuses, and Load's own intrinsic root addition — always
//     read-only — can never self-refuse either.
//  4. Review/ComputeShow expose no second, independently-suppliable
//     workspace parameter that could bypass containment: there is exactly
//     one EffectiveMounts argument, fed identically to Load and ComputeBoM.

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"pix/host/cli"
	"pix/host/envinfo"
)

// ── 1. a local sidecar skill under root succeeds, from >= 3 distinct cwds,
//      with NO caller-supplied workspaces at all ──────────────────────────

func TestLoad_LocalSidecarSkillUnderRootSucceedsAcrossCwds(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeEnvFile(t, root, ".sbxenv.yaml", minimalSbxenv)
	writeEnvFile(t, root, "pix.toml", "schema = 1\n\n[pi]\nskills = [\"./skills\"]\n")
	if _, err := Register(cfg, "home", root); err != nil {
		t.Fatal(err)
	}

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWD) })

	cwds := []string{t.TempDir(), t.TempDir(), t.TempDir()}
	if len(cwds) < 3 {
		t.Fatalf("test setup error: need >= 3 cwds, got %d", len(cwds))
	}
	for _, cwd := range cwds {
		t.Run(cwd, func(t *testing.T) {
			if err := os.Chdir(cwd); err != nil {
				t.Fatal(err)
			}
			// nil: no caller-supplied EffectiveMounts at all — Load must
			// still validate the local skill against its own implicit
			// root workspace.
			got, err := Load(cfg, nil, "home", nil, nil)
			if err != nil {
				t.Fatalf("Load from cwd %s: %v, want success (local skill under root)", cwd, err)
			}
			if got.Sidecar == nil || len(got.Sidecar.Pi.Skills) != 1 {
				t.Fatalf("Sidecar = %+v, want exactly one declared skill", got.Sidecar)
			}
		})
	}
}

// ── 2. a skill that escapes every resolved workspace still fails closed ──

func TestLoad_LocalSidecarSkillEscapingRootFails(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)

	parent := t.TempDir()
	root := filepath.Join(parent, "envroot")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	escaped := filepath.Join(parent, "escaped-skills")
	if err := os.MkdirAll(escaped, 0o755); err != nil {
		t.Fatal(err)
	}
	writeEnvFile(t, root, ".sbxenv.yaml", minimalSbxenv)
	writeEnvFile(t, root, "pix.toml", "schema = 1\n\n[pi]\nskills = [\"../escaped-skills\"]\n")
	if _, err := Register(cfg, "home", root); err != nil {
		t.Fatal(err)
	}

	_, err := Load(cfg, nil, "home", nil, nil)
	if err == nil {
		t.Fatal("Load must refuse a sidecar skill that resolves outside every workspace, including root")
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("cli.ExitCode(err) = %d, want 2", got)
	}
	var sidecarErr *envinfo.Error
	if !errors.As(err, &sidecarErr) {
		t.Fatalf("error = %#v, want *envinfo.Error", err)
	}
	if sidecarErr.Key != "pi.skills" {
		t.Errorf("Key = %q, want pi.skills", sidecarErr.Key)
	}

	// A caller-supplied EffectiveMounts covering only SOME other, unrelated
	// directory must not rescue the escaping skill either — the implicit
	// root addition never widens the set beyond what was actually declared.
	other := t.TempDir()
	_, err = Load(cfg, nil, "home", EffectiveMounts{{Path: other}}, nil)
	if err == nil {
		t.Fatal("Load must still refuse the escaping skill with an unrelated extra workspace declared")
	}
}

// ── 3a. a writable mount containing root refuses with both paths (AC-11
//        semantics preserved for genuine expansions) ─────────────────────

func TestLoad_WritableMountContainingRootRefusesWithBothPaths(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)

	workspace := t.TempDir()
	root := filepath.Join(workspace, "env")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeEnvFile(t, root, ".sbxenv.yaml", minimalSbxenv)
	if _, err := Register(cfg, "home", root); err != nil {
		t.Fatal(err)
	}

	_, err := Load(cfg, nil, "home", EffectiveMounts{{Path: workspace, ReadOnly: false}}, nil)
	if err == nil {
		t.Fatal("Load must refuse a WRITABLE mount that contains the environment root")
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("cli.ExitCode(err) = %d, want 2", got)
	}
	var containment *ContainmentError
	if !errors.As(err, &containment) {
		t.Fatalf("error = %#v, want *ContainmentError", err)
	}
	if !containsAll(err.Error(), root, workspace) {
		t.Errorf("refusal text %q must name both the root %q and the writable workspace %q", err.Error(), root, workspace)
	}
}

// ── 3b. the IDENTICAL path, declared read-only, never self-refuses ───────

func TestLoad_ReadOnlyMountContainingRootDoesNotRefuse(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)

	workspace := t.TempDir()
	root := filepath.Join(workspace, "env")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeEnvFile(t, root, ".sbxenv.yaml", minimalSbxenv)
	if _, err := Register(cfg, "home", root); err != nil {
		t.Fatal(err)
	}

	_, err := Load(cfg, nil, "home", EffectiveMounts{{Path: workspace, ReadOnly: true}}, nil)
	if err != nil {
		t.Fatalf("Load with the same containing mount declared READ-ONLY = %v, want nil (restriction 4 is a writable-only rule)", err)
	}
}

// ── 3c. a caller-supplied read-only mount whose Path IS the root itself
//        never self-refuses ──────────────────────────────────────────────

func TestLoad_ReadOnlyRootMountDoesNotSelfRefuse(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)

	root := t.TempDir()
	writeEnvFile(t, root, ".sbxenv.yaml", minimalSbxenv)
	canon, err := Register(cfg, "home", root)
	if err != nil {
		t.Fatal(err)
	}

	// A caller (or a future E2 composition) that happens to pass the
	// environment's own root back as a read-only EffectiveMounts entry —
	// exactly the shape Load's OWN intrinsic addition takes — must not
	// trip containment merely because Path == root.
	_, err = Load(cfg, nil, "home", EffectiveMounts{{Path: canon, ReadOnly: true}}, nil)
	if err != nil {
		t.Fatalf("Load with a read-only mount equal to root = %v, want nil (read-only root must never self-refuse)", err)
	}
}

// ── 4. Review/ComputeShow expose no second workspaces parameter that
//       could bypass containment ─────────────────────────────────────────

// TestReview_NoIndependentWorkspacesParameterBypassesContainment proves
// there is exactly ONE effective-workspace argument on Review: a writable
// mount containing the root refuses through the SAME single parameter
// ComputeBoM also reads, with no second list a caller could leave empty to
// dodge the refusal while still declaring the mount for the bill (the
// exact shape of E1.9's BLOCK finding).
func TestReview_NoIndependentWorkspacesParameterBypassesContainment(t *testing.T) {
	tempConfigAndState(t)
	cfg := loadConfig(t)

	workspace := t.TempDir()
	root := filepath.Join(workspace, "env")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeEnvFile(t, root, ".sbxenv.yaml", minimalSbxenv)
	if _, err := Register(cfg, "home", root); err != nil {
		t.Fatal(err)
	}

	res, err := Review(cfg, "home", EffectiveMounts{{Path: workspace}}, nil, ReviewOptions{TTY: false, Yes: false})
	if err == nil {
		t.Fatal("Review must refuse a writable mount containing the root — the same containment check Load applies")
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("cli.ExitCode(err) = %d, want 2", got)
	}
	if res != nil {
		t.Errorf("result = %+v, want nil", res)
	}
	var containment *ContainmentError
	if !errors.As(err, &containment) {
		t.Fatalf("error = %#v, want *ContainmentError (proves Review's single EffectiveMounts argument reaches containment, not just the bill)", err)
	}
}

// ── show/review full dispatch with sidecar skills works ──────────────────

// TestComputeShow_WithLocalSidecarSkillSucceeds proves `env show`'s own
// Load call — which supplies no caller EffectiveMounts at all — still
// succeeds for an environment declaring a local sidecar skill under its
// own root.
func TestComputeShow_WithLocalSidecarSkillSucceeds(t *testing.T) {
	tempConfigAndState(t)
	cfg := loadConfig(t)

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeEnvFile(t, root, ".sbxenv.yaml", minimalSbxenv)
	writeEnvFile(t, root, "pix.toml", "schema = 1\n\n[pi]\nskills = [\"./skills\"]\n")
	if _, err := Register(cfg, "work", root); err != nil {
		t.Fatal(err)
	}

	r, err := ComputeShow(cfg, "work")
	if err != nil {
		t.Fatalf("ComputeShow with a local sidecar skill under root = %v, want success", err)
	}
	if !r.Selected || !r.SidecarPresent {
		t.Errorf("ShowResult = %+v, want Selected and SidecarPresent", r)
	}
}

// TestReview_WithLocalSidecarSkillSucceeds proves `env review`'s own Load
// call succeeds the same way, then completes the full Tier0 review with no
// gate (this fixture declares no host-exec facet).
func TestReview_WithLocalSidecarSkillSucceeds(t *testing.T) {
	tempConfigAndState(t)
	cfg := loadConfig(t)

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeEnvFile(t, root, ".sbxenv.yaml", minimalSbxenv)
	writeEnvFile(t, root, "pix.toml", "schema = 1\n\n[pi]\nskills = [\"./skills\"]\n")
	if _, err := Register(cfg, "work", root); err != nil {
		t.Fatal(err)
	}

	res, err := Review(cfg, "work", nil, nil, ReviewOptions{TTY: false, Yes: false})
	if err != nil {
		t.Fatalf("Review with a local sidecar skill under root = %v, want success (Tier0, no gate)", err)
	}
	if !res.Accepted {
		t.Errorf("result = %+v, want Accepted (Tier0 needs no consent)", res)
	}
}
