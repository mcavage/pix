// pack_repair_race_test.go — repairStaleLegacyPackState correctness under
// concurrency and against a corrupt/unreadable store.
//
// The bug these tests pin: the OLD shape computed a PRELIMINARY needCfg/
// needTrust decision from an UNLOCKED probe (which also silently treated a
// probe load error as "no need"), then carried that stale decision straight
// into the locked section instead of recomputing it fresh. That both raced a
// concurrent repair/migration (a decision made before the lock could be stale
// by the time the lock was actually held) and meant a transient load error
// during the cheap preflight silently skipped the repair forever rather than
// aborting loudly and retrying. The fix recomputes which legacy dirs are
// still absent, and reloads config + trust, FRESH, inside the lock — and
// aborts the whole repair (no mutation at all) if either fresh load fails.
package pack

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"pix/host/config"
)

// seedRepairScenario writes a legacy-stale scenario: cfg.Pack points at a
// legacy path that no longer exists on disk, and the trust store has a
// path-keyed record at that same stale legacy path. Returns the raw bytes of
// both files as they existed BEFORE any repair call, for later
// no-mutation comparisons.
func seedRepairScenario(t *testing.T, legacy, def string) (cfgBytes, trustBytes []byte) {
	t.Helper()
	if err := os.MkdirAll(def, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(def, "pack.toml"), []byte("name = \"default\"\nschema = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldCanon := CanonicalizePackRoot(legacy)
	store := &PackTrustStore{
		Version:  1,
		Accepted: map[string]PackTrustRecord{"path:" + oldCanon: {Path: oldCanon, Fingerprint: "fp"}},
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Pack = legacy // stale: legacy no longer exists on disk
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	cb, err := os.ReadFile(config.Path())
	if err != nil {
		t.Fatal(err)
	}
	tb, err := os.ReadFile(packTrustStorePath())
	if err != nil {
		t.Fatal(err)
	}
	return cb, tb
}

// --- revalidation: a legacy dir that reappears is left alone -------------

// TestRepairStaleLegacyPackStateLocked_LegacyDirRevived_NoOp: the locked core
// MUST recompute which legacy candidates are absent at the moment it runs,
// not trust whatever an earlier (unlocked) caller decided. Here the legacy
// dir is stale when the scenario is seeded, but by the time the locked
// repair actually runs it has been recreated (as if a concurrent process
// re-created/adopted it while this call waited for the lock) — the repair
// must do NOTHING: no cfg.Pack rewrite, no trust-store migration.
func TestRepairStaleLegacyPackStateLocked_LegacyDirRevived_NoOp(t *testing.T) {
	legacy, def := migrationTestEnv(t)
	seedRepairScenario(t, legacy, def)

	// The legacy dir is now live again — recreated after the scenario was
	// seeded as stale, exactly like a dir that came back while a caller was
	// blocked waiting for the trust lock.
	writeLegacyPack(t, legacy)

	newCanon := CanonicalizePackRoot(def)
	if err := withPackTrustLock(func() error {
		return repairStaleLegacyPackStateLocked(def, newCanon)
	}); err != nil {
		t.Fatalf("repairStaleLegacyPackStateLocked returned an error for a no-op case: %v", err)
	}

	reloaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Pack != legacy {
		t.Errorf("cfg.Pack must be left alone once the legacy dir is live again, got %q, want %q", reloaded.Pack, legacy)
	}
	s, err := loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	oldCanon := CanonicalizePackRoot(legacy)
	if _, ok := s.Accepted["path:"+oldCanon]; !ok {
		t.Error("trust record must remain keyed at the legacy path once it's live again")
	}
	if _, ok := s.Accepted["path:"+newCanon]; ok {
		t.Error("no migration should happen for a legacy dir that's live again")
	}
	if _, err := os.Stat(filepath.Join(legacy, "pack.toml")); err != nil {
		t.Errorf("the revived legacy pack must be untouched: %v", err)
	}
}

// --- concurrency: repeated/concurrent calls converge without corruption --

// TestRepairStaleLegacyPackState_ConcurrentCalls_ConvergeCleanly: many
// goroutines call repairStaleLegacyPackState against the SAME stale scenario
// concurrently, contending on the real cross-process flock (the same
// mechanism withPackTrustLock uses elsewhere). Whichever goroutine wins the
// lock first performs the repair; every later goroutine — recomputing
// needCfg/needTrust FRESH under the lock, against the now-already-repaired
// state — must see nothing left to do and must not re-migrate, double-count,
// or corrupt either store. This is exactly the race the old
// decide-before-the-lock shape was exposed to.
func TestRepairStaleLegacyPackState_ConcurrentCalls_ConvergeCleanly(t *testing.T) {
	legacy, def := migrationTestEnv(t)
	seedRepairScenario(t, legacy, def)

	const n = 16
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			repairStaleLegacyPackState(def)
		}()
	}
	wg.Wait()

	reloaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Pack != def {
		t.Errorf("cfg.Pack must converge to %q after concurrent repairs, got %q", def, reloaded.Pack)
	}
	s, err := loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	oldCanon := CanonicalizePackRoot(legacy)
	newCanon := CanonicalizePackRoot(def)
	if _, ok := s.Accepted["path:"+oldCanon]; ok {
		t.Errorf("stale legacy trust key must not survive concurrent repairs, got %+v", s.Accepted)
	}
	rec, ok := s.Accepted["path:"+newCanon]
	if !ok {
		t.Fatalf("migrated trust key must be present after concurrent repairs, got %+v", s.Accepted)
	}
	if rec.Fingerprint != "fp" || rec.Path != newCanon {
		t.Errorf("migrated trust record must be intact (no corruption from a concurrent double-migration), got %+v", rec)
	}
	if len(s.Accepted) != 1 {
		t.Errorf("exactly one trust record expected after convergence, got %d: %+v", len(s.Accepted), s.Accepted)
	}
}

// --- corrupt/unreadable stores: abort the WHOLE repair, no one-sided write -

// TestRepairStaleLegacyPackState_CorruptTrustStore_AbortsWithoutTouchingConfig:
// when the trust store is corrupt (unparsable JSON), the fresh in-lock load
// must fail and abort the ENTIRE repair before any mutation — in particular,
// cfg.Pack (which on its own would be a valid, needed repair) must NOT be
// rewritten. A repair that fixed config while the trust store stayed corrupt
// would be exactly the one-sided mutation this function exists to prevent.
func TestRepairStaleLegacyPackState_CorruptTrustStore_AbortsWithoutTouchingConfig(t *testing.T) {
	legacy, def := migrationTestEnv(t)
	cfgBytesBefore, _ := seedRepairScenario(t, legacy, def)

	// Corrupt the trust store directly (bypassing store.Save()'s valid-JSON
	// path) — simulates disk corruption or a hostile/torn write.
	corrupt := []byte("{ not valid json")
	if err := os.WriteFile(packTrustStorePath(), corrupt, 0o644); err != nil {
		t.Fatal(err)
	}

	// Redirect stderr so the expected error log doesn't spam the test output;
	// restore it unconditionally afterward.
	origStderr := os.Stderr
	r, w, perr := os.Pipe()
	if perr != nil {
		t.Fatal(perr)
	}
	os.Stderr = w
	repairStaleLegacyPackState(def)
	os.Stderr = origStderr
	_ = w.Close()
	var logged bytes.Buffer
	_, _ = logged.ReadFrom(r)

	if logged.Len() == 0 {
		t.Error("expected the repair failure to be logged (log and retry later), got nothing on stderr")
	}

	cfgBytesAfter, err := os.ReadFile(config.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cfgBytesBefore, cfgBytesAfter) {
		t.Errorf("config must be UNTOUCHED when the trust-store load fails (no one-sided mutation); before=%q after=%q", cfgBytesBefore, cfgBytesAfter)
	}
	trustAfter, err := os.ReadFile(packTrustStorePath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(corrupt, trustAfter) {
		t.Errorf("corrupt trust store must be left exactly as-is (never partially rewritten), got %q", trustAfter)
	}
}

// TestRepairStaleLegacyPackState_CorruptConfig_AbortsWithoutTouchingTrust:
// the mirror image — a corrupt config.toml must abort the whole repair
// before the (independently valid) trust-store migration is ever saved.
func TestRepairStaleLegacyPackState_CorruptConfig_AbortsWithoutTouchingTrust(t *testing.T) {
	legacy, def := migrationTestEnv(t)
	_, trustBytesBefore := seedRepairScenario(t, legacy, def)

	// Corrupt config.toml directly (invalid TOML — an unterminated table
	// header), bypassing cfg.Save()'s valid-encode path.
	corrupt := []byte("[not valid toml\n")
	if err := os.WriteFile(config.Path(), corrupt, 0o644); err != nil {
		t.Fatal(err)
	}

	origStderr := os.Stderr
	r, w, perr := os.Pipe()
	if perr != nil {
		t.Fatal(perr)
	}
	os.Stderr = w
	repairStaleLegacyPackState(def)
	os.Stderr = origStderr
	_ = w.Close()
	var logged bytes.Buffer
	_, _ = logged.ReadFrom(r)

	if logged.Len() == 0 {
		t.Error("expected the repair failure to be logged (log and retry later), got nothing on stderr")
	}

	trustAfter, err := os.ReadFile(packTrustStorePath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(trustBytesBefore, trustAfter) {
		t.Errorf("trust store must be UNTOUCHED when the config load fails (no one-sided mutation); before=%q after=%q", trustBytesBefore, trustAfter)
	}
	cfgAfter, err := os.ReadFile(config.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(corrupt, cfgAfter) {
		t.Errorf("corrupt config must be left exactly as-is (never partially rewritten), got %q", cfgAfter)
	}
}
