// pi-stack-host `memory restore` — the safety-critical counterpart to
// `memory backup` (memory_backup.go). It takes a backup tar.gz and swaps the
// contained memory.db in as the live database WITHOUT ever corrupting or
// silently clobbering what is already there.
//
// The whole flow is defensive, in order:
//
//  1. REFUSE if `serve` is up (a live writer + a file swap = corruption).
//  2. Extract the archive (with tar-bomb + zip-slip guards), validate the
//     manifest (format + schema version), and require memory.db to be present.
//  3. Read the ARCHIVED db's ACTUAL user_version and cross-check it against the
//     manifest AND the supported version (refuse a newer/mismatched db), then
//     integrity-check it before trusting it near the live db.
//  4. Verify the archived db actually carries the memory schema, then refuse to
//     overwrite an existing live db unless --force is given.
//  5. Stage the validated db into a UNIQUE dest-dir temp, RE-PROBE serve is STILL
//     down IMMEDIATELY before the first live-file rename (the stage copy can be
//     large, so this is the tightest TOCTOU point), then move the CURRENT state
//     (db + -wal + -shm, including any ORPHAN sidecar with no main db) ASIDE into
//     a unique .bak-<ts>-<rand> set (kept, never deleted; no stale sidecar left
//     to replay), then os.Rename the staged file into place LAST (atomic on
//     POSIX). On any failure at/before the final rename, ROLL the moved-aside set
//     back so the live db is never missing — and if that rollback itself fails,
//     return a LOUD error instead of silently continuing.
//  6. Report the row count + the .bak path of the previous db. The FTS index
//     travels inside memory.db (content table) — no rebuild needed.
//
// This lives in pi-stack-host (it needs the sqlite driver); the launcher
// `pi-stack memory restore` execs it. The core is split into pure/logic steps
// (manifest + version gate) and fs/db steps so tests can drive each in isolation
// and inject the "serve running?" probe.

package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Tar-bomb guards: caps applied while extracting an untrusted archive so a
// maliciously-crafted backup cannot exhaust disk. Generous enough to never trip
// on a real memory.db + config + op-refs + manifest.
const (
	restoreMaxTotalBytes = 500 << 20 // 500 MiB across all entries
	restoreMaxEntryBytes = 200 << 20 // 200 MiB for a single entry
	restoreMaxEntryCount = 1000      // at most this many entries
)

// restoreSchemaVersion is the memory schema (PRAGMA user_version) this binary
// understands. An archive whose sqlite_user_version is newer than this was
// written by a newer pi-stack and cannot be safely restored here.
const restoreSchemaVersion = 1

// restoreParams are the fully-resolved inputs to the restore core, so it stays
// hermetic and testable (no env/home lookups, no real socket dial inside).
type restoreParams struct {
	ArchivePath string      // source backup tar.gz
	LiveDBPath  string      // dest live memory.db to swap in
	Force       bool        // overwrite an existing live db
	Now         time.Time   // timestamp source for .bak/.restore names (zero -> time.Now())
	ServeProbe  func() bool // reports whether a serve daemon holds the db (nil -> assume down)
	// swapRename performs the FINAL staged->live rename. nil -> os.Rename. Tests
	// inject a failure here to prove the rollback path restores the previous db.
	swapRename func(oldpath, newpath string) error
	// moveRename performs the move-aside AND rollback renames (current db/sidecars
	// <-> .bak set). nil -> os.Rename. Tests inject a failure on the ROLLBACK
	// direction to prove a failed rollback is surfaced LOUDLY.
	moveRename func(oldpath, newpath string) error
}

// restoreResult reports what happened, for the CLI to print.
type restoreResult struct {
	LivePath   string // where the restored db now lives
	RowCount   int    // recount of live memories in the restored db
	BackupPath string // .bak path of the previous live db ("" if none existed)
}

// validateRestoreManifest is the PURE version gate: the archive must be a format
// this binary understands and must NOT carry a schema newer than ours. Kept free
// of any fs/db work so tests can exercise the gate directly.
func validateRestoreManifest(m backupManifest) error {
	if m.FormatVersion != backupFormatVersion {
		return fmt.Errorf("archive format_version %d is not understood (this binary handles %d)",
			m.FormatVersion, backupFormatVersion)
	}
	if m.SqliteUserVersion > restoreSchemaVersion {
		return fmt.Errorf("archive sqlite schema version %d is newer than this binary's (%d); upgrade pi-stack before restoring",
			m.SqliteUserVersion, restoreSchemaVersion)
	}
	return nil
}

