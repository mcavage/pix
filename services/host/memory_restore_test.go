package main

import (
	"archive/tar"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// backupSeeded takes a hot backup of dbPath into a fresh tar.gz and returns its
// path. Shared by the restore tests; mirrors what `memory backup` writes.
func backupSeeded(t *testing.T, dbPath string) string {
	t.Helper()
	outPath := filepath.Join(t.TempDir(), "pi-stack-backup-20260715-120000.tar.gz")
	if _, err := memoryBackup(backupParams{
		DBPath: dbPath, OutPath: outPath, Keep: 7, Version: "test", Now: time.Now(),
	}); err != nil {
		t.Fatalf("memoryBackup: %v", err)
	}
	return outPath
}

// TestMemoryRestoreRoundtrip is the real gate: seed a db, back it up, wipe the
// source, restore the archive, then prove the rows are back, integrity is ok,
// and FTS search finds a restored term.
func TestMemoryRestoreRoundtrip(t *testing.T) {
	st, dbPath := seedMemDB(t, 3)
	archive := backupSeeded(t, dbPath)
	st.db.Close() // release the source so we can wipe it

	// Wipe the source db + any sidecars, simulating a lost/corrupt live db.
	for _, sc := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		_ = os.Remove(sc)
	}

	res, err := memoryRestore(restoreParams{
		ArchivePath: archive,
		LiveDBPath:  dbPath,
		Force:       false, // no live db exists, so --force not required
		Now:         time.Now(),
		ServeProbe:  func() bool { return false },
	})
	if err != nil {
		t.Fatalf("memoryRestore: %v", err)
	}
	if res.RowCount != 3 {
		t.Errorf("restored RowCount = %d, want 3", res.RowCount)
	}
	if res.BackupPath != "" {
		t.Errorf("BackupPath = %q, want empty (no prior live db)", res.BackupPath)
	}

	// Reopen the restored db as a live store: integrity ok, rows back, FTS works.
	if err := integrityCheckDB(dbPath); err != nil {
		t.Fatalf("restored db integrity: %v", err)
	}
	st2, err := newMemStore(dbPath, nil)
	if err != nil {
		t.Fatalf("reopen restored db: %v", err)
	}
	defer st2.db.Close()
	hits, err := st2.recall("backup fact", 8, 100000, "", "", "")
	if err != nil {
		t.Fatalf("recall on restored db: %v", err)
	}
	if len(hits) == 0 {
		t.Error("FTS recall found nothing in the restored db; index not rebuilt")
	}
}

// TestMemoryRestoreRefusesWhenServeRunning proves the injected probe gates the
// restore and leaves the live db untouched.
func TestMemoryRestoreRefusesWhenServeRunning(t *testing.T) {
	st, dbPath := seedMemDB(t, 2)
	archive := backupSeeded(t, dbPath)
	st.db.Close()

	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	_, err = memoryRestore(restoreParams{
		ArchivePath: archive,
		LiveDBPath:  dbPath,
		Force:       true,
		Now:         time.Now(),
		ServeProbe:  func() bool { return true }, // "serve is up"
	})
	if err == nil {
		t.Fatal("restore succeeded while serve running; want refusal")
	}

	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("live db missing after refused restore: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("live db mutated during refused restore (%d -> %d bytes)", len(before), len(after))
	}
}

// TestMemoryRestoreRefusesOverwriteWithoutForce proves an existing live db is
// not clobbered silently; --force allows it and keeps the old db as a .bak.
func TestMemoryRestoreRefusesOverwriteWithoutForce(t *testing.T) {
	st, dbPath := seedMemDB(t, 2)
	archive := backupSeeded(t, dbPath)
	st.db.Close()

	// A live db still sits at dbPath. Without --force, refuse.
	if _, err := memoryRestore(restoreParams{
		ArchivePath: archive, LiveDBPath: dbPath, Force: false,
		Now: time.Now(), ServeProbe: func() bool { return false },
	}); err == nil {
		t.Fatal("restore overwrote existing live db without --force; want refusal")
	}

	// With --force, it proceeds and moves the old db aside to a .bak-<ts>.
	ts := time.Date(2026, 7, 15, 9, 30, 0, 0, time.UTC)
	res, err := memoryRestore(restoreParams{
		ArchivePath: archive, LiveDBPath: dbPath, Force: true,
		Now: ts, ServeProbe: func() bool { return false },
	})
	if err != nil {
		t.Fatalf("restore --force: %v", err)
	}
	if res.BackupPath == "" {
		t.Fatal("BackupPath empty; want the moved-aside .bak of the previous db")
	}
	if _, err := os.Stat(res.BackupPath); err != nil {
		t.Errorf(".bak of previous db missing at %s: %v", res.BackupPath, err)
	}
	if res.RowCount != 2 {
		t.Errorf("RowCount = %d, want 2", res.RowCount)
	}
}

