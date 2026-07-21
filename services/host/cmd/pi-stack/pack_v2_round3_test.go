package main

// Round-3 review tests for packs-v2 Phase 1: one (or more) test per finding
// S1/R1/R2/R3 of the third security + correctness review. See the matching
// fix comments in pack.go / run.go / sbxargs.go.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pi-stack/host/config"
)

// --- S1 [CRITICAL]: pack.lock symlink bypass + arbitrary overwrite ------------

// TestWritePackLock_RefusesSymlinkDest: os.WriteFile FOLLOWS a symlink, so a
// pack.lock symlink planted at the destination must be Lstat-refused — never
// written through — and the symlink's target must stay untouched.
func TestWritePackLock_RefusesSymlinkDest(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("precious host file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "pack")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, packLockPath(root)); err != nil {
		t.Fatal(err)
	}

	err := writePackLock(root, packLock{MCP: []string{"evil"}})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("writePackLock through a symlink must refuse, got err=%v", err)
	}
	b, rerr := os.ReadFile(victim)
	if rerr != nil || string(b) != "precious host file\n" {
		t.Errorf("symlink target must be untouched, got %q (err=%v)", b, rerr)
	}
}

// TestWritePackLock_AtomicRoundTripNoDebris: the lock is written via a same-dir
// temp file + rename — an overwrite of an existing lock round-trips correctly
// and leaves no temp file behind (the temp-then-rename is what guarantees an
// interrupted write can never truncate the previous lock in place).
func TestWritePackLock_AtomicRoundTripNoDebris(t *testing.T) {
	root := t.TempDir()
	if err := writePackLock(root, packLock{MCP: []string{"first"}}); err != nil {
		t.Fatal(err)
	}
	if err := writePackLock(root, packLock{MCP: []string{"second"}, Remote: "https://example.com/p.git"}); err != nil {
		t.Fatal(err)
	}
	got := readPackLock(root)
	if !containsStr(got.MCP, "second") || got.Remote != "https://example.com/p.git" {
		t.Errorf("lock round-trip = %+v", got)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("leftover lock temp file: %s", e.Name())
		}
	}
	if fi, err := os.Lstat(packLockPath(root)); err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		t.Errorf("pack.lock must be a regular file, got %v (err=%v)", fi, err)
	}
}

// TestClonePack_ScrubsSymlinkPackLock: the S1 end-to-end. A malicious remote
// pack commits pack.lock as a SYMLINK (-> a host file). clonePack must scrub it
// BEFORE markPackAdopted, so (a) the host file is never written through, (b) a
// fresh REAL lock carrying the adoption marker lands on disk (the clone stays
// ADOPTED), and (c) its private local knowledge ref is therefore still skipped.
func TestClonePack_ScrubsSymlinkPackLock(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("host secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	privateDir := filepath.Join(dir, "private-notes")
	if err := os.MkdirAll(privateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	const url = "https://example.com/attacker/pack.git"
	env := shellEnv{run: func(name string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "clone" {
			dest := args[len(args)-1]
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return "", err
			}
			// The attacker's tree: a manifest declaring a private local ref,
			// plus pack.lock checked in as a symlink at a host file.
			m := packManifest{Name: "evil", Schema: 1, Knowledge: []packKnowledge{
				{Name: "steal", Source: privateDir, Shared: false},
			}}
			if err := writePackManifest(dest, m); err != nil {
				return "", err
			}
			if err := os.Symlink(victim, packLockPath(dest)); err != nil {
				return "", err
			}
		}
		if len(args) >= 3 && args[2] == "rev-parse" {
			return "def456\n", nil
		}
		return "", nil
	}}

	dest, err := clonePack(env, &bytes.Buffer{}, url)
	if err != nil {
		t.Fatalf("clonePack: %v", err)
	}
	// The symlink is GONE — replaced by a real lock file carrying adoption.
	fi, lerr := os.Lstat(packLockPath(dest))
	if lerr != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		t.Fatalf("pack.lock must be a fresh regular file after adoption, got %v (err=%v)", fi, lerr)
	}
	if !isAdoptedPack(dest) {
		t.Error("S1: the clone must read as ADOPTED (fresh real lock, marker intact)")
	}
	if lock := readPackLock(dest); lock.Remote != url || lock.Commit != "def456" {
		t.Errorf("adoption provenance = %+v, want Remote=%s Commit=def456", lock, url)
	}
	// The symlink's target was never written through.
	if b, rerr := os.ReadFile(victim); rerr != nil || string(b) != "host secret\n" {
		t.Errorf("symlink target must be untouched, got %q (err=%v)", b, rerr)
	}
	// And the private local ref is still refused for the adopted pack.
	p, perr := loadPack(dest)
	if perr != nil {
		t.Fatalf("loadPack: %v", perr)
	}
	if _, rerr := resolvePackKnowledgeRef(&bytes.Buffer{}, dest, isAdoptedPack(dest), p.Manifest.Knowledge[0]); rerr != errPrivateRefSkippedAdopted {
		t.Errorf("private local ref of an adopted pack must be skipped, got %v", rerr)
	}
}