// memoryRestore is the testable core. It performs the full safety flow above and
// returns the result. It never removes the moved-aside .bak set of the previous
// db, and on any failure BEFORE/AT the final rename it rolls the previous db
// back so the live path is never left missing.
func memoryRestore(p restoreParams) (restoreResult, error) {
	now := p.Now
	if now.IsZero() {
		now = time.Now()
	}
	rename := p.swapRename
	if rename == nil {
		rename = os.Rename
	}
	moveRename := p.moveRename
	if moveRename == nil {
		moveRename = os.Rename
	}

	// 1. REFUSE if a serve daemon is up — it would write to the db while we swap
	// the file underneath it, which corrupts both.
	if p.ServeProbe != nil && p.ServeProbe() {
		return restoreResult{}, fmt.Errorf("memory serve is running; stop it first: pi-stack serve stop")
	}

	// 2a. Extract the archive to a private temp dir (with tar-bomb guards) and read
	// the manifest.
	tmpDir, err := os.MkdirTemp("", "pi-stack-restore-")
	if err != nil {
		return restoreResult{}, fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	if err := extractTarGz(p.ArchivePath, tmpDir); err != nil {
		return restoreResult{}, err
	}

	manifest, err := readRestoreManifest(filepath.Join(tmpDir, "manifest.json"))
	if err != nil {
		return restoreResult{}, err
	}
	if err := validateRestoreManifest(manifest); err != nil {
		return restoreResult{}, err
	}

	archivedDB := filepath.Join(tmpDir, "memory.db")
	if !fileExists(archivedDB) {
		return restoreResult{}, fmt.Errorf("archive is missing memory.db")
	}

	// 2b. Read the ARCHIVED db's ACTUAL schema version (not the manifest's claim)
	// and cross-check it against both the supported version and the manifest. A
	// forged/stale manifest saying "1" over a v999 db must NOT get restored.
	realVersion, err := dbUserVersion(archivedDB)
	if err != nil {
		return restoreResult{}, fmt.Errorf("read archived schema version: %w", err)
	}
	if realVersion > restoreSchemaVersion {
		return restoreResult{}, fmt.Errorf("archived db schema version %d is newer than this binary's (%d); upgrade pi-stack before restoring",
			realVersion, restoreSchemaVersion)
	}
	if realVersion != manifest.SqliteUserVersion {
		return restoreResult{}, fmt.Errorf("archived db schema version %d does not match the manifest (%d); refusing a tampered/inconsistent backup",
			realVersion, manifest.SqliteUserVersion)
	}

	// 3. Integrity-check the ARCHIVED db before we trust it near the live one.
	if err := integrityCheckDB(archivedDB); err != nil {
		return restoreResult{}, err
	}

	// 3b. Integrity + user_version can PASS on a db that is missing the memory
	// schema (e.g. a valid-but-empty sqlite file with user_version=1). Swapping
	// that in yields an unusable live db (countLiveRows would then fail). Verify
	// the archived db actually carries the memory store BEFORE touching live state.
	if err := verifyArchivedUsable(archivedDB); err != nil {
		return restoreResult{}, err
	}

	// 4. Guard against a silent clobber of an existing live db.
	liveExists := fileExists(p.LiveDBPath)
	if liveExists && !p.Force {
		return restoreResult{}, fmt.Errorf("live db already exists at %s; pass --force to overwrite (the current db is moved aside to a .bak first)", p.LiveDBPath)
	}

	destDir := filepath.Dir(p.LiveDBPath)
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return restoreResult{}, fmt.Errorf("dest dir: %w", err)
	}

	// 5a. Stage the validated db into a UNIQUE temp in the DEST dir (os.CreateTemp,
	// not a predictable name) so the final rename is same-filesystem and atomic.
	// This copy can be large; it does NOT touch live state, so it is safe to do
	// BEFORE the final serve re-probe.
	staged, err := stageRestoredDB(destDir, archivedDB)
	if err != nil {
		return restoreResult{}, fmt.Errorf("stage restored db: %w", err)
	}
	defer os.Remove(staged) // best-effort: gone after a successful rename

	// 5b. Re-probe serve is STILL down IMMEDIATELY before the first rename that
	// touches live files. The stage copy above can take a while (up to ~200MB), so
	// the earlier probe would leave a window where serve starts DURING the copy;
	// this is the last safe point to bail with zero live-state mutation.
	if p.ServeProbe != nil && p.ServeProbe() {
		return restoreResult{}, fmt.Errorf("memory serve came up during restore; aborted before any swap — stop it and retry")
	}

	// 5c. Move the CURRENT state aside COMPLETELY (db + -wal + -shm) into a unique
	// .bak-<ts>-<rand> set, so no committed WAL data is lost and no stale sidecar
	// is left at the dest to replay onto the restored file. This runs even when the
	// main db is ABSENT: a prior failed run can leave an ORPHAN -wal/-shm with NO
	// main db, and installing the restored db beside a stale WAL would replay it.
	stamp := now.Format("20060102-150405")
	bakBase := ""
	var movedAside [][2]string // [from, to] pairs, for rollback
	needMove := liveExists || pathExists(p.LiveDBPath+"-wal") || pathExists(p.LiveDBPath+"-shm")
	if needMove {
		token, err := restoreToken()
		if err != nil {
			return restoreResult{}, err
		}
		bakBase = p.LiveDBPath + ".bak-" + stamp + "-" + token
		for _, sc := range []string{"", "-wal", "-shm"} {
			src := p.LiveDBPath + sc
			if !pathExists(src) {
				continue
			}
			dst := bakBase + sc
			if err := moveRename(src, dst); err != nil {
				// Roll back anything already moved before we bail. A rollback that
				// itself FAILS is surfaced LOUDLY (the live db may now be missing).
				if rbErr := rollbackMoves(moveRename, movedAside); rbErr != nil {
					return restoreResult{}, fmt.Errorf("move current %s aside failed (%v) AND rollback FAILED — live db may be missing: %w", src, err, rbErr)
				}
				return restoreResult{}, fmt.Errorf("move current %s aside: %w", src, err)
			}
			movedAside = append(movedAside, [2]string{src, dst})
		}
	}

	// 5d. Atomic move into place LAST. On failure, ROLL BACK the moved-aside set so
	// the live db is never left missing; a rollback that itself FAILS is LOUD.
	if err := rename(staged, p.LiveDBPath); err != nil {
		if rbErr := rollbackMoves(moveRename, movedAside); rbErr != nil {
			return restoreResult{}, fmt.Errorf("swap failed (%v) AND rollback FAILED — live db may be missing: %w", err, rbErr)
		}
		return restoreResult{}, fmt.Errorf("swap restored db into place: %w", err)
	}

	// 6. Recount live memories for the report. The FTS index travels INSIDE
	// memory.db (content table, copied by VACUUM INTO) — no rebuild needed.
	rowCount, err := countLiveRows(p.LiveDBPath)
	if err != nil {
		return restoreResult{}, err
	}

	return restoreResult{
		LivePath:   p.LiveDBPath,
		RowCount:   rowCount,
		BackupPath: bakBase,
	}, nil
}

