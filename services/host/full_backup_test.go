package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pix/host/config"
)

// TestFullBackupIncludesConfigOpRefsAndManifest proves the promoted FULL backup
// packs config.toml + op-refs.env + memory.db verbatim, and that a populated
// `profiles` list on backupParams (kept only for legacy-archive compat; a
// current backup never sets it) round-trips into the manifest, plus a
// knowledge note when a bundle is configured.
func TestFullBackupIncludesConfigOpRefsAndManifest(t *testing.T) {
	st, dbPath := seedMemDB(t, 2)
	defer st.db.Close()

	outDir := t.TempDir()
	cfgPath := filepath.Join(outDir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("gog_account = \"x\"\npack = \"/home/u/work-pack\"\n"), 0o600); err != nil {
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
		ConfigPath: cfgPath,
		OpRefsPath: opPath,
		Profiles:   []string{"default", "work"},
		Knowledge:  []knowledgeNote{{Path: "/home/u/kb", Remote: "git@github.com:me/kb.git"}},
		Now:        time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("memoryBackup: %v", err)
	}
	_ = res

	entries := extractTar(t, outPath)
	for _, want := range []string{"memory.db", "config.toml", "op-refs.env", "manifest.json"} {
		if _, ok := entries[want]; !ok {
			t.Errorf("archive missing %q", want)
		}
	}

	var m backupManifest
	if err := json.Unmarshal(entries["manifest.json"], &m); err != nil {
		t.Fatalf("manifest unmarshal: %v", err)
	}
	if m.FormatVersion != 2 {
		t.Errorf("manifest FormatVersion = %d, want 2", m.FormatVersion)
	}
	if strings.Join(m.Profiles, ",") != "default,work" {
		t.Errorf("manifest Profiles = %v, want [default work]", m.Profiles)
	}
	if len(m.Knowledge) != 1 || m.Knowledge[0].Path != "/home/u/kb" || m.Knowledge[0].Remote != "git@github.com:me/kb.git" {
		t.Errorf("manifest Knowledge = %+v, want one note with path+remote", m.Knowledge)
	}
}

