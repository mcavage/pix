// snapshot.go is the store's data-safety primitives: memory_snapshot and
// memory_restore. Ported from services/host/memory_snapshot.go (pix-v2 U2),
// adapted for a single in-process Store instead of a separate stopped-daemon
// CLI: Restore holds Store.mu for its whole duration (the same lock every
// recall/remember serializes through) instead of a cross-process flock,
// because this service has exactly one process and exactly one Store.
//
// ONE artifact: a snapshot is a plain sqlite file written with `VACUUM INTO`
// against a READ-ONLY handle on the live db (the -wal/-shm sidecars are
// folded in), safe to take while the store is serving requests.
//
// Restore's ordering is the mechanism: validate the snapshot, move the
// current db + sidecars aside to a KEPT .bak set, rename the staged copy
// into place LAST, then reopen. Nothing fallible follows that rename; an
// earlier failure rolls the .bak set back, loudly if the rollback itself
// fails.
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// SnapshotResult reports what Snapshot wrote.
type SnapshotResult struct {
	Path        string
	Rows        int
	Size        int64
	UserVersion int
}

// Snapshot writes a hot, verified snapshot of this Store's live db to
// outPath. It never mutates the source and never clobbers: outPath must not
// exist and must not be the live db.
func (s *Store) Snapshot(outPath string) (SnapshotResult, error) {
	dbPath := s.path
	if dbPath == ":memory:" {
		return SnapshotResult{}, fmt.Errorf("cannot snapshot an in-memory store")
	}
	if resolvePath(outPath) == resolvePath(dbPath) {
		return SnapshotResult{}, fmt.Errorf("%s is the live database; choose another path", outPath)
	}
	if pathExists(outPath) {
		return SnapshotResult{}, fmt.Errorf("refusing to overwrite existing %s; choose another path or remove it first", outPath)
	}
	dir := filepath.Dir(outPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return SnapshotResult{}, fmt.Errorf("snapshot dir: %w", err)
	}
	staged, _, err := stagePath(dir, ".pix-snapshot-")
	if err != nil {
		return SnapshotResult{}, err
	}
	defer os.Remove(staged)
	if err := vacuumInto(dbPath, staged); err != nil {
		return SnapshotResult{}, err
	}
	if err := os.Chmod(staged, 0o600); err != nil {
		return SnapshotResult{}, fmt.Errorf("chmod snapshot: %w", err)
	}
	userVersion, rows, err := verifyMemoryDB(staged)
	if err != nil {
		return SnapshotResult{}, err
	}
	if err := os.Link(staged, outPath); err != nil {
		return SnapshotResult{}, fmt.Errorf("finalize snapshot (refusing to clobber %s): %w", outPath, err)
	}
	fi, err := os.Stat(outPath)
	if err != nil {
		return SnapshotResult{}, err
	}
	return SnapshotResult{Path: outPath, Rows: rows, Size: fi.Size(), UserVersion: userVersion}, nil
}

// RestoreResult reports what Restore did.
type RestoreResult struct {
	LivePath   string
	Rows       int
	BackupPath string
}

// Restore installs snapshotPath as this Store's live db, in place, with
// Store.mu held for the whole operation (blocking every concurrent
// recall/remember/forget until it completes). force is required whenever a
// live db already exists, which in practice is always: a fresh store's
// journal_mode=WAL PRAGMA already creates the file on Open.
func (s *Store) Restore(snapshotPath string, force bool) (RestoreResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	liveDBPath := s.path
	if liveDBPath == ":memory:" {
		return RestoreResult{}, fmt.Errorf("cannot restore into an in-memory store")
	}

	_, rows, err := verifyMemoryDB(snapshotPath)
	if err != nil {
		return RestoreResult{}, err
	}
	if fileExists(liveDBPath) && !force {
		return RestoreResult{}, fmt.Errorf("live db already exists at %s; pass force=true to overwrite (the current db is moved aside to a .bak first)", liveDBPath)
	}

	destDir := filepath.Dir(liveDBPath)
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return RestoreResult{}, fmt.Errorf("dest dir: %w", err)
	}
	staged, token, err := stagePath(destDir, ".pix-restore-")
	if err != nil {
		return RestoreResult{}, err
	}
	defer os.Remove(staged)
	if err := vacuumInto(snapshotPath, staged); err != nil {
		return RestoreResult{}, fmt.Errorf("stage snapshot: %w", err)
	}
	if err := os.Chmod(staged, 0o600); err != nil {
		return RestoreResult{}, fmt.Errorf("chmod staged db: %w", err)
	}

	// Close the live handle before touching its files: sqlite/WAL keeps open
	// fds against the current inode, and this Store's *sql.DB is about to
	// point at a swapped-in file.
	if err := s.db.Close(); err != nil {
		return RestoreResult{}, fmt.Errorf("close live db before restore: %w", err)
	}

	now := time.Now()
	bakBase := liveDBPath + ".bak-" + now.Format("20060102-150405") + "-" + token
	var moved [][2]string
	reopenOrDie := func(failure error) (RestoreResult, error) {
		db, reopenErr := openDB(liveDBPath)
		if reopenErr != nil {
			return RestoreResult{}, fmt.Errorf("%v; AND reopen after rollback FAILED: %w", failure, reopenErr)
		}
		s.db = db
		return RestoreResult{}, failure
	}
	for _, sc := range []string{"", "-wal", "-shm"} {
		src := liveDBPath + sc
		if !pathExists(src) {
			continue
		}
		if err := os.Rename(src, bakBase+sc); err != nil {
			return reopenOrDie(rollback(moved, fmt.Errorf("move current %s aside: %w", src, err)))
		}
		moved = append(moved, [2]string{src, bakBase + sc})
	}
	if len(moved) == 0 {
		bakBase = ""
	}
	if err := os.Rename(staged, liveDBPath); err != nil {
		return reopenOrDie(rollback(moved, fmt.Errorf("swap restored db into place: %w", err)))
	}

	db, err := openDB(liveDBPath)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("restored db in place but failed to reopen it: %w", err)
	}
	s.db = db
	return RestoreResult{LivePath: liveDBPath, Rows: rows, BackupPath: bakBase}, nil
}