// rollbackMoves undoes a set of [from,to] renames (in reverse), restoring the
// previous db + sidecars to their original paths. It attempts EVERY entry and
// returns a joined error if ANY rename back fails — a failed rollback means the
// live db may be left missing, so the caller must surface it loudly rather than
// silently continue.
func rollbackMoves(rename func(oldpath, newpath string) error, moves [][2]string) error {
	var errs []error
	for i := len(moves) - 1; i >= 0; i-- {
		if err := rename(moves[i][1], moves[i][0]); err != nil {
			errs = append(errs, fmt.Errorf("restore %s: %w", moves[i][0], err))
		}
	}
	return errors.Join(errs...)
}

// verifyArchivedUsable opens the archived db read-only and confirms it carries
// the memory store schema (the `memories` + `memories_fts` tables). A db can
// pass integrity_check + a version gate while missing these tables entirely
// (e.g. an empty or unrelated sqlite file), which would swap in an unusable live
// db that only fails LATER at countLiveRows — after the live db was clobbered.
func verifyArchivedUsable(path string) error {
	db, err := sql.Open("sqlite", readOnlyDSN(path))
	if err != nil {
		return fmt.Errorf("open archived db: %w", err)
	}
	defer db.Close()
	for _, tbl := range []string{"memories", "memories_fts"} {
		var name string
		if err := db.QueryRow(
			"SELECT name FROM sqlite_master WHERE type='table' AND name = ?", tbl,
		).Scan(&name); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("archived db is not a usable memory store: missing %q table", tbl)
			}
			return fmt.Errorf("archived db is not a usable memory store (checking %q table): %w", tbl, err)
		}
	}
	return nil
}