// TestFullRestoreAppliesConfigOpRefsAndMemory proves a FULL restore swaps the
// memory db in AND restores config.toml + op-refs.env (moving the current ones
// to .bak), with the manifest's legacy `profiles` list reported back verbatim.
func TestFullRestoreAppliesConfigOpRefsAndMemory(t *testing.T) {
	st, dbPath := seedMemDB(t, 3)

	// Seed a backup that carries a config.toml + an op-refs file.
	srcDir := t.TempDir()
	srcCfg := filepath.Join(srcDir, "config.toml")
	if err := os.WriteFile(srcCfg, []byte("gog_account = \"x\"\npack = \"/home/u/work-pack\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srcOp := filepath.Join(srcDir, "op-refs.env")
	if err := os.WriteFile(srcOp, []byte("ARCHIVED=op://vault/item/field\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "pix-backup-20260715-120000.tar.gz")
	if _, err := memoryBackup(backupParams{
		DBPath: dbPath, OutPath: archive, Keep: 7, Version: "test",
		ConfigPath: srcCfg, OpRefsPath: srcOp,
		Profiles:  []string{"default", "work"},
		Knowledge: []knowledgeNote{{Path: "/home/u/kb", Remote: "git@github.com:me/kb.git"}},
		Now:       time.Now(),
	}); err != nil {
		t.Fatalf("memoryBackup: %v", err)
	}
	st.db.Close()

	// Wipe the live memory db so restore installs cleanly (no --force needed).
	for _, sc := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		_ = os.Remove(sc)
	}

	// The CURRENT config + op-refs live at different dest paths and hold OLD
	// content that must be moved aside to a .bak.
	destDir := t.TempDir()
	destCfg := filepath.Join(destDir, "config.toml")
	if err := os.WriteFile(destCfg, []byte("gog_account = \"old\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	destOp := filepath.Join(destDir, "op-refs.env")
	if err := os.WriteFile(destOp, []byte("OLD=op://old/old/old\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := memoryRestore(restoreParams{
		ArchivePath: archive,
		LiveDBPath:  dbPath,
		ConfigPath:  destCfg,
		OpRefsPath:  destOp,
		Now:         time.Date(2026, 7, 15, 9, 30, 0, 0, time.UTC),
		ServeProbe:  func() bool { return false },
	})
	if err != nil {
		t.Fatalf("memoryRestore: %v", err)
	}

	// Memory came back.
	if res.RowCount != 3 {
		t.Errorf("RowCount = %d, want 3", res.RowCount)
	}

	// Config restored + old one moved to .bak.
	if !res.ConfigRestored {
		t.Error("ConfigRestored = false, want true")
	}
	if res.ConfigBak == "" {
		t.Error("ConfigBak empty; the old config should have been moved aside")
	} else if old, _ := os.ReadFile(res.ConfigBak); !strings.Contains(string(old), "old") {
		t.Errorf("ConfigBak content = %q, want the old config", string(old))
	}
	got, _ := os.ReadFile(destCfg)
	if !strings.Contains(string(got), "work-pack") {
		t.Errorf("restored config = %q, want the archived one with the pack key", string(got))
	}

	// The legacy `profiles` list from the manifest is reported back verbatim.
	if strings.Join(res.Profiles, ",") != "default,work" {
		t.Errorf("restored Profiles = %v, want [default work]", res.Profiles)
	}

	// Op-refs restored + old one moved to .bak.
	if !res.OpRefsRestored {
		t.Error("OpRefsRestored = false, want true")
	}
	if res.OpRefsBak == "" {
		t.Error("OpRefsBak empty; the old op-refs should have been moved aside")
	}
	gotOp, _ := os.ReadFile(destOp)
	if !strings.Contains(string(gotOp), "ARCHIVED") {
		t.Errorf("restored op-refs = %q, want the archived one", string(gotOp))
	}

	// Knowledge note carried through from the manifest for the report.
	if len(res.Knowledge) != 1 || res.Knowledge[0].Path != "/home/u/kb" {
		t.Errorf("restored Knowledge = %+v, want the manifest note", res.Knowledge)
	}
}

// TestRestoreSkipsAbsentConfigOpRefs proves an archive lacking config.toml /
// op-refs.env restores memory fine and leaves the dest files untouched.
func TestRestoreSkipsAbsentConfigOpRefs(t *testing.T) {
	st, dbPath := seedMemDB(t, 2)
	// A memory-only backup (no config/op-refs paths).
	archive := backupSeeded(t, dbPath)
	st.db.Close()
	for _, sc := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		_ = os.Remove(sc)
	}

	destDir := t.TempDir()
	destCfg := filepath.Join(destDir, "config.toml")
	if err := os.WriteFile(destCfg, []byte("keep = \"me\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := memoryRestore(restoreParams{
		ArchivePath: archive, LiveDBPath: dbPath,
		ConfigPath: destCfg, OpRefsPath: filepath.Join(destDir, "op-refs.env"),
		Now: time.Now(), ServeProbe: func() bool { return false },
	})
	if err != nil {
		t.Fatalf("memoryRestore: %v", err)
	}
	if res.ConfigRestored {
		t.Error("ConfigRestored = true, want false (archive had no config.toml)")
	}
	if got, _ := os.ReadFile(destCfg); string(got) != "keep = \"me\"\n" {
		t.Errorf("dest config was touched: %q", string(got))
	}
}

// TestRestoreFormatV1MemoryOnly proves a format_version=1 archive (old
// memory-only manifest, no profiles field) still restores memory.
func TestRestoreFormatV1MemoryOnly(t *testing.T) {
	st, dbPath := seedMemDB(t, 3)
	st.db.Close()

	// Build a v1-style archive: a valid VACUUM snapshot wrapped in a manifest
	// stamped format_version=1 with no profiles/knowledge.
	dir := t.TempDir()
	snap := filepath.Join(dir, "snap.db")
	if err := vacuumInto(dbPath, snap); err != nil {
		t.Fatalf("vacuumInto: %v", err)
	}
	uv, _, err := verifySnapshot(snap)
	if err != nil {
		t.Fatalf("verifySnapshot: %v", err)
	}
	manifest := backupManifest{
		FormatVersion: 1, SqliteUserVersion: uv,
		Contents: []string{"memory.db", "manifest.json"},
	}
	archive := filepath.Join(dir, "v1.tar.gz")
	if err := writeBackupArchive(archive, snap, "", "", manifest); err != nil {
		t.Fatal(err)
	}

	// Restore into a fresh path.
	live := filepath.Join(t.TempDir(), "memory.db")
	res, err := memoryRestore(restoreParams{
		ArchivePath: archive, LiveDBPath: live,
		Now: time.Now(), ServeProbe: func() bool { return false },
	})
	if err != nil {
		t.Fatalf("memoryRestore of v1 archive: %v", err)
	}
	if res.RowCount != 3 {
		t.Errorf("RowCount = %d, want 3", res.RowCount)
	}
}

// --- Data-safety regression gates (B1-B6) -----------------------------------
//
// These lock in the backup/restore data-safety fixes on this branch. Each one is
// written to FAIL if its specific fix were reverted (see the per-test note), and
// each drives the real backup/restore cores through the same harness the tests
// above use (seedMemDB + memoryBackup/memoryRestore + extractTar).

// TestFullBackupRefusesOverwritingExistingArchive is the B1 gate: an --out that
// points at an ALREADY-EXISTING archive must be refused, and that file left
// byte-for-byte intact. The commit uses os.Link (fails EEXIST), so a revert of
// the no-clobber commit/pre-check would let a re-run destroy a prior backup.
func TestFullBackupRefusesOverwritingExistingArchive(t *testing.T) {
	st, dbPath := seedMemDB(t, 2)
	defer st.db.Close()

	outDir := t.TempDir()
	outPath := filepath.Join(outDir, "pix-backup-20260715-120000.tar.gz")
	// Pre-create a file at the --out path; its exact bytes must survive.
	sentinel := []byte("PRECIOUS previous backup \x00\x01 bytes that must not be lost")
	if err := os.WriteFile(outPath, sentinel, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := memoryBackup(backupParams{
		DBPath: dbPath, OutPath: outPath, Keep: 7, Now: time.Now(),
	})
	if err == nil {
		t.Fatal("backup accepted --out pointing at an existing archive; want refusal")
	}
	got, rerr := os.ReadFile(outPath)
	if rerr != nil {
		t.Fatalf("pre-existing archive missing after refused backup: %v", rerr)
	}
	if !bytes.Equal(got, sentinel) {
		t.Errorf("pre-existing archive was modified by a refused backup:\n got %q\nwant %q", got, sentinel)
	}
}

// TestFullBackupResolvesCanonicalConfigPaths is the B2 gate: resolveBackupParams
// must derive ConfigPath from config.Path() and OpRefsPath from
// config.OpRefsPath() (the XDG config dir), NOT a CWD-relative config/op-refs.env.
// With PIX_CONFIG set to a temp config.toml, both must land under that temp
// config dir. A revert to the old CWD-relative default would break both asserts.
func TestFullBackupResolvesCanonicalConfigPaths(t *testing.T) {
	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "config.toml")
	t.Setenv("PIX_CONFIG", cfgPath)
	t.Setenv("HOME", t.TempDir()) // isolate MEMORY_DB default too

	bp := resolveBackupParams("", 7, time.Now())

	if bp.ConfigPath != config.Path() {
		t.Errorf("ConfigPath = %q, want config.Path() = %q", bp.ConfigPath, config.Path())
	}
	if bp.OpRefsPath != config.OpRefsPath() {
		t.Errorf("OpRefsPath = %q, want config.OpRefsPath() = %q", bp.OpRefsPath, config.OpRefsPath())
	}
	// Both must be under the temp config dir (proving canonical, not CWD-relative).
	if bp.ConfigPath != cfgPath {
		t.Errorf("ConfigPath = %q, want %q (under the temp config dir)", bp.ConfigPath, cfgPath)
	}
	if want := filepath.Join(cfgDir, "op-refs.env"); bp.OpRefsPath != want {
		t.Errorf("OpRefsPath = %q, want %q (config-dir sibling)", bp.OpRefsPath, want)
	}
	if !filepath.IsAbs(bp.OpRefsPath) || strings.HasPrefix(bp.OpRefsPath, "config"+string(os.PathSeparator)) {
		t.Errorf("OpRefsPath looks CWD-relative: %q", bp.OpRefsPath)
	}
}

// TestFullRestoreRefusesMalformedArchivedConfigBeforeCommit is the B4 gate: an
// archived config.toml that is invalid TOML must abort the restore at the parse
// validation (config.LoadFrom) BEFORE anything is committed — the LIVE config is
// unchanged AND the LIVE memory.db is NOT swapped. Revert that validation and the
// restore would proceed and mutate both, failing this test.
func TestFullRestoreRefusesMalformedArchivedConfigBeforeCommit(t *testing.T) {
	st, dbPath := seedMemDB(t, 3)

	// Build a full archive whose config.toml is broken TOML, over a VALID
	// memory.db + manifest (so ONLY the config parse can abort the restore).
	srcDir := t.TempDir()
	srcCfg := filepath.Join(srcDir, "config.toml")
	if err := os.WriteFile(srcCfg, []byte("this = = broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "pix-backup-20260715-120000.tar.gz")
	if _, err := memoryBackup(backupParams{
		DBPath: dbPath, OutPath: archive, Keep: 7, Version: "test",
		ConfigPath: srcCfg, Now: time.Now(),
	}); err != nil {
		t.Fatalf("memoryBackup: %v", err)
	}
	st.db.Close() // leave the live db FILE in place (existing live state)

	memBefore, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	destDir := t.TempDir()
	destCfg := filepath.Join(destDir, "config.toml")
	cfgBefore := []byte("gog_account = \"keep-me\"\n")
	if err := os.WriteFile(destCfg, cfgBefore, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := memoryRestore(restoreParams{
		ArchivePath: archive, LiveDBPath: dbPath, ConfigPath: destCfg, Force: true,
		Now: time.Now(), ServeProbe: func() bool { return false },
	}); err == nil {
		t.Fatal("restore accepted a malformed archived config.toml; want refusal before any commit")
	}

	// LIVE config unchanged.
	if got, _ := os.ReadFile(destCfg); !bytes.Equal(got, cfgBefore) {
		t.Errorf("live config mutated by a refused restore: got %q, want %q", got, cfgBefore)
	}
	// LIVE memory.db NOT swapped.
	memAfter, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("live db missing after refused restore: %v", err)
	}
	if !bytes.Equal(memAfter, memBefore) {
		t.Errorf("live memory.db mutated by a refused restore (%d -> %d bytes)", len(memBefore), len(memAfter))
	}
	// No .bak left behind by the aborted restore.
	if m, _ := filepath.Glob(destCfg + ".bak-*"); len(m) != 0 {
		t.Errorf("aborted restore left a config .bak: %v", m)
	}
	if m, _ := filepath.Glob(dbPath + ".bak-*"); len(m) != 0 {
		t.Errorf("aborted restore left a memory .bak: %v", m)
	}
}

// TestFullRestoreRollsBackConfigOnMemoryMoveFailure is the B3 gate: a failure
// injected at the MEMORY step (via the moveRename seam, so the current db's
// move-aside fails) must roll the already-restored config + op-refs BACK to their
// originals — no split state. Revert the cross-artifact rollback and the config
// would be left as the archived one while memory never landed, failing this test.
func TestFullRestoreRollsBackConfigOnMemoryMoveFailure(t *testing.T) {
	st, dbPath := seedMemDB(t, 3)

	srcDir := t.TempDir()
	srcCfg := filepath.Join(srcDir, "config.toml")
	if err := os.WriteFile(srcCfg, []byte("gog_account = \"archived\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srcOp := filepath.Join(srcDir, "op-refs.env")
	if err := os.WriteFile(srcOp, []byte("ARCHIVED=op://v/i/f\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "pix-backup-20260715-120000.tar.gz")
	if _, err := memoryBackup(backupParams{
		DBPath: dbPath, OutPath: archive, Keep: 7, Version: "test",
		ConfigPath: srcCfg, OpRefsPath: srcOp, Now: time.Now(),
	}); err != nil {
		t.Fatalf("memoryBackup: %v", err)
	}
	st.db.Close() // keep the live db FILE so the memory move-aside is exercised

	memBefore, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	destDir := t.TempDir()
	destCfg := filepath.Join(destDir, "config.toml")
	cfgBefore := []byte("gog_account = \"original\"\n")
	if err := os.WriteFile(destCfg, cfgBefore, 0o600); err != nil {
		t.Fatal(err)
	}
	destOp := filepath.Join(destDir, "op-refs.env")
	opBefore := []byte("ORIGINAL=op://o/o/o\n")
	if err := os.WriteFile(destOp, opBefore, 0o600); err != nil {
		t.Fatal(err)
	}

	// Inject a failure into the memory move-aside (the LAST step) via moveRename.
	// The live db exists, so swapMemory attempts the move-aside and fails there.
	_, err = memoryRestore(restoreParams{
		ArchivePath: archive, LiveDBPath: dbPath, ConfigPath: destCfg, OpRefsPath: destOp, Force: true,
		Now: time.Now(), ServeProbe: func() bool { return false },
		moveRename: func(_, _ string) error { return fmt.Errorf("injected move-aside failure") },
	})
	if err == nil {
		t.Fatal("restore succeeded despite an injected memory move-aside failure; want error")
	}

	// Config + op-refs rolled back to the ORIGINALS — proving no split state.
	if got, _ := os.ReadFile(destCfg); !bytes.Equal(got, cfgBefore) {
		t.Errorf("config not rolled back after memory failure: got %q, want %q", got, cfgBefore)
	}
	if got, _ := os.ReadFile(destOp); !bytes.Equal(got, opBefore) {
		t.Errorf("op-refs not rolled back after memory failure: got %q, want %q", got, opBefore)
	}
	// Memory db was NOT swapped (move-aside failed before the final rename).
	if got, _ := os.ReadFile(dbPath); !bytes.Equal(got, memBefore) {
		t.Errorf("live memory.db mutated despite the injected failure (%d -> %d bytes)", len(memBefore), len(got))
	}
	// No orphan .bak files from the rolled-back plain-file steps.
	if m, _ := filepath.Glob(destCfg + ".bak-*"); len(m) != 0 {
		t.Errorf("rollback left a config .bak: %v", m)
	}
	if m, _ := filepath.Glob(destOp + ".bak-*"); len(m) != 0 {
		t.Errorf("rollback left an op-refs .bak: %v", m)
	}
}

// TestFullBackupManifestRedactsRemoteToken is the B6 gate, end-to-end: a
// knowledge bundle whose git origin embeds userinfo (https://user:tok@host/repo)
// must be recorded in the ACTUAL manifest bytes REDACTED (https://***@host/repo),
// so the token never reaches disk. Revert config.RedactURL in resolveBackupParams
// and the raw token would appear in manifest.json, failing this test.
func TestFullBackupManifestRedactsRemoteToken(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	const token = "tok3n-SECRET-DEADBEEF12345"
	remote := "https://user:" + token + "@github.com/me/kb.git"

	// A real git repo whose origin carries the token — bundleGitRemote reads this.
	kb := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", remote},
	} {
		cmd := exec.Command("git", append([]string{"-C", kb}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}

	// A config referencing the bundle, resolved through the canonical path.
	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("knowledge_bundles = [\""+kb+"\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIX_CONFIG", cfgPath)
	t.Setenv("HOME", t.TempDir())

	// Seed a live db and point MEMORY_DB at it so resolveBackupParams finds it.
	st, dbPath := seedMemDB(t, 1)
	st.db.Close()
	t.Setenv("MEMORY_DB", dbPath)

	// Resolve params (this is where RedactURL is applied) then actually WRITE the
	// archive so we can inspect the real manifest.json bytes.
	bp := resolveBackupParams("", 7, time.Now())
	bp.OutPath = filepath.Join(t.TempDir(), "pix-backup-20260715-120000.tar.gz")
	if _, err := memoryBackup(bp); err != nil {
		t.Fatalf("memoryBackup: %v", err)
	}

	entries := extractTar(t, bp.OutPath)
	manifestBytes, ok := entries["manifest.json"]
	if !ok {
		t.Fatal("archive missing manifest.json")
	}
	if bytes.Contains(manifestBytes, []byte(token)) {
		t.Errorf("manifest.json contains the raw token; userinfo was NOT redacted:\n%s", manifestBytes)
	}
	if bytes.Contains(manifestBytes, []byte("user:")) {
		t.Errorf("manifest.json still contains userinfo (user:...):\n%s", manifestBytes)
	}
	if want := "https://***@github.com/me/kb.git"; !bytes.Contains(manifestBytes, []byte(want)) {
		t.Errorf("manifest.json missing the redacted remote %q:\n%s", want, manifestBytes)
	}
}