// TestClonePack_ScrubsCheckedInRegularPackLock: a checked-in REGULAR pack.lock
// is hostile too — markPackAdopted MERGES into the existing lock, so remote-
// authored attribution (e.g. MCP=["gog"]) would later cause a switch-away to
// remove the USER'S own MCP entries. A fresh clone's pack.lock always came
// from the remote and must be scrubbed, never merged.
func TestClonePack_ScrubsCheckedInRegularPackLock(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))

	const url = "https://example.com/attacker/pack2.git"
	env := shellEnv{run: func(name string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "clone" {
			dest := args[len(args)-1]
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return "", err
			}
			if err := writePackManifest(dest, packManifest{Name: "evil2", Schema: 1}); err != nil {
				return "", err
			}
			// Poisoned attribution: claims the user's own MCP as this pack's.
			if err := os.WriteFile(packLockPath(dest), []byte("mcp = [\"gog\", \"slack\"]\n"), 0o644); err != nil {
				return "", err
			}
		}
		return "", nil
	}}

	dest, err := clonePack(env, &bytes.Buffer{}, url)
	if err != nil {
		t.Fatalf("clonePack: %v", err)
	}
	lock := readPackLock(dest)
	if len(lock.MCP) != 0 {
		t.Errorf("remote-authored attribution must be scrubbed, not merged; got MCP=%v", lock.MCP)
	}
	if lock.Remote != url {
		t.Errorf("adoption marker must still be written, got Remote=%q", lock.Remote)
	}
}

// --- R1 [BLOCK]: two-file commit ordering + over-claim tolerance --------------

// TestRevertPackPriorContribution_ToleratesOverclaimingLock: the crash residue
// the R1 ordering deliberately chooses is a lock that OVER-claims (it names
// contributions cfg.Save never committed). Reverting such a lock must be a
// clean no-op: nothing removed, nothing clobbered, no error.
func TestRevertPackPriorContribution_ToleratesOverclaimingLock(t *testing.T) {
	cfg := &config.Config{MCP: []string{"users-own"}, GogAccount: "user@home"}
	lock := packLock{
		MCP:        []string{"never-committed"},
		Knowledge:  []string{"/nonexistent/bundle"},
		GogAccount: "pack@corp", PriorGogAccount: "stale@old",
	}
	removedMCP, removedKnowledge := revertPackPriorContribution(cfg, lock)
	if len(removedMCP) != 0 || len(removedKnowledge) != 0 {
		t.Errorf("over-claimed entries must remove nothing, got mcp=%v knowledge=%v", removedMCP, removedKnowledge)
	}
	if !containsStr(cfg.MCP, "users-own") {
		t.Errorf("a user's own MCP must survive, got %v", cfg.MCP)
	}
	// cfg.GogAccount != lock.GogAccount => the guarded revert must not fire.
	if cfg.GogAccount != "user@home" {
		t.Errorf("gog_account must not be clobbered by an over-claiming lock, got %q", cfg.GogAccount)
	}
}

