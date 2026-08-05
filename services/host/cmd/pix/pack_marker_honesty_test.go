// Marker-honesty fix (packs-v2 Phase 1): the .pix/sandbox.pack marker
// (and the memory-scope context write) must record the pack that ACTUALLY
// loaded and applied, not the merely CONFIGURED cfg.Pack/o.Pack path.
//
// Before this fix, launch.ApplyPackToLaunch returned only an error, so run.go and
// task.go wrote the marker from pack.ActivePackRoot(cfg.Pack, o.Pack) even when
// launch.ApplyPackToLaunch degraded to pack-less via pack.ErrNotAPack (active pack dir
// genuinely missing -> warn + proceed without the pack). The marker then
// recorded a pack that was NOT attached; once the dir came back and the user
// reattached, launch.StalePackReattachWarning compared marker == active and stayed
// silent even though the sandbox never got the pack's create-time facets.
//
// launch.ApplyPackToLaunch now returns (effectiveRoot string, err error):
// effectiveRoot is "" both when there is no active pack AND when it degraded
// via pack.ErrNotAPack, and the real pack root when the pack loaded and applied.
// Callers must write the marker (and scope memory) from that return value.
package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"pix/host/config"
	"pix/host/workflow/launch"
	"pix/host/workflow/pack"
)

// TestApplyPackToLaunch_DegradedMissingPack_EffectiveRootEmpty: a cfg.Pack
// pointing at a dir that does not exist (or has no pack.toml) is the
// pack.ErrNotAPack degrade path — launch.ApplyPackToLaunch must warn-and-proceed (nil
// error) but report "" as the effective root, since nothing was actually
// mounted onto o/cfg.
func TestApplyPackToLaunch_DegradedMissingPack_EffectiveRootEmpty(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{Pack: filepath.Join(dir, "gone")}
	o := launch.RunOpts{}

	root, err := launch.ApplyPackToLaunch(cfg, &o, fakeGitEnv(nil), io.Discard)
	if err != nil {
		t.Fatalf("a genuinely absent active pack must degrade, not fail: %v", err)
	}
	if root != "" {
		t.Errorf("effective root = %q, want \"\" (nothing actually applied)", root)
	}
	if len(o.Skills) != 0 || len(o.PackKits) != 0 {
		t.Errorf("a degraded launch must mount nothing, got skills=%v kits=%v", o.Skills, o.PackKits)
	}
}

// TestApplyPackToLaunch_ValidPack_EffectiveRootIsRealRoot: a valid, loadable
// active pack returns its own root as the effective root.
func TestApplyPackToLaunch_ValidPack_EffectiveRootIsRealRoot(t *testing.T) {
	packRoot := t.TempDir()
	mustWritePack(t, packRoot, pack.Manifest{Name: "work", Schema: 1})

	cfg := &config.Config{Pack: packRoot}
	o := launch.RunOpts{}

	root, err := launch.ApplyPackToLaunch(cfg, &o, fakeGitEnv(nil), io.Discard)
	if err != nil {
		t.Fatalf("a valid active pack must load cleanly: %v", err)
	}
	if root != packRoot {
		t.Errorf("effective root = %q, want the real pack root %q", root, packRoot)
	}
}

// TestSandboxPackMarker_HonestAboutDegradedLaunch is the end-to-end
// regression: it drives the exact sequence run.go/task.go run (call
// launch.ApplyPackToLaunch, then write the marker from its returned effective root,
// never from pack.ActivePackRoot(cfg.Pack, o.Pack) directly) and asserts the
// marker agrees with what actually loaded in both directions.
func TestSandboxPackMarker_HonestAboutDegradedLaunch(t *testing.T) {
	t.Run("degraded pack: marker is removed, not written", func(t *testing.T) {
		dir := t.TempDir()
		ws := t.TempDir()
		// Pre-seed a stale marker to prove degrade REMOVES it rather than
		// leaving a stale/wrong value in place.
		if err := os.MkdirAll(filepath.Join(ws, ".pix"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(launch.SandboxPackMarkerPath(ws), []byte("/some/stale/pack\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		cfg := &config.Config{Pack: filepath.Join(dir, "gone")}
		o := launch.RunOpts{Workspace: ws}

		effectiveRoot, err := launch.ApplyPackToLaunch(cfg, &o, fakeGitEnv(nil), io.Discard)
		if err != nil {
			t.Fatalf("degrade path must not error: %v", err)
		}
		launch.WriteSandboxPackMarker(o.Workspace, effectiveRoot)

		if got := launch.ReadSandboxPackMarker(ws); got != "" {
			t.Errorf("marker after a degraded launch = %q, want \"\" (removed, not the unavailable configured pack)", got)
		}
	})

	t.Run("valid active pack: marker records the real root", func(t *testing.T) {
		packRoot := t.TempDir()
		mustWritePack(t, packRoot, pack.Manifest{Name: "work", Schema: 1})
		ws := t.TempDir()

		cfg := &config.Config{Pack: packRoot}
		o := launch.RunOpts{Workspace: ws}

		effectiveRoot, err := launch.ApplyPackToLaunch(cfg, &o, fakeGitEnv(nil), io.Discard)
		if err != nil {
			t.Fatalf("a valid active pack must load cleanly: %v", err)
		}
		launch.WriteSandboxPackMarker(o.Workspace, effectiveRoot)

		if got := launch.ReadSandboxPackMarker(ws); got != pack.CanonicalizePackRoot(packRoot) {
			t.Errorf("marker = %q, want the real pack root %q", got, pack.CanonicalizePackRoot(packRoot))
		}
	})
}

// TestWritePackContextFiles_AgreesWithDegradedMarker: launch.WritePackContextFiles
// takes the SAME effective-root argument the marker is written from, so a
// degraded launch leaves memory unscoped exactly when the marker is removed —
// the two writes can never disagree about what actually loaded.
func TestWritePackContextFiles_AgreesWithDegradedMarker(t *testing.T) {
	dir := t.TempDir()
	ws := t.TempDir()
	cfg := &config.Config{Pack: filepath.Join(dir, "gone")}
	o := launch.RunOpts{Workspace: ws}

	effectiveRoot, err := launch.ApplyPackToLaunch(cfg, &o, fakeGitEnv(nil), io.Discard)
	if err != nil {
		t.Fatalf("degrade path must not error: %v", err)
	}
	launch.WritePackContextFiles(cfg, o, effectiveRoot, io.Discard)
	launch.WriteSandboxPackMarker(o.Workspace, effectiveRoot)

	if _, err := os.Stat(filepath.Join(ws, ".pix", "profile")); err == nil {
		t.Error("a degraded launch must leave memory unscoped (no profile file)")
	}
	if got := launch.ReadSandboxPackMarker(ws); got != "" {
		t.Errorf("marker = %q, want \"\" to agree with the unscoped memory write", got)
	}
}