// stageRestoredDB copies archivedDB into a UNIQUE temp file in destDir (via
// os.CreateTemp, so the name is unpredictable) and returns its path. The final
// rename of this file into place is same-filesystem and atomic.
func stageRestoredDB(destDir, archivedDB string) (string, error) {
	tmp, err := os.CreateTemp(destDir, ".memory-restore-*.tmp")
	if err != nil {
		return "", err
	}
	staged := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		_ = os.Remove(staged)
		return "", err
	}
	in, err := os.Open(archivedDB)
	if err != nil {
		tmp.Close()
		_ = os.Remove(staged)
		return "", err
	}
	defer in.Close()
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		_ = os.Remove(staged)
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		_ = os.Remove(staged)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(staged)
		return "", err
	}
	return staged, nil
}

// restoreToken returns a short random hex token so the .bak set name is unique
// even within the same second.
func restoreToken() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("random token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// pathExists reports whether a path exists (file, dir, or otherwise). Unlike
// fileExists it does not require a regular file — a sidecar check only cares that
// something is there to move.
func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// extractTarGz unpacks a tar.gz into destDir. It is defensive about path
// traversal (an entry escaping destDir is rejected) AND about resource
// exhaustion: a tar bomb is capped by entry count, per-file size, and total
// extracted bytes, so an untrusted archive cannot fill the disk.
func extractTarGz(archivePath, destDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var totalBytes int64
	entries := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue // the backup only writes regular files
		}
		entries++
		if entries > restoreMaxEntryCount {
			return fmt.Errorf("archive has too many entries (> %d); refusing possible tar bomb", restoreMaxEntryCount)
		}
		clean := filepath.Clean(hdr.Name)
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			return fmt.Errorf("archive entry escapes dest: %q", hdr.Name)
		}
		target := filepath.Join(destDir, clean)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		// Cap this entry AND the running total. LimitReader lets us detect an
		// oversize entry by reading one byte past the cap.
		remainingTotal := restoreMaxTotalBytes - totalBytes
		entryCap := int64(restoreMaxEntryBytes)
		if remainingTotal < entryCap {
			entryCap = remainingTotal
		}
		n, err := io.Copy(out, io.LimitReader(tr, entryCap+1))
		if err != nil {
			out.Close()
			return fmt.Errorf("extract %s: %w", hdr.Name, err)
		}
		if err := out.Close(); err != nil {
			return err
		}
		if n > int64(restoreMaxEntryBytes) {
			return fmt.Errorf("archive entry %q exceeds per-file limit (%d bytes); refusing possible tar bomb", hdr.Name, restoreMaxEntryBytes)
		}
		totalBytes += n
		if totalBytes > int64(restoreMaxTotalBytes) {
			return fmt.Errorf("archive exceeds total size limit (%d bytes); refusing possible tar bomb", restoreMaxTotalBytes)
		}
	}
	return nil
}

// readRestoreManifest reads + parses manifest.json from an extracted archive.
func readRestoreManifest(path string) (backupManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return backupManifest{}, fmt.Errorf("read manifest: %w", err)
	}
	var m backupManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return backupManifest{}, fmt.Errorf("parse manifest: %w", err)
	}
	return m, nil
}

// integrityCheckDB opens a db standalone and refuses it unless both a fast
// quick_check and the full integrity_check report "ok". Never trust a backup
// blindly.
func integrityCheckDB(path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return fmt.Errorf("open archived db: %w", err)
	}
	defer db.Close()

	var quick string
	if err := db.QueryRow("PRAGMA quick_check").Scan(&quick); err != nil {
		return fmt.Errorf("quick_check: %w", err)
	}
	if quick != "ok" {
		return fmt.Errorf("archived db failed quick_check: %s", quick)
	}
	var integrity string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		return fmt.Errorf("integrity_check: %w", err)
	}
	if integrity != "ok" {
		return fmt.Errorf("archived db failed integrity_check: %s", integrity)
	}
	return nil
}

