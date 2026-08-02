// Round-4 review fixes (packs-v2 Phase 1):
//
//	F1 — lock-write failure ABORTS before cfg.Save (config is never committed
//	     without its attribution), in both `pack use` and the active-pack
//	     `pack add mcp` path.
//	F2 — kit synthesis fails CLOSED at the launch boundary: a pack declaring a
//	     sandbox proxy whose kit can't be built refuses the launch instead of
//	     creating a sandbox missing the declared wrapper.
//	F3 — definitelyCreating no longer counts --replace on an INCONCLUSIVE
//	     probe (sbxUnknown) as a definite create (covered by the updated table
//	     in TestSandboxPackMarker_NotOverwrittenOnInconclusiveProbe).
package pack

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"pix/host/config"
)

// brokenPackLock makes writePackLock(root, ...) fail deterministically: the
// destination pack.lock is a non-empty DIRECTORY, so the atomic tmp+rename in
// writePackLock fails (rename onto a directory), while everything else in the
// pack root (pack.toml, bin/) stays perfectly readable/writable.
func brokenPackLock(t *testing.T, root string) {
	t.Helper()
	lockDir := PackLockPath(root)
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lockDir, "occupied"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- F1: abort-on-lock-failure, unit level -------------------------------------

// TestCommitPackActivation_LockFailureAbortsBeforeSave: when the lock can't be
// written, commitPackActivation returns an error WITHOUT calling cfg.Save —
// the on-disk config is untouched (here: never even created).
func TestCommitPackActivation_LockFailureAbortsBeforeSave(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	t.Setenv("PIX_CONFIG", cfgPath)

	root := filepath.Join(dir, "pack")
	mustWritePack(t, root, Manifest{Name: "p", Schema: 1})
	brokenPackLock(t, root)

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.AddMCP("fastmail")
	cfg.Pack = root

	store, serr := loadPackTrustStore()
	if serr != nil {
		t.Fatal(serr)
	}
	if err := commitPackActivation(cfg, store, root, packLock{MCP: []string{"fastmail"}}); err == nil {
		t.Fatal("expected an error when the lock cannot be written")
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Errorf("F1: cfg.Save must not run after a lock-write failure; config file exists (stat err=%v)", err)
	}

	// Sanity: with a writable lock the same commit succeeds and saves.
	if err := os.RemoveAll(PackLockPath(root)); err != nil {
		t.Fatal(err)
	}
	if err := commitPackActivation(cfg, store, root, packLock{MCP: []string{"fastmail"}}); err != nil {
		t.Fatalf("commit with a writable lock should succeed: %v", err)
	}
	if readPackLock(root).MCP[0] != "fastmail" {
		t.Error("lock not written on the success path")
	}
	saved, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(saved.MCP, "fastmail") {
		t.Error("config not saved on the success path")
	}
}

// --- F1: abort-on-lock-failure, end-to-end through RunPackUse ------------------

// TestPackUse_LockWriteFailureAbortsWithoutCommit re-executes the test binary
// (RunPackUse calls os.Exit on this path) and asserts a forced writePackLock
// failure leaves the config UNCOMMITTED: no MCP added, pack not switched — the
// config file is never written at all.
func TestPackUse_LockWriteFailureAbortsWithoutCommit(t *testing.T) {
	if os.Getenv("PIX_TEST_LOCKFAIL") == "use" {
		// Child: this os.Exits(1) at the commit point if the fix holds. --yes
		// accepts the Phase-2 Tier-1 gate (the pack declares an mcp) so the
		// child reaches the commit point instead of failing closed at the gate.
		RunPackUse(fakeGitEnv(nil), os.Stdout, []string{os.Getenv("PIX_TEST_PACK_ROOT"), "--yes"}, registerOK)
		return // reaching here (exit 0) means RunPackUse did NOT abort
	}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	root := filepath.Join(dir, "pack")
	mustWritePack(t, root, Manifest{Name: "work", Schema: 1, Integrations: []Integration{
		{Name: "fastmail", MCP: "fastmail"},
	}})
	brokenPackLock(t, root)

	cmd := exec.Command(os.Args[0], "-test.run", "^TestPackUse_LockWriteFailureAbortsWithoutCommit$")
	cmd.Env = append(os.Environ(),
		"PIX_TEST_LOCKFAIL=use",
		"PIX_TEST_PACK_ROOT="+root,
		"PIX_CONFIG="+cfgPath,
		"XDG_STATE_HOME="+filepath.Join(dir, "state"),
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected `pack use` to exit non-zero on a lock-write failure; output:\n%s", out)
	}
	if !strings.Contains(string(out), "aborting without saving config") {
		t.Errorf("expected the abort message, got:\n%s", out)
	}
	// Nothing committed: the config file must not exist (Save never ran), so
	// the pack was not switched and no MCP was added.
	if _, serr := os.Stat(cfgPath); !os.IsNotExist(serr) {
		b, _ := os.ReadFile(cfgPath)
		t.Errorf("F1: config must stay uncommitted after a lock failure; found:\n%s", b)
	}
}