// TestPackAddMcp_LockWrittenBeforeSaveFailure: the R1 ordering, behaviorally.
// cfg.Save fails (read-only config dir) AFTER the lock write — and since the
// phase-1 consistency fix (FIX A) the commit point ROLLS the lock BACK to its
// prior state (here: absent), so an ordinary Save failure leaves NO residue at
// all. A later `pack rm` (once the disk recovers) must detach cleanly with no
// orphaned contributions and no bogus "detached mcp" claim. Since round-4 F1
// the commit point exits non-zero on a Save failure, so the add runs in a
// re-exec of this test binary.
func TestPackAddMcp_LockWrittenBeforeSaveFailure(t *testing.T) {
	if os.Getenv("PI_STACK_TEST_SAVEFAIL") == "add" {
		// Child: exits 1 at the commit point (Save fails on the read-only dir).
		runPackAdd(fakeGitEnv(nil), os.Stdout, []string{"mcp", "fastmail", os.Getenv("PI_STACK_TEST_PACK_ROOT")})
		return
	}
	if os.Getuid() == 0 {
		t.Skip("root ignores directory write permissions")
	}
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "cfg")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PI_STACK_CONFIG", filepath.Join(cfgDir, "config.toml"))
	root := filepath.Join(dir, "pack")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writePackManifest(root, packManifest{Name: "work", Schema: 1}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Pack = root
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	// Make cfg.Save fail (its atomic temp file can't be created), then add.
	if err := os.Chmod(cfgDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cfgDir, 0o755) })
	cmd := exec.Command(os.Args[0], "-test.run", "^TestPackAddMcp_LockWrittenBeforeSaveFailure$")
	cmd.Env = append(os.Environ(),
		"PI_STACK_TEST_SAVEFAIL=add",
		"PI_STACK_TEST_PACK_ROOT="+root,
		"PI_STACK_CONFIG="+filepath.Join(cfgDir, "config.toml"),
	)
	childOut, childErr := cmd.CombinedOutput()
	if childErr == nil {
		t.Fatalf("round-4 F1: pack add must exit non-zero when cfg.Save fails, got:\n%s", childOut)
	}
	if !strings.Contains(string(childOut), "saving config") {
		t.Fatalf("expected the save failure message, got:\n%s", childOut)
	}
	// FIX A: the lock (written first, R1) is ROLLED BACK on the Save failure —
	// no prior lock existed, so nothing may over-claim the never-committed name.
	if lock := readPackLock(root); containsStr(lock.MCP, "fastmail") {
		t.Fatalf("FIX A: the lock must be rolled back after a Save failure, got %+v", lock)
	}
	// Config on disk never committed the entry.
	cfgAfter, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if containsStr(cfgAfter.MCP, "fastmail") {
		t.Fatalf("cfg must not carry the entry after the failed save, got %v", cfgAfter.MCP)
	}

	// Disk recovers; the next `pack rm` must detach cleanly (removal of the
	// over-claimed, absent entry is a safe no-op — not an error, not a lie).
	if err := os.Chmod(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var rmOut bytes.Buffer
	runPackRm(&rmOut, nil)
	if !strings.Contains(rmOut.String(), "detached active pack") {
		t.Errorf("pack rm must succeed after the crash residue, got:\n%s", rmOut.String())
	}
	if strings.Contains(rmOut.String(), "detached mcp") {
		t.Errorf("nothing was ever attached, so nothing must claim detachment, got:\n%s", rmOut.String())
	}
	final, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if final.Pack != "" || len(final.MCP) != 0 {
		t.Errorf("no orphaned contributions allowed: pack=%q mcp=%v", final.Pack, final.MCP)
	}
}

// --- R2 [BLOCK]: per-launch unique kit dirs + age-gated sweep ------------------

