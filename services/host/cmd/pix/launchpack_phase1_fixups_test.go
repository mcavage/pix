// Moved from pack/pack_v2_phase1_fixups_test.go: the subject is applying a pack to a LAUNCH
// (runOpts, applyPackToLaunch, writePackContextFiles), which lives in
// launchpack.go on this side of the boundary.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/workflow/pack"
)

// TestApplyPackToLaunch_BrokenActivePackFailsClosed: an ACTIVE pack (cfg.Pack,
// no explicit --pack) whose bin/ contains a symlink is rejected by pack.LoadPack —
// the launch must REFUSE instead of proceeding without the pack's declared
// wrappers. A cfg.Pack pointing at a deleted dir (or a dir with no pack.toml)
// is "genuinely absent" and proceeds. An explicit --pack keeps failing closed
// in every error class.
func TestApplyPackToLaunch_BrokenActivePackFailsClosed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	// 1) Tampered active pack: a symlink injected into bin/ -> launch refused.
	tampered := filepath.Join(dir, "tampered")
	mustWritePack(t, tampered, pack.Manifest{Name: "t", Schema: 1, Proxies: []pack.PackProxy{{Name: "warehouse"}}})
	if err := os.MkdirAll(filepath.Join(tampered, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	evil := filepath.Join(dir, "evil")
	if err := os.WriteFile(evil, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(evil, filepath.Join(tampered, "bin", "warehouse")); err != nil {
		t.Fatal(err)
	}
	cfg.Pack = tampered
	o := runOpts{}
	_, lerr := applyPackToLaunch(cfg, &o, fakeGitEnv(nil))
	if lerr == nil {
		t.Fatal("FIX B: a broken/tampered ACTIVE pack must refuse the launch, not proceed without its context")
	}
	if !strings.Contains(lerr.Error(), "refusing to launch") {
		t.Errorf("expected a refusal message, got: %v", lerr)
	}
	if len(o.Skills) != 0 || len(o.PackKits) != 0 {
		t.Errorf("nothing may be mounted on a refused launch, got skills=%v kits=%v", o.Skills, o.PackKits)
	}

	// 2) Genuinely absent active pack (deleted dir): warn + proceed.
	cfg.Pack = filepath.Join(dir, "gone")
	o = runOpts{}
	if _, lerr := applyPackToLaunch(cfg, &o, fakeGitEnv(nil)); lerr != nil {
		t.Fatalf("a deleted active-pack dir must degrade to no-pack, got: %v", lerr)
	}
	if len(o.Skills) != 0 || len(o.PackKits) != 0 {
		t.Errorf("an absent pack must mount nothing, got skills=%v kits=%v", o.Skills, o.PackKits)
	}

	// 2b) Dir exists but has no pack.toml: same "absent" class, proceeds.
	notAPack := filepath.Join(dir, "not-a-pack")
	if err := os.MkdirAll(notAPack, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg.Pack = notAPack
	o = runOpts{}
	if _, lerr := applyPackToLaunch(cfg, &o, fakeGitEnv(nil)); lerr != nil {
		t.Fatalf("a dir without pack.toml must degrade to no-pack, got: %v", lerr)
	}

	// 3) Explicit --pack keeps failing closed, even for the absent class.
	cfg.Pack = ""
	o = runOpts{Pack: filepath.Join(dir, "gone")}
	if _, lerr := applyPackToLaunch(cfg, &o, fakeGitEnv(nil)); lerr == nil {
		t.Fatal("an explicit --pack that does not load must stay fatal")
	}
	o = runOpts{Pack: tampered}
	if _, lerr := applyPackToLaunch(cfg, &o, fakeGitEnv(nil)); lerr == nil {
		t.Fatal("an explicit --pack with a tampered bin/ must stay fatal")
	}
}
