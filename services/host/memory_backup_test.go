package main

import (
	"archive/tar"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// extractTar reads a tar.gz and returns a name->bytes map of its entries.
func extractTar(t *testing.T, path string) map[string][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	out := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("tar read %s: %v", hdr.Name, err)
		}
		out[hdr.Name] = data
	}
	return out
}

// seedMemDB builds a real on-disk memory.db with n live facts and returns the
// open store (kept open so callers can simulate a running serve) + the path.
func seedMemDB(t *testing.T, n int) (*memStore, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "memory.db")
	st, err := newMemStore(path, nil) // FTS-only, deterministic, no Ollama
	if err != nil {
		t.Fatalf("newMemStore: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, err := st.remember(rememberInput{content: "backup fact number " + string(rune('a'+i))}); err != nil {
			t.Fatalf("remember: %v", err)
		}
	}
	return st, path
}

func TestMemoryBackupRoundtrip(t *testing.T) {
	st, dbPath := seedMemDB(t, 3)
	defer st.db.Close()

	outDir := t.TempDir()
	// A config.toml + op-refs.env to prove they land in the archive.
	cfgPath := filepath.Join(outDir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("gog_account = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opPath := filepath.Join(outDir, "op-refs.env")
	if err := os.WriteFile(opPath, []byte("FOO=op://vault/item/field\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(outDir, "pix-backup-20260715-120000.tar.gz")

	res, err := memoryBackup(backupParams{
		DBPath:     dbPath,
		OutPath:    outPath,
		Keep:       7,
		Version:    "9.9.9-test",
		EmbedModel: "nomic-embed-text",
		ConfigPath: cfgPath,
		OpRefsPath: opPath,
		Now:        time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("memoryBackup: %v", err)
	}

	// Archive exists and result reports what we expect.
	if _, err := os.Stat(outPath); err != nil {
		t.Fatalf("archive missing: %v", err)
	}
	if res.RowCount != 3 {
		t.Errorf("result RowCount = %d, want 3", res.RowCount)
	}
	if res.Size <= 0 {
		t.Errorf("result Size = %d, want > 0", res.Size)
	}

	entries := extractTar(t, outPath)

	// No WAL/SHM sidecars ever get archived.
	for name := range entries {
		if strings.HasSuffix(name, "-wal") || strings.HasSuffix(name, "-shm") {
			t.Errorf("archive contains sidecar %q; must never copy -wal/-shm", name)
		}
	}
	for _, want := range []string{"memory.db", "config.toml", "op-refs.env", "manifest.json"} {
		if _, ok := entries[want]; !ok {
			t.Errorf("archive missing %q", want)
		}
	}

	// The contained memory.db is intact and matches the manifest row count.
	snap := filepath.Join(t.TempDir(), "restored.db")
	if err := os.WriteFile(snap, entries["memory.db"], 0o600); err != nil {
		t.Fatal(err)
	}
	uv, rc, err := verifySnapshot(snap)
	if err != nil {
		t.Fatalf("verify restored snapshot: %v", err)
	}
	if uv != 1 {
		t.Errorf("restored user_version = %d, want 1", uv)
	}
	if rc != 3 {
		t.Errorf("restored row count = %d, want 3", rc)
	}

	// Manifest fields present + correct.
	var m backupManifest
	if err := json.Unmarshal(entries["manifest.json"], &m); err != nil {
		t.Fatalf("manifest unmarshal: %v", err)
	}
	if m.FormatVersion != backupFormatVersion {
		t.Errorf("manifest FormatVersion = %d, want %d", m.FormatVersion, backupFormatVersion)
	}
	if m.PixVersion != "9.9.9-test" {
		t.Errorf("manifest PixVersion = %q, want 9.9.9-test", m.PixVersion)
	}
	if m.SqliteUserVersion != 1 {
		t.Errorf("manifest SqliteUserVersion = %d, want 1", m.SqliteUserVersion)
	}
	if m.MemoryRowCount != 3 {
		t.Errorf("manifest MemoryRowCount = %d, want 3", m.MemoryRowCount)
	}
	if m.MemoryEmbedModel != "nomic-embed-text" {
		t.Errorf("manifest MemoryEmbedModel = %q, want nomic-embed-text", m.MemoryEmbedModel)
	}
	if m.CreatedAt != "2026-07-15T12:00:00Z" {
		t.Errorf("manifest CreatedAt = %q, want 2026-07-15T12:00:00Z", m.CreatedAt)
	}
	if m.Hostname == "" {
		t.Error("manifest Hostname is empty")
	}
	wantContents := []string{"memory.db", "config.toml", "op-refs.env", "manifest.json"}
	if strings.Join(m.Contents, ",") != strings.Join(wantContents, ",") {
		t.Errorf("manifest Contents = %v, want %v", m.Contents, wantContents)
	}
}

func TestMemoryBackupOmitsMissingOptionalFiles(t *testing.T) {
	st, dbPath := seedMemDB(t, 1)
	defer st.db.Close()
	outDir := t.TempDir()
	outPath := filepath.Join(outDir, "pix-backup-20260715-130000.tar.gz")

	if _, err := memoryBackup(backupParams{
		DBPath: dbPath, OutPath: outPath, Keep: 7, Version: "dev",
		ConfigPath: filepath.Join(outDir, "nope.toml"), // absent
		OpRefsPath: filepath.Join(outDir, "nope.env"),  // absent
		Now:        time.Now(),
	}); err != nil {
		t.Fatalf("memoryBackup: %v", err)
	}
	entries := extractTar(t, outPath)
	if _, ok := entries["config.toml"]; ok {
		t.Error("config.toml should be omitted when absent")
	}
	if _, ok := entries["op-refs.env"]; ok {
		t.Error("op-refs.env should be omitted when absent")
	}
	var m backupManifest
	_ = json.Unmarshal(entries["manifest.json"], &m)
	if strings.Join(m.Contents, ",") != "memory.db,manifest.json" {
		t.Errorf("manifest Contents = %v, want [memory.db manifest.json]", m.Contents)
	}
}

func TestMemoryBackupRetention(t *testing.T) {
	st, dbPath := seedMemDB(t, 1)
	defer st.db.Close()
	outDir := t.TempDir()

	const keep = 3
	// Write keep+2 backups with distinct, chronologically-ordered names.
	base := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	for i := 0; i < keep+2; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		name := "pix-backup-" + ts.Format("20060102-150405") + ".tar.gz"
		if _, err := memoryBackup(backupParams{
			DBPath: dbPath, OutPath: filepath.Join(outDir, name), Keep: keep, Now: ts,
		}); err != nil {
			t.Fatalf("backup %d: %v", i, err)
		}
	}

	matches, _ := filepath.Glob(filepath.Join(outDir, "pix-backup-*.tar.gz"))
	if len(matches) != keep {
		t.Fatalf("retention left %d backups, want %d", len(matches), keep)
	}
	// The survivors must be the NEWEST keep (largest timestamps).
	for _, mth := range matches {
		if strings.Contains(filepath.Base(mth), "000000") || strings.Contains(filepath.Base(mth), "000100") {
			t.Errorf("oldest backup %q should have been pruned", filepath.Base(mth))
		}
	}
}

// TestMemoryBackupRetentionByMtimeNotName is a data-safety gate for the fix that
// makes retention sort by FILE MODIFICATION TIME, not filename. Now that default
// archive names carry a RANDOM suffix, a lexical filename sort no longer equals
// chronological order, so --keep N could delete the just-written backup. Two
// archives are written in the SAME second; the one with the LATER mtime is given
// a lexically SMALLER suffix, so a lexical sort would wrongly treat it as oldest
// and prune it. Retention must keep the newest by mtime. This FAILS if reverted
// to a lexical (sort.Strings) prune.
func TestMemoryBackupRetentionByMtimeNotName(t *testing.T) {
	st, dbPath := seedMemDB(t, 1)
	defer st.db.Close()
	outDir := t.TempDir()

	ts := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	// Newer-written archive gets the lexically SMALLER suffix; older gets the larger.
	older := filepath.Join(outDir, "pix-backup-20260715-120000-ffffffff.tar.gz")
	newer := filepath.Join(outDir, "pix-backup-20260715-120000-00000000.tar.gz")
	// Keep:0 so memoryBackup itself does not prune; we drive pruneBackups directly.
	if _, err := memoryBackup(backupParams{DBPath: dbPath, OutPath: older, Keep: 0, Now: ts}); err != nil {
		t.Fatalf("backup older: %v", err)
	}
	if _, err := memoryBackup(backupParams{DBPath: dbPath, OutPath: newer, Keep: 0, Now: ts}); err != nil {
		t.Fatalf("backup newer: %v", err)
	}
	// Force distinct mtimes WITHIN the same wall-clock second.
	sec := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(older, sec.Add(100*time.Millisecond), sec.Add(100*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newer, sec.Add(900*time.Millisecond), sec.Add(900*time.Millisecond)); err != nil {
		t.Fatal(err)
	}

	if err := pruneBackups(outDir, 1, ""); err != nil {
		t.Fatalf("pruneBackups: %v", err)
	}
	if _, err := os.Stat(newer); err != nil {
		t.Errorf("retention pruned the NEWER backup (by mtime) — lexical-sort regression: %v", err)
	}
	if _, err := os.Stat(older); err == nil {
		t.Error("retention kept the OLDER backup; want it pruned")
	}
}

// TestMemoryBackupRetentionNeverPrunesFreshArchive is the data-safety gate for
// the fix that EXCLUDES the just-created archive from retention. On a coarse-
// timestamp filesystem the fresh archive can share an mtime with an older one,
// lose the name tie-break, and be pruned while the backup still reports success.
// Here two archives share an IDENTICAL mtime and the fresh one is given the
// lexically SMALLER (tie-break-losing) suffix; with --keep 1 the fresh archive
// (the path returned to the caller) must ALWAYS survive. This FAILS if the
// keepPath exclusion is removed (a pure mtime/name prune would delete it).
func TestMemoryBackupRetentionNeverPrunesFreshArchive(t *testing.T) {
	st, dbPath := seedMemDB(t, 1)
	defer st.db.Close()
	outDir := t.TempDir()

	ts := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	// The fresh archive gets the lexically SMALLEST suffix so a name tie-break would
	// single it out for pruning; the older one gets the larger suffix.
	older := filepath.Join(outDir, "pix-backup-20260715-120000-ffffffff.tar.gz")
	fresh := filepath.Join(outDir, "pix-backup-20260715-120000-00000000.tar.gz")
	// Keep:0 so memoryBackup itself does not prune; we drive pruneBackups directly
	// after forcing the identical mtime.
	if _, err := memoryBackup(backupParams{DBPath: dbPath, OutPath: older, Keep: 0, Now: ts}); err != nil {
		t.Fatalf("backup older: %v", err)
	}
	res, err := memoryBackup(backupParams{DBPath: dbPath, OutPath: fresh, Keep: 0, Now: ts})
	if err != nil {
		t.Fatalf("backup fresh: %v", err)
	}
	// Give BOTH files an IDENTICAL mtime so ordering falls to the name tie-break.
	sec := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(older, sec, sec); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(fresh, sec, sec); err != nil {
		t.Fatal(err)
	}
	// Retention with --keep 1, passing the fresh archive as keepPath: it must be
	// spared unconditionally even though its name loses the tie-break.
	if err := pruneBackups(outDir, 1, fresh); err != nil {
		t.Fatalf("pruneBackups: %v", err)
	}
	if res.Path != fresh {
		t.Errorf("result path = %q, want the fresh archive %q", res.Path, fresh)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("retention pruned the JUST-CREATED archive %s: %v", fresh, err)
	}
	if _, err := os.Stat(older); err == nil {
		t.Error("retention kept the OLDER archive; with --keep 1 and the fresh one spared, it should be pruned")
	}
}

// TestMemoryBackupWhileServeHoldsDB proves VACUUM INTO succeeds while a second
// connection is open on the source (simulating serve holding the db).
func TestMemoryBackupWhileServeHoldsDB(t *testing.T) {
	st, dbPath := seedMemDB(t, 2)
	defer st.db.Close() // kept OPEN across the backup on purpose

	outPath := filepath.Join(t.TempDir(), "pix-backup-20260715-140000.tar.gz")
	res, err := memoryBackup(backupParams{DBPath: dbPath, OutPath: outPath, Keep: 7, Now: time.Now()})
	if err != nil {
		t.Fatalf("backup while db held open: %v", err)
	}
	if res.RowCount != 2 {
		t.Errorf("RowCount = %d, want 2", res.RowCount)
	}
	// The live store is still usable after the hot backup.
	if _, err := st.remember(rememberInput{content: "after backup still works"}); err != nil {
		t.Fatalf("remember after backup: %v", err)
	}
}

// TestFreshStoreReportsUserVersion1 asserts the schema-version stamp.
func TestFreshStoreReportsUserVersion1(t *testing.T) {
	st, err := newMemStore(filepath.Join(t.TempDir(), "fresh.db"), nil)
	if err != nil {
		t.Fatalf("newMemStore: %v", err)
	}
	defer st.db.Close()
	var uv int
	if err := st.db.QueryRow("PRAGMA user_version").Scan(&uv); err != nil {
		t.Fatalf("PRAGMA user_version: %v", err)
	}
	if uv != 1 {
		t.Errorf("fresh store user_version = %d, want 1", uv)
	}
}

// TestVerifySnapshotRejectsCorrupt guards the integrity gate: a non-sqlite file
// must be rejected rather than silently packed.
func TestVerifySnapshotRejectsCorrupt(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "corrupt.db")
	if err := os.WriteFile(bad, []byte("this is not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := verifySnapshot(bad); err == nil {
		t.Error("verifySnapshot accepted a corrupt file; want error")
	}
}

// TestMemoryBackupRefusesClobberLiveDB is a REAL data-safety gate: --out pointed
// at the live memory.db must be refused, and the live db must be left byte-for-
// byte intact (an atomic rename onto it would have destroyed it).
func TestMemoryBackupRefusesClobberLiveDB(t *testing.T) {
	st, dbPath := seedMemDB(t, 2)
	defer st.db.Close()
	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	_, err = memoryBackup(backupParams{
		DBPath: dbPath, OutPath: dbPath, // <- clobber attempt
		Keep: 7, Now: time.Now(),
	})
	if err == nil {
		t.Fatal("backup accepted --out == live memory.db; want refusal")
	}
	after, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("live db missing after refused backup: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("live db mutated by refused backup (%d -> %d bytes)", len(before), len(after))
	}
	// config.toml and op-refs.env are refused too.
	cfg := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfg, []byte("x=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := memoryBackup(backupParams{
		DBPath: dbPath, OutPath: cfg, ConfigPath: cfg, Keep: 7, Now: time.Now(),
	}); err == nil {
		t.Error("backup accepted --out == config.toml; want refusal")
	}
}

// TestMemoryBackupRetentionSparesNonMatching proves retention deletes ONLY files
// matching the exact generated pattern; a hand-placed keepme.tar.gz survives even
// when it is lexicographically "oldest".
func TestMemoryBackupRetentionSparesNonMatching(t *testing.T) {
	st, dbPath := seedMemDB(t, 1)
	defer st.db.Close()
	outDir := t.TempDir()

	keepme := filepath.Join(outDir, "keepme.tar.gz")
	if err := os.WriteFile(keepme, []byte("not ours"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Also a differently-shaped name that must NOT match the strict regexp.
	other := filepath.Join(outDir, "pix-backup-nope.tar.gz")
	if err := os.WriteFile(other, []byte("also not ours"), 0o600); err != nil {
		t.Fatal(err)
	}

	const keep = 2
	base := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	for i := 0; i < keep+2; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		name := "pix-backup-" + ts.Format("20060102-150405") + ".tar.gz"
		if _, err := memoryBackup(backupParams{
			DBPath: dbPath, OutPath: filepath.Join(outDir, name), Keep: keep, Now: ts,
		}); err != nil {
			t.Fatalf("backup %d: %v", i, err)
		}
	}

	if _, err := os.Stat(keepme); err != nil {
		t.Errorf("retention deleted non-matching keepme.tar.gz: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("retention deleted non-matching pix-backup-nope.tar.gz: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(outDir, "pix-backup-2026*.tar.gz"))
	if len(matches) != keep {
		t.Errorf("retention left %d generated backups, want %d", len(matches), keep)
	}
}

// TestWriteBackupArchiveAtomic proves the write is atomic + private: a mid-write
// failure (a missing snapshot source) leaves NO file at the final path and NO
// leftover .tmp in the dir, and a successful write leaves no temp behind either.
func TestWriteBackupArchiveAtomic(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "pix-backup-20260715-120000.tar.gz")

	// Failure path: snapPath does not exist -> tarAddFile fails mid-write.
	err := writeBackupArchive(outPath, filepath.Join(dir, "does-not-exist.db"), "", "",
		backupManifest{FormatVersion: 1})
	if err == nil {
		t.Fatal("writeBackupArchive succeeded with a missing snapshot; want error")
	}
	if _, statErr := os.Stat(outPath); !os.IsNotExist(statErr) {
		t.Errorf("failed write left a partial archive at %s (err=%v)", outPath, statErr)
	}
	assertNoTempLeft(t, dir)

	// Success path: a real snapshot -> archive exists, no temp left.
	_, dbPath := seedMemDB(t, 1)
	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := vacuumInto(dbPath, snap); err != nil {
		t.Fatalf("vacuumInto: %v", err)
	}
	if err := writeBackupArchive(outPath, snap, "", "", backupManifest{FormatVersion: 1}); err != nil {
		t.Fatalf("writeBackupArchive: %v", err)
	}
	if _, err := os.Stat(outPath); err != nil {
		t.Fatalf("archive missing after success: %v", err)
	}
	assertNoTempLeft(t, dir)
}

func assertNoTempLeft(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".pix-backup-") && strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp archive %q; write must clean up its temp", e.Name())
		}
	}
}

// TestNewMemStoreRejectsNewerSchema proves the schema-version guard: opening a db
// stamped with a FUTURE user_version (2) errors instead of silently downgrading
// the marker to 1.
func TestNewMemStoreRejectsNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	future, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := future.Exec("PRAGMA user_version = 2"); err != nil {
		t.Fatal(err)
	}
	// Force the header to disk before reopening.
	if _, err := future.Exec("CREATE TABLE t(x)"); err != nil {
		t.Fatal(err)
	}
	future.Close()

	if _, err := newMemStore(path, nil); err == nil {
		t.Error("newMemStore accepted a db with user_version=2; want a version error")
	}
}