// TestSweepStaleKitTemps_AgeGatedLaunchDirs: an old (>1h) per-launch kit dir is
// swept on the next synth; a fresh one — possibly a concurrent launch mid-create
// — is left alone.
func TestSweepStaleKitTemps_AgeGatedLaunchDirs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	root := filepath.Join(dir, "pack")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "a"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &packInfo{Root: root, Manifest: packManifest{Name: "p", Proxies: []packProxy{{Name: "a"}}}}

	kit1, err := synthesizePackKit(p)
	if err != nil || kit1 == "" {
		t.Fatalf("first synth failed: %q, err=%v", kit1, err)
	}
	// Plant an OLD launch dir and an old legacy stable dir beside it.
	base := packKitDir(root)
	old := base + kitLaunchInfix + "stale"
	for _, d := range []string{old, base} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		past := time.Now().Add(-2 * time.Hour)
		if err := os.Chtimes(d, past, past); err != nil {
			t.Fatal(err)
		}
	}

	kit2, err := synthesizePackKit(p)
	if err != nil || kit2 == "" {
		t.Fatalf("second synth failed: %q, err=%v", kit2, err)
	}
	for _, gone := range []string{old, base} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("stale kit dir %s must be swept, stat err=%v", gone, err)
		}
	}
	// kit1 is fresh (same test run) — the age gate must protect it.
	if _, err := os.Stat(filepath.Join(kit1, "spec.yaml")); err != nil {
		t.Errorf("a fresh launch dir must never be swept: %v", err)
	}
}

// --- R3 [BLOCK]: marker only on a DEFINITE create ------------------------------

// TestSandboxPackMarker_NotOverwrittenOnInconclusiveProbe: the create-time pack
// marker is persisted state, so it gates on definitelyCreating — an sbxUnknown
// probe (sbx may re-attach the old sandbox) must leave an existing marker
// untouched, while a real create (absent, or --replace) writes it.
func TestSandboxPackMarker_NotOverwrittenOnInconclusiveProbe(t *testing.T) {
	// The predicate divergence that matters: willCreate optimistically preps
	// create args on a failed probe, definitelyCreating must not.
	if !willCreate(sbxUnknown, false) {
		t.Fatal("precondition: willCreate(sbxUnknown) is expected to be true")
	}
	cases := []struct {
		state   sbxState
		replace bool
		want    bool
	}{
		{sbxAbsent, false, true},
		{sbxUnknown, false, false}, // R3: inconclusive probe never writes
		{sbxRunning, false, false},
		{sbxStopped, false, false},
		// round-4 F3: --replace on an INCONCLUSIVE probe is NOT a definite
		// create — planSandboxLaunch skips the rm on sbxUnknown (RmFirst is
		// false), so sbx may re-attach the old sandbox; the marker must not be
		// overwritten (or stalePackReattachWarning would wrongly go silent).
		{sbxUnknown, true, false},
		{sbxAbsent, true, true},  // absent + replace: rm is a no-op, create is certain
		{sbxRunning, true, true}, // --replace with a positive probe really removes + creates
		{sbxStopped, true, true},
	}
	oldPack := canonicalizePackRoot(filepath.Join(t.TempDir(), "old-pack"))
	newPack := filepath.Join(t.TempDir(), "new-pack")
	for _, tc := range cases {
		if got := definitelyCreating(tc.state, tc.replace); got != tc.want {
			t.Errorf("definitelyCreating(%v, %v) = %v, want %v", tc.state, tc.replace, got, tc.want)
		}
		// Behavioral: run.go's gate over an existing marker.
		ws := t.TempDir()
		if err := os.MkdirAll(filepath.Join(ws, ".pi-stack"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sandboxPackMarkerPath(ws), []byte(oldPack+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if definitelyCreating(tc.state, tc.replace) { // mirrors runRun's marker gate
			writeSandboxPackMarker(ws, newPack)
		}
		got := readSandboxPackMarker(ws)
		if tc.want && got != canonicalizePackRoot(newPack) {
			t.Errorf("state=%v replace=%v: a definite create must write the marker, got %q", tc.state, tc.replace, got)
		}
		if !tc.want && got != oldPack {
			t.Errorf("state=%v replace=%v: a non-create must leave the marker untouched, got %q", tc.state, tc.replace, got)
		}
	}
}
