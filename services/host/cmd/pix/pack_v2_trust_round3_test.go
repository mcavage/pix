// pack_v2_trust_round3_test.go — round-3 review fixes for the packs-v2
// Phase 2 trust model:
//
//	#1 — the trust store has a cross-process lock: every read-modify-write
//	     goes through withPackTrustLock/mutatePackTrustStore, which re-load
//	     the store FRESH under the lock — a concurrent `pix host`
//	     wrapper refresh can no longer save a stale object over the
//	     activation/acceptance a `pack use` just committed.
//	#2 — one-time Phase-1 → Phase-2 migration: a pre-Phase-2 active pack's
//	     pack.lock attribution is lifted into the host-state store before a
//	     switch computes its removal set — for a LOCAL (authored) pack only;
//	     an adopted pack's lock is payload and never trusted (revert nothing).
//	#3 — unknown local-vs-remote MCP classification FAILS CLOSED: a name that
//	     cannot be classified (probe failed) is treated as HOST-EXEC (Tier-1)
//	     and gated; gog stays the reference-only Tier-0 special case.
//	#4 — a host-wrapper clear failure is an honest failure: `pack rm` exits
//	     non-zero and does NOT claim "detached"; the lenient refresh surfaces
//	     the error instead of reporting success.
//	#5 — acceptance identity is commit-STABLE: the trust key is the remote
//	     URL (or canonical path) without the commit, so a new commit with a
//	     byte-identical host-exec fingerprint never re-prompts, while any
//	     fingerprint change still re-gates. Legacy commit-suffixed keys are
//	     honored via a one-time fallback.
package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/sys/systest"
)

// --- #1: cross-process lock, fresh-load mutations ------------------------------

