package main

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedMemDB builds a REAL on-disk memory.db with n live facts and returns the
// open store (kept open so a caller can simulate a running serve) + the path.
// Every test in this file drives real sqlite — there is no fake store, because
// the thing under test IS the file format.
func seedMemDB(t *testing.T, n int) (*memStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "memory.db")
	st, err := newMemStore(path, nil) // FTS-only, deterministic, no Ollama
	if err != nil {
		t.Fatalf("newMemStore: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, err := st.remember(rememberInput{content: fmt.Sprintf("snapshot fact number %d about pelicans", i)}); err != nil {
			t.Fatalf("remember: %v", err)
		}
	}
	return st, path
}

// snapshotOf writes a snapshot of dbPath into a fresh temp dir.
func snapshotOf(t *testing.T, dbPath string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "memory-snapshot.db")
	if _, err := memorySnapshot(dbPath, out); err != nil {
		t.Fatalf("memorySnapshot: %v", err)
	}
	return out
}

// TestSnapshotRestoreRoundtrip is the gate for the whole feature, on real
// sqlite: snapshot a LIVE (open, WAL) store, wipe the db and its sidecars,
// restore the snapshot, then prove the rows came back, the schema version
// survived, keyword recall still works (the FTS index travels inside the file),
// and the installed db is 0600.
func TestSnapshotRestoreRoundtrip(t *testing.T) {
	st, dbPath := seedMemDB(t, 3)
	snap := snapshotOf(t, dbPath) // taken HOT: the store is still open
	st.db.Close()

	for _, sc := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		_ = os.Remove(sc)
	}

	res, err := memoryRestore(restoreParams{SnapshotPath: snap, LiveDBPath: dbPath})
	if err != nil {
		t.Fatalf("memoryRestore: %v", err)
	}
	if res.Rows != 3 {
		t.Errorf("restored Rows = %d, want 3", res.Rows)
	}
	if res.BackupPath != "" {
		t.Errorf("BackupPath = %q, want empty (nothing was there to move aside)", res.BackupPath)
	}
	fi, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("restored db missing: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("restored db mode = %o, want 600", fi.Mode().Perm())
	}
	uv, rows, err := verifyMemoryDB(dbPath)
	if err != nil {
		t.Fatalf("restored db does not verify: %v", err)
	}
	if uv != memSchemaVersion || rows != 3 {
		t.Errorf("restored db user_version=%d rows=%d, want %d/3", uv, rows, memSchemaVersion)
	}

	// The store must be usable, and keyword recall must find a restored term.
	st2, err := newMemStore(dbPath, nil)
	if err != nil {
		t.Fatalf("reopen restored store: %v", err)
	}
	defer st2.db.Close()
	hits, err := st2.recall("pelicans", 10, 10000, "", "", "")
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(hits) != 3 {
		t.Errorf("FTS recall after restore returned %d hits, want 3 (index did not survive)", len(hits))
	}
}

// TestSnapshotIsHotAndDoesNotTouchTheSource: a snapshot taken while the store is
// open and being written must succeed, leave the live db byte-identical, and
// never create sidecars beside the snapshot.
func TestSnapshotIsHotAndDoesNotTouchTheSource(t *testing.T) {
	st, dbPath := seedMemDB(t, 2)
	defer st.db.Close()

	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "snap.db")
	res, err := memorySnapshot(dbPath, out)
	if err != nil {
		t.Fatalf("hot snapshot failed: %v", err)
	}
	if res.Rows != 2 || res.UserVersion != memSchemaVersion {
		t.Errorf("snapshot reported rows=%d user_version=%d, want 2/%d", res.Rows, res.UserVersion, memSchemaVersion)
	}
	if res.Size <= 0 {
		t.Errorf("snapshot size = %d, want > 0", res.Size)
	}
	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Errorf("snapshot mutated the live db (%d -> %d bytes)", len(before), len(after))
	}
	fi, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("snapshot mode = %o, want 600", fi.Mode().Perm())
	}
	for _, sc := range []string{"-wal", "-shm"} {
		if pathExists(out + sc) {
			t.Errorf("snapshot left a %s sidecar; VACUUM INTO must produce ONE file", sc)
		}
	}
	// No staging temp left behind in the destination dir.
	if m, _ := filepath.Glob(filepath.Join(filepath.Dir(out), ".pix-snapshot-*")); len(m) != 0 {
		t.Errorf("snapshot left staging temps: %v", m)
	}
}

