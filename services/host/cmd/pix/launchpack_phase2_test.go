// Moved from pack/pack_v2_phase2_test.go: the subject is applying a pack to a LAUNCH
// (runOpts, applyPackToLaunch, writePackContextFiles), which lives in
// launchpack.go on this side of the boundary.
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/workflow/pack"
)

// TestRefreshHostPackWrappers_Tier0AndMissingPackNoOp: no active pack, an
// absent pack dir, and a Tier-0 pack are all clean no-ops (no error, nothing
// installed) — the runHostSetup/runHostLaunch wiring must never trip on them.
func TestRefreshHostPackWrappers_Tier0AndMissingPackNoOp(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	var out bytes.Buffer
	if p, err := pack.RefreshHostPackWrappers(&out, &config.Config{}, true); err != nil || p != nil {
		t.Errorf("no active pack: want (nil,nil), got (%v,%v)", p, err)
	}
	if p, err := pack.RefreshHostPackWrappers(&out, &config.Config{Pack: filepath.Join(dir, "gone")}, true); err != nil || p != nil {
		t.Errorf("absent pack must degrade (pack.ErrNotAPack), got (%v,%v)", p, err)
	}
	root := filepath.Join(dir, "tier0")
	mustWritePack(t, root, pack.Manifest{Name: "tier0", Schema: 1})
	p, err := pack.RefreshHostPackWrappers(&out, &config.Config{Pack: root}, true)
	if err != nil || p == nil {
		t.Errorf("Tier-0 pack: want the loaded pack and no error, got (%v,%v)", p, err)
	}
	if entries, _ := os.ReadDir(pack.HostPackBinDir()); len(entries) != 0 {
		t.Errorf("nothing may be installed for a Tier-0 pack, found %v", entries)
	}
}

// TestHostPackBinDir_OnHostPathOnly (fitness #1): hostChildEnv prepends the
// pack host-bin dir to PATH for the `pix host` child ONLY. The sandbox
// side is pinned separately: TestSynthesizePackKit_SandboxOnly proves a
// host=true wrapper never enters the sandbox kit, and nothing in
// buildSbxArgs/applyPackToLaunch references pack.HostPackBinDir.

// TestHostPackBinDir_OnHostPathOnly (fitness #1): hostChildEnv prepends the
// pack host-bin dir to PATH for the `pix host` child ONLY. The sandbox
// side is pinned separately: TestSynthesizePackKit_SandboxOnly proves a
// host=true wrapper never enters the sandbox kit, and nothing in
// buildSbxArgs/applyPackToLaunch references pack.HostPackBinDir.
func TestHostPackBinDir_OnHostPathOnly(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	env := hostChildEnv("/sa", "")
	wantPrefix := "PATH=" + pack.HostPackBinDir() + string(os.PathListSeparator)
	found := false
	for _, kv := range env {
		if strings.HasPrefix(kv, wantPrefix) {
			found = true
		}
	}
	if !found {
		t.Errorf("hostChildEnv must prepend %s to PATH, got %v", pack.HostPackBinDir(), env)
	}
	// The sandbox launch path must not touch the host bin dir at all: applying
	// a pack with a HOST wrapper to a sandbox launch installs nothing there.
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	root := phase2HostPack(t, dir, "work", "platformio")
	cfg := &config.Config{Pack: root}
	o := runOpts{}
	if _, err := applyPackToLaunch(cfg, &o, fakeGitEnv(nil)); err != nil {
		t.Fatalf("applyPackToLaunch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(pack.HostPackBinDir(), "platformio")); err == nil {
		t.Error("a sandbox launch must never install host wrappers")
	}
	if len(o.PackKits) != 0 {
		t.Errorf("a host-only pack must synthesize no sandbox kit, got %v", o.PackKits)
	}
}

// TestVerifyPackBinSHA_Contract: empty sha, missing file, mismatch all refuse;
// a correct (case-insensitive) sha passes — mirroring verifyPluginSHA.