// TestMutatePackTrustStore_InterleavedMutationsLoseNothing: two writers whose
// in-memory views were loaded independently (the `pack use` commit vs the host
// launch's wrapper refresh) both mutate through the serialized helper — each
// re-loads FRESH under the lock, so neither can clobber the other's committed
// record (the old last-writer-wins bug saved whichever stale object came last).
func TestMutatePackTrustStore_InterleavedMutationsLoseNothing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	root := filepath.Join(dir, "pack")

	// Writer 1 (a `pack use` commit): records an activation + an acceptance.
	if _, err := mutatePackTrustStore(func(s *packTrustStore) error {
		s.setActivation(root, packLock{MCP: []string{"a-mcp"}})
		s.recordAcceptance("path:"+root, packTrustRecord{Path: root, Fingerprint: "fp1"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Writer 2 (a concurrent host launch): its logical view predates writer 1,
	// but the helper re-loads fresh, so its Installed write lands BESIDE the
	// activation instead of over it.
	if _, err := mutatePackTrustStore(func(s *packTrustStore) error {
		s.Installed = &packInstalledSet{Owner: "path:" + root, Wrappers: []string{"tool"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	after, err := loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	if got := after.activationFor(root); len(got.MCP) != 1 || got.MCP[0] != "a-mcp" {
		t.Errorf("the committed activation was clobbered by a later mutation, got %+v", got)
	}
	if fp, ok := after.acceptedFingerprint("path:" + root); !ok || fp != "fp1" {
		t.Errorf("the committed acceptance was clobbered, got (%q,%v)", fp, ok)
	}
	if after.Installed == nil || !slices.Contains(after.Installed.Wrappers, "tool") {
		t.Errorf("the second writer's own mutation must land too, got %+v", after.Installed)
	}
}

// TestMutatePackTrustStore_ConcurrentWritersSerialized: N goroutines each
// commit one acceptance record through the lock-serialized helper; every
// record survives (no lost update). Run with -race, this also exercises the
// flock across distinct file descriptors.
func TestMutatePackTrustStore_ConcurrentWritersSerialized(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	const writers = 8
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("path:/pack-%d", i)
			if _, err := mutatePackTrustStore(func(s *packTrustStore) error {
				s.recordAcceptance(key, packTrustRecord{Path: fmt.Sprintf("/pack-%d", i), Fingerprint: "fp"})
				return nil
			}); err != nil {
				t.Errorf("writer %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	after, err := loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < writers; i++ {
		if _, ok := after.acceptedFingerprint(fmt.Sprintf("path:/pack-%d", i)); !ok {
			t.Errorf("writer %d's record was lost (last-writer-wins clobber); store=%+v", i, after.Accepted)
		}
	}
}

// --- #2: one-time Phase-1 → Phase-2 activation migration ------------------------

// TestPackUse_MigratesPhase1LocalActivation: a Phase-1 active LOCAL pack has
// its attribution ONLY in pack.lock (the store has no activation record). The
// first Phase-2 switch must migrate that lock into the store so A's
// contribution reverts correctly — while a user-added MCP survives untouched.
func TestPackUse_MigratesPhase1LocalActivation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	pinLocalMCP(t) // nothing is local: a-mcp stays a remote (Tier-0) reference

	rootA := filepath.Join(dir, "a")
	mustWritePack(t, rootA, packManifest{Name: "a", Schema: 1,
		Integrations: []packIntegration{{Name: "A", MCP: "a-mcp"}}})
	rootB := filepath.Join(dir, "b")
	mustWritePack(t, rootB, packManifest{Name: "b", Schema: 1})

	// Phase-1 residue: config carries the pack's MCP AND the user's own; the
	// attribution lives ONLY in pack.lock; there is NO trust store at all.
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.AddMCP("usermcp")
	cfg.AddMCP("a-mcp")
	cfg.Pack = rootA
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if err := writePackLock(rootA, packLock{MCP: []string{"a-mcp"}}); err != nil {
		t.Fatal(err)
	}
	if _, serr := os.Stat(packTrustStorePath()); !os.IsNotExist(serr) {
		t.Fatalf("test setup: the store must not exist yet (Phase-1 state), stat=%v", serr)
	}

	var out bytes.Buffer
	runPackUse(fakeGitEnv(nil), &out, []string{rootB})
	cfg2, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(cfg2.MCP, "a-mcp") {
		t.Errorf("Phase-1 attribution must be migrated: switching away must revert a-mcp, cfg.MCP=%v", cfg2.MCP)
	}
	if !slices.Contains(cfg2.MCP, "usermcp") {
		t.Errorf("the user's own MCP must survive the migrated switch, cfg.MCP=%v", cfg2.MCP)
	}
}

// TestPackUse_AdoptedPackLockNeverMigrated: an ADOPTED pack with no store
// activation record must NOT have its pack.lock trusted (it is remote-writable
// payload — a forged lock could claim the user's own entries). The switch
// reverts NOTHING (safe over-retention) and the user's config survives.
func TestPackUse_AdoptedPackLockNeverMigrated(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	pinLocalMCP(t)

	rootA := filepath.Join(dir, "a")
	mustWritePack(t, rootA, packManifest{Name: "a", Schema: 1})
	rootB := filepath.Join(dir, "b")
	mustWritePack(t, rootB, packManifest{Name: "b", Schema: 1})

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.AddMCP("usermcp")
	cfg.Pack = rootA
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	// A is ADOPTED (host-recorded provenance) but has NO activation record —
	// and its lock (a `git pull` away from the attacker) claims the user's MCP.
	if err := recordPackAdoptionInTrustStore(rootA, "https://example.com/a.git", "c1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(packLockPath(rootA),
		[]byte("mcp = [\"usermcp\"]\nremote = \"https://example.com/a.git\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runPackUse(fakeGitEnv(nil), &out, []string{rootB})
	cfg2, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cfg2.MCP, "usermcp") {
		t.Errorf("CRITICAL: an adopted pack's lock was migrated and deleted the user's own MCP, cfg.MCP=%v", cfg2.MCP)
	}
}

// --- #3: unknown MCP classification fails closed --------------------------------

// TestLocalMCPClassifier_UnknownFailsClosed: when the local set cannot be
// established (no probe at all, or a failing probe), a non-gog name classifies
// as HOST-EXEC — the pack is Tier-1 and the gate fails closed on a non-TTY
// without --yes. gog stays reference-only Tier-0.
func TestLocalMCPClassifier_UnknownFailsClosed(t *testing.T) {
	// No probe available at all.
	unknown := localMCPClassifier(hostenv.Env{System: &systest.Fake{}}, nil)
	if !unknown("fastmail") {
		t.Error("unknown classification must treat a non-gog name as host-exec (fail closed)")
	}
	if unknown(gwServerName) {
		t.Error("gog stays the reference-only Tier-0 special case even when the partition is unknown")
	}
	// Probe resolves but errors.
	failEnv := hostenv.Env{System: &systest.Fake{RunFn: func(string, ...string) (string, error) { return "", fmt.Errorf("probe failed") }}}
	resolver := func() (string, error) { return "pix-host", nil }
	unknown2 := localMCPClassifier(failEnv, resolver)
	if !unknown2("notion") {
		t.Error("a failing `mcp --list` probe must classify a non-gog name as host-exec")
	}

	p := &packInfo{Root: "/p", Manifest: packManifest{Name: "p",
		Integrations: []packIntegration{{Name: "N", MCP: "notion"}}}}
	b := computeHostBoM(p, "", unknown2)
	if !b.tier1() {
		t.Fatalf("a pack whose MCP cannot be classified must be Tier-1 (gated), got %+v", b)
	}
	// The gate itself fails closed non-interactively without --yes.
	if err := packTrustGate(strings.NewReader(""), io.Discard, false, false, "p", b); err == nil {
		t.Error("non-TTY without --yes must fail closed for an unclassifiable MCP")
	}
	// A nil classifier (no partition available) fails closed the same way.
	if bn := computeHostBoM(p, "", nil); !bn.tier1() {
		t.Errorf("a nil classifier must fail closed too, got %+v", bn)
	}
	// gog-only packs stay Tier-0 under an unknown partition.
	pg := &packInfo{Root: "/p", Manifest: packManifest{Name: "g",
		Integrations: []packIntegration{{Name: "gog", MCP: gwServerName}}}}
	if computeHostBoM(pg, "", unknown2).tier1() {
		t.Error("a gog-only reference must stay Tier-0 even when the partition is unknown")
	}
}

// --- #4: a clear failure is an honest failure ------------------------------------

// TestPackRm_ClearFailureExitsNonZero: when the installed host wrappers cannot
// be removed (symlinked host bin dir), `pack rm` must exit non-zero and must
// NOT claim "detached" — and nothing detaches, so a plain re-run retries.
// Subprocess: the failure path os.Exits.
func TestPackRm_ClearFailureExitsNonZero(t *testing.T) {
	if os.Getenv("PIX_TEST_TRUST") == "rm-clear-fail" {
		runPackRm(os.Stdout, nil)
		return // exit 0 == the clear failure was swallowed as success
	}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	stateDir := filepath.Join(dir, "state")
	t.Setenv("PIX_CONFIG", cfgPath)
	t.Setenv("XDG_STATE_HOME", stateDir)

	root := filepath.Join(dir, "pack")
	mustWritePack(t, root, packManifest{Name: "p", Schema: 1})
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Pack = root
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := mutatePackTrustStore(func(s *packTrustStore) error {
		s.Installed = &packInstalledSet{Owner: "path:" + root, Wrappers: []string{"tool"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Make the clear FAIL: hostPackBinDir is a symlink (never traversed).
	if err := os.MkdirAll(hostAgentDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(dir, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, hostPackBinDir()); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run", "^TestPackRm_ClearFailureExitsNonZero$")
	cmd.Env = append(os.Environ(),
		"PIX_TEST_TRUST=rm-clear-fail",
		"PIX_CONFIG="+cfgPath,
		"XDG_STATE_HOME="+stateDir,
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("pack rm must exit non-zero when the host wrappers cannot be removed; output:\n%s", out)
	}
	if strings.Contains(string(out), "detached active pack") {
		t.Errorf("pack rm must NOT claim \"detached\" on a clear failure, got:\n%s", out)
	}
	// Nothing detached: the active pack and the attribution are intact, so a
	// re-run retries the whole detach.
	cfg2, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.Pack != root {
		t.Errorf("nothing may detach on a clear failure, cfg.Pack=%q", cfg2.Pack)
	}
	store, serr := loadPackTrustStore()
	if serr != nil {
		t.Fatal(serr)
	}
	if store.Installed == nil || !slices.Contains(store.Installed.Wrappers, "tool") {
		t.Errorf("attribution must be kept until removal is confirmed, got %+v", store.Installed)
	}
}

// TestRefreshHostPackWrappers_LenientClearFailureSurfaced: the lenient
// (non-strict) refresh must RETURN a clear failure instead of reporting
// success while wrappers remain (round-3 #4; strict launch already refused).
func TestRefreshHostPackWrappers_LenientClearFailureSurfaced(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	if _, err := mutatePackTrustStore(func(s *packTrustStore) error {
		s.Installed = &packInstalledSet{Owner: "path:/gone", Wrappers: []string{"stale"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(hostAgentDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(dir, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, hostPackBinDir()); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	var out bytes.Buffer
	if _, err := refreshHostPackWrappers(&out, &config.Config{}, false); err == nil {
		t.Error("the lenient refresh must surface a clear failure, not report success")
	}
}

// --- #5: acceptance identity is commit-stable ------------------------------------

// TestTrustKey_StableAcrossCommits: the trust key is the remote URL without
// the commit; a provenance update (new commit after a pull) does not move the
// identity, so an acceptance over an unchanged fingerprint still matches (no
// re-prompt) while a CHANGED fingerprint still mismatches (re-gates). A
// legacy commit-suffixed record is honored via the one-time fallback.
func TestTrustKey_StableAcrossCommits(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	root := filepath.Join(dir, "clone")
	mustWritePack(t, root, packManifest{Name: "c", Schema: 1})
	const url = "https://example.com/x.git"

	if err := recordPackAdoptionInTrustStore(root, url, "c1"); err != nil {
		t.Fatal(err)
	}
	store, err := loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	key := store.trustKey(root)
	if strings.Contains(key, "#") {
		t.Fatalf("the trust key must not embed the commit (round-3 #5), got %q", key)
	}
	store.recordAcceptance(key, packTrustRecord{Path: canonicalizePackRoot(root), Remote: url, Commit: "c1", Fingerprint: "fp1"})
	if err := store.save(); err != nil {
		t.Fatal(err)
	}

	// A new commit lands (git pull): provenance updates, identity must not move.
	if err := recordPackAdoptionInTrustStore(root, url, "c2"); err != nil {
		t.Fatal(err)
	}
	store2, err := loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	if got := store2.trustKey(root); got != key {
		t.Errorf("a commit bump moved the identity (%q -> %q); acceptance would spuriously re-gate", key, got)
	}
	got, ok := store2.acceptedFingerprint(store2.trustKey(root))
	if !ok || got != "fp1" {
		t.Errorf("same fingerprint across commits must stay accepted (no re-prompt), got (%q,%v)", got, ok)
	}
	if got == "fp2-changed-surface" {
		t.Error("sanity: a changed fingerprint must mismatch and re-gate")
	}

	// Legacy commit-suffixed key (pre-round-3 store on disk) still honored.
	legacy := &packTrustStore{Accepted: map[string]packTrustRecord{
		"remote:https://l.example/y.git#c9": {Remote: "https://l.example/y.git", Commit: "c9", Fingerprint: "fpL"},
	}}
	if fp, ok := legacy.acceptedFingerprint("remote:https://l.example/y.git"); !ok || fp != "fpL" {
		t.Errorf("legacy commit-suffixed acceptance must be honored once, got (%q,%v)", fp, ok)
	}
}

// TestPackUse_NewCommitSameFingerprintDoesNotRegate (end-to-end): an accepted
// adopted Tier-1 pack whose provenance commit changes — but whose host-exec
// fingerprint is byte-identical — re-activates non-interactively with NO
// --yes. In-process: a misfiring gate would os.Exit(1) and fail the test
// binary, exactly like the non-interactive pack trust-gate tests.
func TestPackUse_NewCommitSameFingerprintDoesNotRegate(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	root := phase2HostPack(t, dir, "work", "platformio")
	const url = "https://example.com/work.git"
	if err := recordPackAdoptionInTrustStore(root, url, "c1"); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	runPackUse(fakeGitEnv(nil), &out, []string{root, "--yes"}) // accept once

	// A README-only pull: new commit, identical host-exec surface.
	if err := recordPackAdoptionInTrustStore(root, url, "c2"); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	runPackUse(fakeGitEnv(nil), &out, []string{root}) // no --yes, non-TTY
	if strings.Contains(out.String(), "adds these integrations to Pix") {
		t.Errorf("a commit bump with an unchanged fingerprint must not re-prompt:\n%s", out.String())
	}

	// A CHANGED surface (mutated wrapper script) still re-gates: the strict
	// launch-side check refuses until re-accepted.
	if err := os.WriteFile(filepath.Join(root, "bin", "platformio"), []byte("#!/bin/sh\necho evil\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if _, rerr := refreshHostPackWrappers(&out, cfg, true); rerr == nil {
		t.Error("a changed host-exec fingerprint must still fail closed (re-gate/refuse)")
	}
}