// rollback undoes a set of [from,to] renames in reverse and folds the
// outcome into the original failure.
func rollback(moves [][2]string, cause error) error {
	var errs []error
	for i := len(moves) - 1; i >= 0; i-- {
		if err := os.Rename(moves[i][1], moves[i][0]); err != nil {
			errs = append(errs, fmt.Errorf("restore %s: %w", moves[i][0], err))
		}
	}
	if rbErr := errors.Join(errs...); rbErr != nil {
		return fmt.Errorf("%v; AND rollback FAILED — the live db may be missing: %w", cause, rbErr)
	}
	return cause
}

func stagePath(dir, prefix string) (path, token string, err error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", fmt.Errorf("random token: %w", err)
	}
	token = hex.EncodeToString(b[:])
	return filepath.Join(dir, prefix+token+".tmp"), token, nil
}

func vacuumInto(srcPath, dst string) error {
	db, err := sql.Open("sqlite", readOnlyDSN(srcPath))
	if err != nil {
		return fmt.Errorf("open source db: %w", err)
	}
	defer db.Close()
	if _, err := db.Exec("PRAGMA busy_timeout=10000;"); err != nil {
		return fmt.Errorf("busy_timeout: %w", err)
	}
	if _, err := db.Exec("VACUUM INTO ?", dst); err != nil {
		return fmt.Errorf("VACUUM INTO snapshot: %w", err)
	}
	return nil
}

// verifyMemoryDB opens path READ-ONLY and answers "is this a memory store I
// can trust", returning its schema version and live-row count.
func verifyMemoryDB(path string) (userVersion, rows int, err error) {
	if !fileExists(path) {
		return 0, 0, fmt.Errorf("snapshot %s does not exist", path)
	}
	db, err := sql.Open("sqlite", readOnlyDSN(path))
	if err != nil {
		return 0, 0, fmt.Errorf("open db: %w", err)
	}
	defer db.Close()

	var integrity string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		return 0, 0, fmt.Errorf("integrity_check: %w", err)
	}
	if integrity != "ok" {
		return 0, 0, fmt.Errorf("db failed integrity_check: %s", integrity)
	}
	if err := db.QueryRow("PRAGMA user_version").Scan(&userVersion); err != nil {
		return 0, 0, fmt.Errorf("user_version: %w", err)
	}
	if userVersion > schemaVersion {
		return 0, 0, fmt.Errorf("db schema version %d is newer than this binary's (%d); upgrade pix-memory first",
			userVersion, schemaVersion)
	}
	if err := db.QueryRow("SELECT count(*) FROM memories WHERE deleted_at IS NULL").Scan(&rows); err != nil {
		return 0, 0, fmt.Errorf("not a usable memory store (live-row count failed): %w", err)
	}
	var id, kind, content, durability, project any
	if err := db.QueryRow("SELECT id, kind, content, durability, project FROM memories LIMIT 1").
		Scan(&id, &kind, &content, &durability, &project); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, 0, fmt.Errorf("not a usable memory store (memories schema mismatch): %w", err)
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM memories_fts").Scan(&n); err != nil {
		return 0, 0, fmt.Errorf("not a usable memory store (memories_fts unusable): %w", err)
	}
	return userVersion, rows, nil
}

func readOnlyDSN(path string) string {
	if strings.Contains(path, "?") {
		return path + "&mode=ro"
	}
	return path + "?mode=ro"
}

func resolvePath(p string) string {
	if p == "" {
		return ""
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = filepath.Clean(p)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
