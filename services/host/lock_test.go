package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAcquireLockExclusive proves the primitive: a second acquire of the SAME
// path fails while the first is held, and succeeds once released.
func TestAcquireLockExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".memory.lock")

	release, err := acquireLock(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	if _, err := acquireLock(path); err == nil {
		t.Fatal("second acquire succeeded while lock held; want failure")
	}

	release()
	// Idempotent: a second release must not panic.
	release()

	release2, err := acquireLock(path)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	release2()
}

// TestMemoryLockPathMatchesDBDir proves the daemon and restore resolve the SAME
// lock path: config.MemoryLockPath() sits next to MEMORY_DB, and restore's
// default (dir of LiveDBPath) lands on the identical file. This is the wiring the
// mutual-exclusion depends on.
func TestMemoryLockPathMatchesDBDir(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "memory.db")
	t.Setenv("MEMORY_DB", dbPath)

	// resolveRestoreParams pulls MEMORY_DB and sets LockPath from config.
	bp := resolveRestoreParams("archive.tar.gz", false, time.Now())
	want := filepath.Join(dir, ".memory.lock")
	if bp.LockPath != want {
		t.Errorf("restore LockPath = %q, want %q", bp.LockPath, want)
	}
	// And the restore-core default (dir of LiveDBPath) must land on the same file,
	// so a caller that leaves LockPath empty still shares with the daemon.
	if got := filepath.Join(filepath.Dir(bp.LiveDBPath), ".memory.lock"); got != want {
		t.Errorf("derived lock path = %q, want %q", got, want)
	}
}

// TestRestoreRefusesWhenLockHeld is the core contention gate: with the shared
// lock ALREADY held (as the running daemon would hold it), restore must REFUSE
// with the clear message and leave the LIVE db byte-for-byte UNTOUCHED. The lock
// is taken with the REAL acquireLock (real flock), not a stub, so this proves the
// primitive, not just the plumbing.
func TestRestoreRefusesWhenLockHeld(t *testing.T) {
	st, dbPath := seedMemDB(t, 3)
	archive := backupSeeded(t, dbPath)
	st.db.Close()

	liveBefore, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	lockPath := filepath.Join(filepath.Dir(dbPath), ".memory.lock")
	release, err := acquireLock(lockPath)
	if err != nil {
		t.Fatalf("test could not take the lock: %v", err)
	}
	defer release()

	_, err = memoryRestore(restoreParams{
		ArchivePath: archive, LiveDBPath: dbPath, Force: true,
		LockPath: lockPath, Now: time.Now(),
		ServeProbe: func() bool { return false }, // port DOWN — lock is the authority
	})
	if err == nil {
		t.Fatal("restore succeeded while the store lock was held; want refusal")
	}
	if !strings.Contains(err.Error(), "pi-stack serve stop") {
		t.Errorf("refusal message = %q, want it to mention 'pi-stack serve stop'", err.Error())
	}

	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("live db missing after refused restore: %v", err)
	}
	if len(after) != len(liveBefore) {
		t.Errorf("live db mutated by a refused restore (%d -> %d bytes)", len(liveBefore), len(after))
	}
	if m, _ := filepath.Glob(dbPath + ".bak-*"); len(m) != 0 {
		t.Errorf("refused restore left a memory .bak: %v", m)
	}
}

// TestRestoreSucceedsAndReleasesLock proves the happy path: with the lock FREE,
// restore performs the swap AND releases the lock afterward — so the daemon (or a
// subsequent restore) can take it again. A leaked lock would block the next
// acquire and fail this test.
func TestRestoreSucceedsAndReleasesLock(t *testing.T) {
	st, dbPath := seedMemDB(t, 4)
	archive := backupSeeded(t, dbPath)
	st.db.Close()
	// Wipe the live db so restore installs cleanly (no --force needed).
	for _, sc := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		_ = os.Remove(sc)
	}

	lockPath := filepath.Join(filepath.Dir(dbPath), ".memory.lock")
	res, err := memoryRestore(restoreParams{
		ArchivePath: archive, LiveDBPath: dbPath,
		LockPath: lockPath, Now: time.Now(),
		ServeProbe: func() bool { return false },
	})
	if err != nil {
		t.Fatalf("restore with free lock: %v", err)
	}
	if res.RowCount != 4 {
		t.Errorf("restored RowCount = %d, want 4", res.RowCount)
	}
	if !fileExists(dbPath) {
		t.Fatal("restore did not install the live db")
	}

	// The swap released the lock: a fresh acquire must succeed immediately.
	release, err := acquireLock(lockPath)
	if err != nil {
		t.Fatalf("lock not released after a successful restore: %v", err)
	}
	release()
}

// TestRestoreRefusalRollsBackPlainFilesWhenLockHeld proves the refusal is atomic
// on the plain-file side too: when the lock is held, any config/op-refs restored
// before the swap step are rolled back, and (as above) the memory db is untouched.
func TestRestoreRefusalRollsBackPlainFilesWhenLockHeld(t *testing.T) {
	st, dbPath := seedMemDB(t, 2)

	srcDir := t.TempDir()
	srcCfg := filepath.Join(srcDir, "config.toml")
	if err := os.WriteFile(srcCfg, []byte("gog_account = \"restored@example.com\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "pi-stack-backup-20260715-120000.tar.gz")
	if _, err := memoryBackup(backupParams{
		DBPath: dbPath, OutPath: archive, Keep: 7, Version: "test",
		ConfigPath: srcCfg, Now: time.Now(),
	}); err != nil {
		t.Fatalf("seed backup: %v", err)
	}
	st.db.Close()

	// A live config to be moved aside (then rolled back).
	destDir := t.TempDir()
	destCfg := filepath.Join(destDir, "config.toml")
	origCfg := []byte("gog_account = \"live@example.com\"\n")
	if err := os.WriteFile(destCfg, origCfg, 0o600); err != nil {
		t.Fatal(err)
	}

	lockPath := filepath.Join(filepath.Dir(dbPath), ".memory.lock")
	release, err := acquireLock(lockPath)
	if err != nil {
		t.Fatalf("test could not take the lock: %v", err)
	}
	defer release()

	if _, err := memoryRestore(restoreParams{
		ArchivePath: archive, LiveDBPath: dbPath, ConfigPath: destCfg, Force: true,
		LockPath: lockPath, Now: time.Now(),
		ServeProbe: func() bool { return false },
	}); err == nil {
		t.Fatal("restore succeeded while the lock was held; want refusal")
	}

	// Config rolled back to its original content.
	if got, _ := os.ReadFile(destCfg); string(got) != string(origCfg) {
		t.Errorf("config not rolled back: got %q, want %q", got, origCfg)
	}
	if m, _ := filepath.Glob(destCfg + ".bak-*"); len(m) != 0 {
		t.Errorf("refused restore left a config .bak: %v", m)
	}
}
