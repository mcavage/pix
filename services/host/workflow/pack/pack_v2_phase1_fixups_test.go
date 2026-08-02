// Phase-1 failure-path consistency fixes (packs-v2):
//
//	FIX A — commitPackActivation ROLLS the lock BACK on an ordinary cfg.Save
//	        failure: the prior pack.lock bytes are snapshotted before the new
//	        lock is written and restored atomically when Save errors, so lock
//	        and config never diverge on a plain error (read-only config dir,
//	        disk full). Only a true hard kill between the two atomic renames
//	        can still over-retain.
//	FIX B — applyPackToLaunch fails CLOSED when the CONFIGURED active pack
//	        (cfg.Pack) EXISTS but won't load (symlink rejection, validation or
//	        parse error): the launch is refused instead of silently creating a
//	        sandbox missing the pack's declared context. Only a genuinely
//	        ABSENT pack (deleted dir / no pack.toml) warns and proceeds.
package pack

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"pix/host/config"
)

// --- FIX A: Save failure restores the prior lock -------------------------------

// TestCommitPackActivation_SaveFailureRestoresPriorLock: a same-pack
// reactivation that DROPS an MCP, with the commit forced to fail (read-only
// config dir — under the round-2 A model this now trips the HOST-STATE
// activation write, which shares the config dir and aborts BEFORE cfg.Save),
// must (1) restore the prior pack.lock byte-for-byte, (2) leave the on-disk
// config unchanged, (3) leave the on-disk PRIOR activation record intact, and
// (4) let a subsequent successful `pack rm` remove everything cleanly — no
// orphaned, unattributed MCP.
func TestCommitPackActivation_SaveFailureRestoresPriorLock(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a read-only dir cannot force cfg.Save to fail")
	}
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "cfg")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(cfgDir, "config.toml")
	t.Setenv("PIX_CONFIG", cfgPath)

	root := filepath.Join(dir, "pack")
	mustWritePack(t, root, Manifest{Name: "work", Schema: 1})

	// Prior activation state: the config carries the pack's MCP contribution
	// and the lock attributes it.
	cfgBefore := "pack = \"" + root + "\"\nmcp = [\"fastmail\"]\n"
	if err := os.WriteFile(cfgPath, []byte(cfgBefore), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writePackLock(root, packLock{MCP: []string{"fastmail"}}); err != nil {
		t.Fatal(err)
	}
	// Round-2 A: the AUTHORITATIVE attribution is the host-state activation
	// record — seed it exactly as the prior activation's commit would have.
	priorStore, err := loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	priorStore.setActivation(root, packLock{MCP: []string{"fastmail"}})
	if err := priorStore.Save(); err != nil {
		t.Fatal(err)
	}
	lockBefore, err := os.ReadFile(PackLockPath(root))
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	// Same-pack reactivation whose manifest no longer declares the MCP: the
	// contribution is dropped from cfg and the NEW lock is empty.
	if !cfg.RemoveMCP("fastmail") {
		t.Fatal("test setup: fastmail should have been in cfg.MCP")
	}

	// Force cfg.Save to fail: writeFileAtomic's CreateTemp needs a writable
	// config dir.
	if err := os.Chmod(cfgDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cfgDir, 0o755) })

	store, err := loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	cerr := commitPackActivation(cfg, store, root, packLock{})
	if cerr == nil {
		t.Fatal("expected commitPackActivation to fail when the commit cannot write")
	}
	if !strings.Contains(cerr.Error(), "nothing was committed") {
		t.Errorf("error should say nothing was committed, got: %v", cerr)
	}

	// (1) The prior lock is back, byte-for-byte — NOT the new empty lock.
	lockAfter, err := os.ReadFile(PackLockPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(lockAfter, lockBefore) {
		t.Errorf("FIX A: prior pack.lock must be restored on a Save failure.\nbefore: %q\nafter:  %q", lockBefore, lockAfter)
	}
	// (2) The on-disk config is untouched.
	cfgAfter, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(cfgAfter) != cfgBefore {
		t.Errorf("on-disk config must be unchanged after a Save failure.\nbefore: %q\nafter:  %q", cfgBefore, cfgAfter)
	}

	// (3) The on-disk PRIOR activation record is intact (the failed commit
	// never overwrote it).
	if err := os.Chmod(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	afterStore, err := loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	if got := afterStore.activationFor(root); len(got.MCP) != 1 || got.MCP[0] != "fastmail" {
		t.Errorf("prior activation record must survive a failed commit, got %+v", got)
	}

	// (4) Config writable again: `pack rm` removes everything cleanly — the
	// intact activation record still attributes fastmail, so nothing is
	// orphaned.
	var out bytes.Buffer
	RunPackRm(&out, nil)
	saved, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(saved.MCP, "fastmail") {
		t.Errorf("FIX A: pack rm must remove the lock-attributed MCP; config still has it:\n%s", out.String())
	}
	if saved.Pack != "" {
		t.Errorf("pack rm must detach the active pack, still: %q", saved.Pack)
	}
}

// TestCommitPackActivation_SaveFailureRemovesFirstLock: when there was NO
// prior lock (first activation), a Save failure must remove the just-written
// lock — an over-claiming lock beside an uncommitted config is the exact
// divergence FIX A closes.
func TestCommitPackActivation_SaveFailureRemovesFirstLock(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a read-only dir cannot force cfg.Save to fail")
	}
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "cfg")
	if err := os.MkdirAll(cfgDir, 0o555); err != nil { // read-only from the start
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cfgDir, 0o755) })
	t.Setenv("PIX_CONFIG", filepath.Join(cfgDir, "config.toml"))

	root := filepath.Join(dir, "pack")
	mustWritePack(t, root, Manifest{Name: "work", Schema: 1})

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.AddMCP("fastmail")
	cfg.Pack = root

	if cerr := commitPackActivation(cfg, &PackTrustStore{}, root, packLock{MCP: []string{"fastmail"}}); cerr == nil {
		t.Fatal("expected commitPackActivation to fail when cfg.Save cannot write")
	}
	if _, serr := os.Stat(PackLockPath(root)); !os.IsNotExist(serr) {
		t.Errorf("FIX A: with no prior lock, a Save failure must remove the new lock (stat err=%v)", serr)
	}
}

// --- FIX B: broken active pack fails the launch closed -------------------------