// TestMemoryRestoreDeletesStaleSidecar proves a leftover -wal at the dest is
// removed (not replayed onto the restored file).
func TestMemoryRestoreDeletesStaleSidecar(t *testing.T) {
	st, dbPath := seedMemDB(t, 1)
	archive := backupSeeded(t, dbPath)
	st.db.Close()

	// Plant a bogus WAL sidecar next to the live db.
	walPath := dbPath + "-wal"
	if err := os.WriteFile(walPath, []byte("stale wal garbage that must not replay"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := memoryRestore(restoreParams{
		ArchivePath: archive, LiveDBPath: dbPath, Force: true,
		Now: time.Now(), ServeProbe: func() bool { return false },
	}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, err := os.Stat(walPath); !os.IsNotExist(err) {
		t.Errorf("stale %s still present after restore (err=%v); must be deleted", walPath, err)
	}
}

// TestMemoryRestoreVersionGate proves a manifest whose schema version is newer
// than this binary understands is refused.
func TestMemoryRestoreVersionGate(t *testing.T) {
	if err := validateRestoreManifest(backupManifest{
		FormatVersion: backupFormatVersion, SqliteUserVersion: 999,
	}); err == nil {
		t.Error("validateRestoreManifest accepted sqlite_user_version=999; want refusal")
	}
	// Sanity: a same-version manifest is accepted.
	if err := validateRestoreManifest(backupManifest{
		FormatVersion: backupFormatVersion, SqliteUserVersion: restoreSchemaVersion,
	}); err != nil {
		t.Errorf("validateRestoreManifest rejected a valid manifest: %v", err)
	}
	// An unknown format is refused too.
	if err := validateRestoreManifest(backupManifest{
		FormatVersion: 99, SqliteUserVersion: 1,
	}); err == nil {
		t.Error("validateRestoreManifest accepted format_version=99; want refusal")
	}
}

// TestMemoryRestoreRefusesCorruptArchive proves a backup whose memory.db fails
// integrity_check is refused and the live db is left intact.
func TestMemoryRestoreRefusesCorruptArchive(t *testing.T) {
	st, dbPath := seedMemDB(t, 2)
	st.db.Close()

	// Build an archive that carries a valid manifest but a corrupt memory.db.
	dir := t.TempDir()
	badDB := filepath.Join(dir, "memory.db")
	if err := os.WriteFile(badDB, []byte("this is not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := backupManifest{
		FormatVersion: backupFormatVersion, SqliteUserVersion: restoreSchemaVersion,
		Contents: []string{"memory.db", "manifest.json"},
	}
	archive := filepath.Join(dir, "bad.tar.gz")
	if err := writeBackupArchive(archive, badDB, "", "", manifest); err != nil {
		t.Fatal(err)
	}

	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := memoryRestore(restoreParams{
		ArchivePath: archive, LiveDBPath: dbPath, Force: true,
		Now: time.Now(), ServeProbe: func() bool { return false },
	}); err == nil {
		t.Fatal("restore accepted a corrupt archived db; want refusal")
	}

	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("live db missing after refused restore: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("live db mutated during refused restore (%d -> %d bytes)", len(before), len(after))
	}
	// No .bak should have been created (we refused before the swap).
	matches, _ := filepath.Glob(dbPath + ".bak-*")
	if len(matches) != 0 {
		t.Errorf("refused restore left a .bak: %v", matches)
	}
}

// TestMemoryRestoreMissingMemoryDB proves an archive without memory.db is
// refused (defends the "require memory.db present" step).
func TestMemoryRestoreMissingMemoryDB(t *testing.T) {
	dir := t.TempDir()
	manifestOnly := filepath.Join(dir, "manifest-only.tar.gz")
	writeManifestOnlyArchive(t, manifestOnly, backupManifest{
		FormatVersion: backupFormatVersion, SqliteUserVersion: restoreSchemaVersion,
	})
	if _, err := memoryRestore(restoreParams{
		ArchivePath: manifestOnly, LiveDBPath: filepath.Join(dir, "live.db"),
		Now: time.Now(), ServeProbe: func() bool { return false },
	}); err == nil {
		t.Error("restore accepted an archive with no memory.db; want refusal")
	}
}

// TestMemoryRestorePreservesWAL is REAL gate (a): a -wal sidecar at the dest
// (committed data not yet checkpointed) must be MOVED into the .bak set, not
// deleted, so the previous state is fully recoverable.
func TestMemoryRestorePreservesWAL(t *testing.T) {
	st, dbPath := seedMemDB(t, 2)
	archive := backupSeeded(t, dbPath)
	st.db.Close()

	// Plant a -wal holding the "old" committed state alongside the live db.
	walPath := dbPath + "-wal"
	walData := []byte("previous committed wal state that must be recoverable")
	if err := os.WriteFile(walPath, walData, 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := memoryRestore(restoreParams{
		ArchivePath: archive, LiveDBPath: dbPath, Force: true,
		Now: time.Now(), ServeProbe: func() bool { return false },
	})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	// The old -wal must have travelled into the .bak set (recoverable), NOT be
	// deleted, and NOT be left at the dest to replay onto the restored db.
	bakWal := res.BackupPath + "-wal"
	got, err := os.ReadFile(bakWal)
	if err != nil {
		t.Fatalf("previous -wal not preserved in .bak set at %s: %v", bakWal, err)
	}
	if string(got) != string(walData) {
		t.Errorf("preserved -wal content changed; want the original bytes")
	}
	if _, err := os.Stat(walPath); !os.IsNotExist(err) {
		t.Errorf("stale -wal still at dest after restore (err=%v); must be moved aside", err)
	}
}

// TestMemoryRestoreRejectsRealVersionOverManifest is REAL gate (b): the ARCHIVED
// db's ACTUAL user_version is checked, not the manifest's claim. A db with
// user_version=999 wrapped in a manifest that lies "1" is refused, live db intact.
func TestMemoryRestoreRejectsRealVersionOverManifest(t *testing.T) {
	st, dbPath := seedMemDB(t, 2)
	st.db.Close()
	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	// Build a VALID sqlite db but stamp a future schema version.
	dir := t.TempDir()
	futureDB := filepath.Join(dir, "memory.db")
	fdb, err := sql.Open("sqlite", futureDB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fdb.Exec("CREATE TABLE memories(x)"); err != nil {
		t.Fatal(err)
	}
	if _, err := fdb.Exec("PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	fdb.Close()

	// Manifest LIES that the schema is 1.
	manifest := backupManifest{FormatVersion: backupFormatVersion, SqliteUserVersion: 1}
	archive := filepath.Join(dir, "lying.tar.gz")
	if err := writeBackupArchive(archive, futureDB, "", "", manifest); err != nil {
		t.Fatal(err)
	}

	if _, err := memoryRestore(restoreParams{
		ArchivePath: archive, LiveDBPath: dbPath, Force: true,
		Now: time.Now(), ServeProbe: func() bool { return false },
	}); err == nil {
		t.Fatal("restore accepted a db with real user_version=999 under a manifest saying 1; want refusal")
	}
	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("live db missing after refused restore: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("live db mutated by refused restore (%d -> %d bytes)", len(before), len(after))
	}
	if m, _ := filepath.Glob(dbPath + ".bak-*"); len(m) != 0 {
		t.Errorf("refused restore left a .bak: %v", m)
	}
}

// TestMemoryRestoreRollsBackOnRenameFailure is REAL gate (c): if the FINAL
// staged->live rename fails, the previous db is rolled back so memory.db still
// exists (never left missing).
func TestMemoryRestoreRollsBackOnRenameFailure(t *testing.T) {
	st, dbPath := seedMemDB(t, 3)
	archive := backupSeeded(t, dbPath)
	st.db.Close()
	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	injected := fmt.Errorf("injected rename failure")
	_, err = memoryRestore(restoreParams{
		ArchivePath: archive, LiveDBPath: dbPath, Force: true,
		Now: time.Now(), ServeProbe: func() bool { return false },
		swapRename: func(_, _ string) error { return injected },
	})
	if err == nil {
		t.Fatal("restore succeeded despite an injected rename failure; want error")
	}
	// memory.db must still exist AND be the original (rolled back from the .bak).
	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("live db missing after failed rename — rollback did not restore it: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("rolled-back db differs from original (%d -> %d bytes)", len(before), len(after))
	}
	// The .bak set must have been rolled back (no orphan left).
	if m, _ := filepath.Glob(dbPath + ".bak-*"); len(m) != 0 {
		t.Errorf("rollback left an orphaned .bak: %v", m)
	}
	// No staged temp left behind.
	if m, _ := filepath.Glob(filepath.Join(filepath.Dir(dbPath), ".memory-restore-*.tmp")); len(m) != 0 {
		t.Errorf("failed restore left a staged temp: %v", m)
	}
}

// TestMemoryRestoreFTSWithoutRebuild is REAL gate (d): the FTS content travels
// inside memory.db (VACUUM INTO copies the content table), so recall works after
// restore WITHOUT any FTS 'rebuild' step (which was removed).
func TestMemoryRestoreFTSWithoutRebuild(t *testing.T) {
	st, dbPath := seedMemDB(t, 3)
	archive := backupSeeded(t, dbPath)
	st.db.Close()
	for _, sc := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		_ = os.Remove(sc)
	}

	if _, err := memoryRestore(restoreParams{
		ArchivePath: archive, LiveDBPath: dbPath, Force: false,
		Now: time.Now(), ServeProbe: func() bool { return false },
	}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	st2, err := newMemStore(dbPath, nil)
	if err != nil {
		t.Fatalf("reopen restored db: %v", err)
	}
	defer st2.db.Close()
	hits, err := st2.recall("backup fact", 8, 100000, "", "", "")
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(hits) == 0 {
		t.Error("FTS recall found nothing after restore; the FTS content did not travel in memory.db")
	}
}

// TestExtractTarGzRejectsTarBomb is REAL gate (e): an archive with more entries
// than the cap is refused rather than extracted.
func TestExtractTarGzRejectsTarBomb(t *testing.T) {
	dir := t.TempDir()
	bomb := filepath.Join(dir, "bomb.tar.gz")
	f, err := os.Create(bomb)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for i := 0; i < restoreMaxEntryCount+5; i++ {
		if err := tarAddBytes(tw, fmt.Sprintf("f%d", i), []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if err := extractTarGz(bomb, t.TempDir()); err == nil {
		t.Error("extractTarGz accepted an archive with too many entries; want a tar-bomb refusal")
	}
}

// TestMemoryRestoreReprobeAbortsBeforeSwap is REAL gate (f): if serve was down on
// the first probe but comes UP by the pre-swap re-probe, restore aborts before
// touching the live db.
func TestMemoryRestoreReprobeAbortsBeforeSwap(t *testing.T) {
	st, dbPath := seedMemDB(t, 2)
	archive := backupSeeded(t, dbPath)
	st.db.Close()
	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	calls := 0
	probe := func() bool {
		calls++
		return calls > 1 // down on first probe, up on the re-probe
	}
	if _, err := memoryRestore(restoreParams{
		ArchivePath: archive, LiveDBPath: dbPath, Force: true,
		Now: time.Now(), ServeProbe: probe,
	}); err == nil {
		t.Fatal("restore proceeded after serve came up at the re-probe; want abort")
	}
	if calls < 2 {
		t.Errorf("ServeProbe called %d times; the pre-swap re-probe did not run", calls)
	}
	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("live db missing after aborted restore: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("live db mutated by aborted restore (%d -> %d bytes)", len(before), len(after))
	}
	if m, _ := filepath.Glob(dbPath + ".bak-*"); len(m) != 0 {
		t.Errorf("aborted restore left a .bak: %v", m)
	}
}

// writeManifestOnlyArchive packs just a manifest.json (no memory.db) so a test
// can exercise the "require memory.db present" gate.
func writeManifestOnlyArchive(t *testing.T, outPath string, m backupManifest) {
	t.Helper()
	f, err := os.Create(outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := tarAddBytes(tw, "manifest.json", data); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
}