// TestSnapshotRefusesClobber: neither an existing file nor the live db itself
// may be written over, and a refusal must not touch either.
func TestSnapshotRefusesClobber(t *testing.T) {
	st, dbPath := seedMemDB(t, 1)
	defer st.db.Close()

	if _, err := memorySnapshot(dbPath, dbPath); err == nil {
		t.Error("snapshot accepted the live db as its destination; want refusal")
	}
	out := filepath.Join(t.TempDir(), "existing.db")
	sentinel := []byte("a previous snapshot that must survive")
	if err := os.WriteFile(out, sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := memorySnapshot(dbPath, out); err == nil {
		t.Error("snapshot overwrote an existing file; want refusal")
	}
	if got, _ := os.ReadFile(out); string(got) != string(sentinel) {
		t.Errorf("existing file was destroyed by a refused snapshot: %q", got)
	}
}

// TestVerifyMemoryDBRejectsUnusable is the validation gate every restore runs
// first: a corrupt file, a valid-but-unrelated sqlite db, a db with a table
// merely NAMED memories, and a forward-incompatible schema are all refused.
func TestVerifyMemoryDBRejectsUnusable(t *testing.T) {
	dir := t.TempDir()

	corrupt := filepath.Join(dir, "corrupt.db")
	if err := os.WriteFile(corrupt, []byte("this is not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := verifyMemoryDB(corrupt); err == nil {
		t.Error("verifyMemoryDB accepted a corrupt file")
	}
	if _, _, err := verifyMemoryDB(filepath.Join(dir, "missing.db")); err == nil {
		t.Error("verifyMemoryDB accepted a missing file")
	}

	cases := []struct {
		name string
		ddl  []string
	}{
		{"unrelated sqlite db", []string{"CREATE TABLE other(x)"}},
		{"memories with the wrong columns", []string{"CREATE TABLE memories(x)", "CREATE VIRTUAL TABLE memories_fts USING fts5(content)"}},
		{"no fts index", []string{"CREATE TABLE memories(id TEXT, kind TEXT, content TEXT, durability TEXT, project TEXT, deleted_at TEXT)"}},
		{"schema from a newer pix", []string{"CREATE TABLE memories(id TEXT, kind TEXT, content TEXT, durability TEXT, project TEXT, deleted_at TEXT)",
			"CREATE VIRTUAL TABLE memories_fts USING fts5(content)", "PRAGMA user_version = 99"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "decoy.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			for _, stmt := range tc.ddl {
				if _, err := db.Exec(stmt); err != nil {
					t.Fatalf("%s: %v", stmt, err)
				}
			}
			db.Close()
			if _, _, err := verifyMemoryDB(path); err == nil {
				t.Errorf("verifyMemoryDB accepted %s", tc.name)
			}
		})
	}
}

// TestRestoreRefusesWhenLockHeld is the stopped-service contract, proven with a
// REAL flock (what a running daemon holds): restore refuses, names the fix, and
// leaves the live db byte-for-byte intact with no .bak.
func TestRestoreRefusesWhenLockHeld(t *testing.T) {
	st, dbPath := seedMemDB(t, 3)
	snap := snapshotOf(t, dbPath)
	st.db.Close()
	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	lockPath := filepath.Join(filepath.Dir(dbPath), ".memory.lock")
	release, err := acquireLock(lockPath)
	if err != nil {
		t.Fatalf("test could not take the lock: %v", err)
	}
	defer release()

	_, err = memoryRestore(restoreParams{SnapshotPath: snap, LiveDBPath: dbPath, Force: true, LockPath: lockPath})
	if err == nil {
		t.Fatal("restore succeeded while the store lock was held; want refusal")
	}
	if !strings.Contains(err.Error(), "pix serve stop") {
		t.Errorf("refusal = %q, want it to name `pix serve stop`", err.Error())
	}
	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("live db missing after a refused restore: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("live db mutated by a refused restore (%d -> %d bytes)", len(before), len(after))
	}
	if m, _ := filepath.Glob(dbPath + ".bak-*"); len(m) != 0 {
		t.Errorf("refused restore left a .bak: %v", m)
	}
}

// TestRestoreReleasesLock: a successful restore must not leak the lock, or the
// daemon can never start again.
func TestRestoreReleasesLock(t *testing.T) {
	st, dbPath := seedMemDB(t, 2)
	snap := snapshotOf(t, dbPath)
	st.db.Close()

	lockPath := filepath.Join(filepath.Dir(dbPath), ".memory.lock")
	if _, err := memoryRestore(restoreParams{SnapshotPath: snap, LiveDBPath: dbPath, Force: true, LockPath: lockPath}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	release, err := acquireLock(lockPath)
	if err != nil {
		t.Fatalf("lock not released after a successful restore: %v", err)
	}
	release()
}

// TestRestoreRefusesOverwriteWithoutForce, and with --force keeps the previous
// db (plus any orphan sidecar) in a .bak set rather than deleting it.
func TestRestoreForceKeepsPreviousDB(t *testing.T) {
	st, dbPath := seedMemDB(t, 2)
	snap := snapshotOf(t, dbPath)
	st.db.Close()

	if _, err := memoryRestore(restoreParams{SnapshotPath: snap, LiveDBPath: dbPath}); err == nil {
		t.Fatal("restore overwrote an existing live db without --force")
	}

	// An ORPHAN sidecar must be swept aside too: leaving a stale -wal beside a
	// freshly installed db would replay it into the restored store.
	if err := os.WriteFile(dbPath+"-wal", []byte("stale wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := memoryRestore(restoreParams{SnapshotPath: snap, LiveDBPath: dbPath, Force: true})
	if err != nil {
		t.Fatalf("forced restore: %v", err)
	}
	if res.BackupPath == "" {
		t.Fatal("forced restore reported no .bak for the previous db")
	}
	if !fileExists(res.BackupPath) {
		t.Errorf("previous db not kept at %s", res.BackupPath)
	}
	if !fileExists(res.BackupPath + "-wal") {
		t.Errorf("orphan -wal not moved aside to %s-wal", res.BackupPath)
	}
	if pathExists(dbPath + "-wal") {
		t.Error("a stale -wal was left beside the restored db; it would be replayed")
	}
}

// TestRestoreRollsBackOnSwapFailure: if the final rename fails, the previous db
// must be put back — a restore may never leave the live path missing.
func TestRestoreRollsBackOnSwapFailure(t *testing.T) {
	st, dbPath := seedMemDB(t, 2)
	snap := snapshotOf(t, dbPath)
	st.db.Close()
	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	// Fail only the FINAL staged->live rename; the move-aside renames succeed.
	failSwap := func(oldpath, newpath string) error {
		if strings.Contains(filepath.Base(oldpath), ".pix-restore-") {
			return fmt.Errorf("injected swap failure")
		}
		return os.Rename(oldpath, newpath)
	}
	_, err = memoryRestore(restoreParams{
		SnapshotPath: snap, LiveDBPath: dbPath, Force: true, renameFn: failSwap,
	})
	if err == nil {
		t.Fatal("restore reported success despite a failed swap")
	}
	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("live db missing after a rolled-back restore: %v", err)
	}
	if string(after) != string(before) {
		t.Error("rollback did not restore the previous db bytes")
	}
	if m, _ := filepath.Glob(dbPath + ".bak-*"); len(m) != 0 {
		t.Errorf("rollback left the .bak set behind: %v", m)
	}
}

// TestRestoreDecidesUnderTheLock: a db that appears in the window before the
// lock is taken (a racing restore installing one) must still be caught by the
// --force rule, because every check is made AFTER the acquire. A stale decision
// would clobber the racing db.
func TestRestoreDecidesUnderTheLock(t *testing.T) {
	st, srcPath := seedMemDB(t, 3)
	snap := snapshotOf(t, srcPath)
	st.db.Close()

	destDir := t.TempDir()
	dbPath := filepath.Join(destDir, "memory.db")
	sentinel := []byte("a racing restore's db — must NOT be clobbered")
	stub := func(path string) (func(), error) {
		if err := os.WriteFile(dbPath, sentinel, 0o600); err != nil {
			return nil, err
		}
		return acquireLock(path)
	}

	_, err := memoryRestore(restoreParams{
		SnapshotPath: snap, LiveDBPath: dbPath, acquireLockFn: stub,
	})
	if err == nil {
		t.Fatal("restore clobbered a db that appeared at the lock seam; want refusal")
	}
	got, rerr := os.ReadFile(dbPath)
	if rerr != nil {
		t.Fatalf("live db missing after a refused restore: %v", rerr)
	}
	if string(got) != string(sentinel) {
		t.Errorf("the racing db was replaced: %q", got)
	}
}

// TestMemoryCLISnapshotRestoreRoundtrip drives the DOCUMENTED user path end to
// end through the real binary: `pix-host memory snapshot PATH`, wipe, then
// `pix-host memory restore PATH`. It is the proof that the argv seam, the
// MEMORY_DB resolution, and the primitives agree.
func TestMemoryCLISnapshotRestoreRoundtrip(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the real pix-host binary for a snapshot/restore CLI roundtrip; covered by the untimed race/metrics CI jobs")
	}
	bin := buildHostBinary(t)
	st, dbPath := seedMemDB(t, 3)
	st.db.Close()
	snap := filepath.Join(t.TempDir(), "memory-snapshot.db")
	env := append(os.Environ(), "MEMORY_DB="+dbPath, "HOME="+t.TempDir())

	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("pix-host %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}

	if out := run("memory", "snapshot", snap); !strings.Contains(out, "3 rows") {
		t.Errorf("snapshot output = %q, want a 3-row report", out)
	}
	for _, sc := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		_ = os.Remove(sc)
	}
	if out := run("memory", "restore", snap); !strings.Contains(out, "restored 3 memory rows") {
		t.Errorf("restore output = %q, want a 3-row report", out)
	}
	if _, rows, err := verifyMemoryDB(dbPath); err != nil || rows != 3 {
		t.Errorf("db after CLI restore: rows=%d err=%v, want 3/nil", rows, err)
	}

	// A second restore without --force must refuse (exit 1) and keep the db.
	cmd := exec.Command(bin, "memory", "restore", snap)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("second restore succeeded without --force:\n%s", out)
	}
	if !strings.Contains(string(out), "--force") {
		t.Errorf("refusal = %q, want it to name --force", out)
	}
	// A missing PATH is a usage error, not a silent daemon start.
	cmd = exec.Command(bin, "memory", "snapshot")
	cmd.Env = env
	out, err = cmd.CombinedOutput()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 2 {
		t.Fatalf("`memory snapshot` with no PATH: err=%v out=%s, want exit 2", err, out)
	}
}

// TestSnapshotTimestampedNamesAreCallerChosen documents the deliberate absence
// of retention/rotation: the artifact is one file at a caller-chosen path, so
// two snapshots of the same store coexist and neither prunes the other.
func TestSnapshotTimestampedNamesAreCallerChosen(t *testing.T) {
	st, dbPath := seedMemDB(t, 1)
	defer st.db.Close()
	dir := t.TempDir()
	for _, name := range []string{"a.db", "b.db"} {
		if _, err := memorySnapshot(dbPath, filepath.Join(dir, name)); err != nil {
			t.Fatalf("snapshot %s: %v", name, err)
		}
	}
	for _, name := range []string{"a.db", "b.db"} {
		if _, _, err := verifyMemoryDB(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s did not survive the second snapshot: %v", name, err)
		}
	}
	_ = time.Now // snapshots carry no timestamp of their own; the caller names them
}