// dbUserVersion opens a db standalone (read-only) and returns its actual
// PRAGMA user_version. Used to cross-check the archived db against the manifest
// so a forged manifest cannot smuggle a forward-incompatible db past the gate.
func dbUserVersion(path string) (int, error) {
	db, err := sql.Open("sqlite", readOnlyDSN(path))
	if err != nil {
		return 0, fmt.Errorf("open db: %w", err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("user_version: %w", err)
	}
	return v, nil
}

// countLiveRows opens the restored db and returns the count of live (non-deleted)
// memories, for the report. It does NOT rebuild FTS: memories_fts is a standalone
// FTS5 content table stored INSIDE memory.db, so VACUUM INTO already carried its
// content into the snapshot — a 'rebuild' would be a no-op-shaped lie.
func countLiveRows(path string) (int, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return 0, fmt.Errorf("open restored db: %w", err)
	}
	defer db.Close()
	if _, err := db.Exec("PRAGMA busy_timeout=10000;"); err != nil {
		return 0, fmt.Errorf("busy_timeout: %w", err)
	}
	var rowCount int
	if err := db.QueryRow("SELECT count(*) FROM memories WHERE deleted_at IS NULL").Scan(&rowCount); err != nil {
		return 0, fmt.Errorf("row count: %w", err)
	}
	return rowCount, nil
}

// serveIsUp is the default ServeProbe: it dials the memory daemon's address
// (127.0.0.1:MEMORY_PORT, honoring MEMORY_BIND) with a short timeout. A
// successful connect means serve is holding the db.
func serveIsUp() bool {
	addr := env("MEMORY_BIND", "127.0.0.1") + ":" + env("MEMORY_PORT", "11435")
	conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// --- CLI wiring --------------------------------------------------------------

const memoryRestoreUsage = `usage: pi-stack-host memory restore <archive> [--force]

  Restore ~/.pi-stack/memory/memory.db (honors MEMORY_DB) from a backup tar.gz
  produced by 'memory backup'. Refuses to run while 'serve' holds the db, and
  refuses to overwrite an existing live db unless --force is given (in which
  case the current db is moved aside to a .bak-<ts> first, never deleted).

  <archive>    path to the pi-stack-backup-<ts>.tar.gz to restore
  --force      overwrite an existing live db (current db kept as .bak-<ts>)`

func runMemoryRestoreCLI(args []string) {
	archive := ""
	force := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-h" || a == "--help":
			fmt.Println(memoryRestoreUsage)
			return
		case a == "--force" || a == "-f":
			force = true
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "pi-stack-host memory restore: unknown flag %q\n%s\n", a, memoryRestoreUsage)
			os.Exit(2)
		default:
			if archive != "" {
				fmt.Fprintf(os.Stderr, "pi-stack-host memory restore: unexpected argument %q\n%s\n", a, memoryRestoreUsage)
				os.Exit(2)
			}
			archive = a
		}
	}
	if archive == "" {
		fmt.Fprintf(os.Stderr, "pi-stack-host memory restore: missing <archive>\n%s\n", memoryRestoreUsage)
		os.Exit(2)
	}

	res, err := memoryRestore(resolveRestoreParams(archive, force, time.Now()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack-host memory restore: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("restored %d rows to %s\n", res.RowCount, res.LivePath)
	if res.BackupPath != "" {
		fmt.Printf("previous db kept at %s\n", res.BackupPath)
	}
	fmt.Println("start it: pi-stack serve")
}

// resolveRestoreParams fills restoreParams from the environment/home, so
// memoryRestore itself stays hermetic.
func resolveRestoreParams(archive string, force bool, now time.Time) restoreParams {
	home, _ := os.UserHomeDir()

	dbPath := strings.TrimSpace(os.Getenv("MEMORY_DB"))
	if dbPath == "" {
		dbPath = filepath.Join(home, ".pi-stack", "memory", "memory.db")
	}

	return restoreParams{
		ArchivePath: archive,
		LiveDBPath:  dbPath,
		Force:       force,
		Now:         now,
		ServeProbe:  serveIsUp,
	}
}
