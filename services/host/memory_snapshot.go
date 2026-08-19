// pix-host `memory snapshot` / `memory restore` — the memory store's data safety
// primitives, and deliberately the whole of them. They live here rather than in
// the dependency-light launcher because they need sqlite.
//
// ONE artifact: a snapshot is a plain sqlite file written with `VACUUM INTO`
// against a READ-ONLY handle on the live db — a consistent single file (the
// -wal/-shm sidecars are folded in), safe to take while `serve` holds the store.
// memory.db is the only unreproducible piece of pix state, so nothing else rides
// along: config.toml is reproducible with `pix config set`, and op-refs.env holds
// only op:// pointers.
//
// Restore is the STOPPED-SERVICE primitive, and its ordering is the mechanism:
// take the advisory flock the daemon holds FIRST and keep it across the whole
// commit — that lock, not a port probe, is the authority, because the daemon
// opens the db before it binds. Under it: validate, move the current db +
// sidecars aside to a KEPT .bak set, rename the staged copy into place LAST.
// Nothing fallible follows that rename; an earlier failure rolls the .bak set
// back, loudly if the rollback itself fails.

package main

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

	"pix/host/config"

	_ "modernc.org/sqlite"
)

// memSnapshotSchemaVersion is the memory schema (PRAGMA user_version) this
// binary understands; newMemStore stamps the same value (memSchemaVersion,
// memory.go — the two are the SAME number, not merely kept in sync by hand).
// A db claiming a newer one came from a newer pix and is refused.
const memSnapshotSchemaVersion = memSchemaVersion

type snapshotResult struct {
	Path        string
	Rows        int
	Size        int64
	UserVersion int
}

// memorySnapshot writes a hot, verified snapshot of dbPath to outPath. It never
// mutates the source, never stops a running serve, and never clobbers: the dest
// must not exist, must not BE the live db, and is linked in only once verified.
func memorySnapshot(dbPath, outPath string) (snapshotResult, error) {
	if resolvePath(outPath) == resolvePath(dbPath) {
		return snapshotResult{}, fmt.Errorf("%s is the live database; choose another path", outPath)
	}
	if pathExists(outPath) {
		return snapshotResult{}, fmt.Errorf("refusing to overwrite existing %s; choose another path or remove it first", outPath)
	}
	dir := filepath.Dir(outPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return snapshotResult{}, fmt.Errorf("snapshot dir: %w", err)
	}
	// Stage in the DEST dir so the commit is same-filesystem; the random token keeps
	// the name free (VACUUM INTO refuses an existing target).
	staged, _, err := stagePath(dir, ".pix-snapshot-")
	if err != nil {
		return snapshotResult{}, err
	}
	defer os.Remove(staged) // no-op after the link; cleanup on any failure
	if err := vacuumInto(dbPath, staged); err != nil {
		return snapshotResult{}, err
	}
	// sqlite honors umask, and a copy of a 0600 store carries the same facts — so
	// the snapshot gets the store's mode, not whatever umask allowed.
	if err := os.Chmod(staged, 0o600); err != nil {
		return snapshotResult{}, fmt.Errorf("chmod snapshot: %w", err)
	}
	userVersion, rows, err := verifyMemoryDB(staged)
	if err != nil {
		return snapshotResult{}, err
	}
	// Hard-link, not rename: os.Link fails EEXIST, which is the atomic no-clobber
	// commit rename lacks on POSIX, closing the window after the check above.
	if err := os.Link(staged, outPath); err != nil {
		return snapshotResult{}, fmt.Errorf("finalize snapshot (refusing to clobber %s): %w", outPath, err)
	}
	fi, err := os.Stat(outPath)
	if err != nil {
		return snapshotResult{}, err
	}
	return snapshotResult{Path: outPath, Rows: rows, Size: fi.Size(), UserVersion: userVersion}, nil
}

// restoreParams are the fully-resolved inputs to the restore core, keeping it
// hermetic (no env/home lookups inside).
type restoreParams struct {
	SnapshotPath string // source snapshot (a plain memory.db)
	LiveDBPath   string // dest live memory.db to swap in
	Force        bool   // overwrite an existing live db
	LockPath     string // shared store flock ("" -> <dir of LiveDBPath>/.memory.lock)
	Now          time.Time

	// Test seams: acquireLockFn (nil -> acquireLock) stubs a held lock or races a
	// db in at the acquire seam; renameFn (nil -> os.Rename) performs the swap and
	// move-aside/rollback renames, so a failure can prove the previous db returns.
	acquireLockFn func(path string) (func(), error)
	renameFn      func(oldpath, newpath string) error
}

type restoreResult struct {
	LivePath   string // where the restored db now lives
	Rows       int    // live memories in the restored db
	BackupPath string // .bak base path of the previous db ("" if none existed)
}

// memoryRestore installs a snapshot as the live memory db, with the service
// stopped (see the file header for the lock contract).
func memoryRestore(p restoreParams) (restoreResult, error) {
	now := p.Now
	if now.IsZero() {
		now = time.Now()
	}
	rename := p.renameFn
	if rename == nil {
		rename = os.Rename
	}
	acquire := p.acquireLockFn
	if acquire == nil {
		acquire = acquireLock
	}
	lockPath := p.LockPath
	if lockPath == "" {
		lockPath = filepath.Join(filepath.Dir(p.LiveDBPath), ".memory.lock")
	}
	release, err := acquire(lockPath)
	if err != nil {
		return restoreResult{}, fmt.Errorf("the memory service (or another restore) is using the database — stop it first: pix serve stop")
	}
	defer release()

	// Validate BEFORE touching live state: a valid-but-unrelated sqlite file
	// passes integrity_check and would otherwise land as an unusable live db.
	_, rows, err := verifyMemoryDB(p.SnapshotPath)
	if err != nil {
		return restoreResult{}, err
	}
	if fileExists(p.LiveDBPath) && !p.Force {
		return restoreResult{}, fmt.Errorf("live db already exists at %s; pass --force to overwrite (the current db is moved aside to a .bak first)", p.LiveDBPath)
	}
	destDir := filepath.Dir(p.LiveDBPath)
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return restoreResult{}, fmt.Errorf("dest dir: %w", err)
	}
	// Stage with VACUUM INTO, not a byte copy: same-filesystem (so the final rename
	// is atomic), and it re-reads the snapshot through sqlite once more.
	staged, token, err := stagePath(destDir, ".pix-restore-")
	if err != nil {
		return restoreResult{}, err
	}
	defer os.Remove(staged)
	if err := vacuumInto(p.SnapshotPath, staged); err != nil {
		return restoreResult{}, fmt.Errorf("stage snapshot: %w", err)
	}
	if err := os.Chmod(staged, 0o600); err != nil {
		return restoreResult{}, fmt.Errorf("chmod staged db: %w", err)
	}

	// Move the CURRENT state aside COMPLETELY (db + -wal + -shm) into a unique kept
	// .bak set. This runs even when the main db is absent: a prior failed run can
	// leave an ORPHAN sidecar, and installing beside a stale WAL replays it.
	bakBase := p.LiveDBPath + ".bak-" + now.Format("20060102-150405") + "-" + token
	var moved [][2]string
	for _, sc := range []string{"", "-wal", "-shm"} {
		src := p.LiveDBPath + sc
		if !pathExists(src) {
			continue
		}
		if err := rename(src, bakBase+sc); err != nil {
			return restoreResult{}, rollback(rename, moved, fmt.Errorf("move current %s aside: %w", src, err))
		}
		moved = append(moved, [2]string{src, bakBase + sc})
	}
	if len(moved) == 0 {
		bakBase = ""
	}
	// Atomic move into place LAST. Nothing fallible follows: the row count came from
	// the snapshot, and the FTS index travels INSIDE the db, so nothing is rebuilt.
	if err := rename(staged, p.LiveDBPath); err != nil {
		return restoreResult{}, rollback(rename, moved, fmt.Errorf("swap restored db into place: %w", err))
	}
	return restoreResult{LivePath: p.LiveDBPath, Rows: rows, BackupPath: bakBase}, nil
}

// rollback undoes a set of [from,to] renames in reverse and folds the outcome
// into the original failure. A rollback that itself fails can leave the live db
// MISSING, so it is surfaced loudly rather than swallowed.
func rollback(rename func(oldpath, newpath string) error, moves [][2]string, cause error) error {
	var errs []error
	for i := len(moves) - 1; i >= 0; i-- {
		if err := rename(moves[i][1], moves[i][0]); err != nil {
			errs = append(errs, fmt.Errorf("restore %s: %w", moves[i][0], err))
		}
	}
	if rbErr := errors.Join(errs...); rbErr != nil {
		return fmt.Errorf("%v; AND rollback FAILED — the live db may be missing: %w", cause, rbErr)
	}
	return cause
}

// stagePath returns an unpredictable, NOT-created temp path in dir (VACUUM INTO
// wants a free name) plus its token, reused for the .bak set.
func stagePath(dir, prefix string) (path, token string, err error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", fmt.Errorf("random token: %w", err)
	}
	token = hex.EncodeToString(b[:])
	return filepath.Join(dir, prefix+token+".tmp"), token, nil
}

// vacuumInto opens src READ-ONLY and writes a consistent single-file copy to
// dst via `VACUUM INTO` (which refuses an existing dst). A busy_timeout waits
// briefly for a concurrent writer instead of failing immediately.
func vacuumInto(srcPath, dst string) error {
	db, err := sql.Open("sqlite", readOnlyDSN(srcPath))
	if err != nil {
		return fmt.Errorf("open source db: %w", err)
	}
	defer db.Close()
	if _, err := db.Exec("PRAGMA busy_timeout=10000;"); err != nil {
		return fmt.Errorf("busy_timeout: %w", err)
	}
	// A bound parameter avoids quote-escaping the destination path.
	if _, err := db.Exec("VACUUM INTO ?", dst); err != nil {
		return fmt.Errorf("VACUUM INTO snapshot: %w", err)
	}
	return nil
}

// verifyMemoryDB opens path READ-ONLY and answers "is this a memory store I can
// trust", returning its schema version and live-row count. Past integrity and the
// version gate it runs the ACTUAL queries the app relies on: a db with a table
// merely NAMED `memories` passes integrity_check but is not a store, and must be
// refused before it can become live.
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
	if userVersion > memSnapshotSchemaVersion {
		return 0, 0, fmt.Errorf("db schema version %d is newer than this binary's (%d); upgrade pix first",
			userVersion, memSnapshotSchemaVersion)
	}
	// Exercises deleted_at, and is the count we report.
	if err := db.QueryRow("SELECT count(*) FROM memories WHERE deleted_at IS NULL").Scan(&rows); err != nil {
		return 0, 0, fmt.Errorf("not a usable memory store (live-row count failed): %w", err)
	}
	// Exercises the columns recall reads. LIMIT 1 tolerates an empty store.
	var id, kind, content, durability, project any
	if err := db.QueryRow("SELECT id, kind, content, durability, project FROM memories LIMIT 1").
		Scan(&id, &kind, &content, &durability, &project); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, 0, fmt.Errorf("not a usable memory store (memories schema mismatch): %w", err)
	}
	// A missing/wrong FTS index would break keyword recall after a restore.
	var n int
	if err := db.QueryRow("SELECT count(*) FROM memories_fts").Scan(&n); err != nil {
		return 0, 0, fmt.Errorf("not a usable memory store (memories_fts unusable): %w", err)
	}
	return userVersion, rows, nil
}

// readOnlyDSN builds a modernc-sqlite DSN that opens READ-ONLY, so reading a db
// can never create or mutate it.
func readOnlyDSN(path string) string {
	if strings.Contains(path, "?") {
		return path + "&mode=ro"
	}
	return path + "?mode=ro"
}

// resolvePath makes a best-effort absolute, symlink-resolved form for
// comparison. It never fails: on any error it falls back to filepath.Clean.
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

// pathExists reports whether ANYTHING is there (file, dir, dangling symlink) —
// a sidecar check only cares that something must be moved.
func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// --- CLI wiring --------------------------------------------------------------

const memoryHostUsage = `usage: pix-host memory [<subcommand>]

  (no subcommand)          run the memory daemon (:11435, JSON-RPC)
  snapshot PATH            write a verified copy of memory.db to PATH; safe
                           while the daemon is running (honors MEMORY_DB)
  restore PATH [--force]   install a snapshot as the live memory.db. The
                           service must be STOPPED (pix serve stop); the
                           current db is kept as a .bak-<ts>`

// runMemoryHost dispatches `pix-host memory`. No args runs the daemon (the
// service entry point); an UNKNOWN token is a typo, not a silent daemon start.
func runMemoryHost(args []string) {
	switch {
	case len(args) == 0:
		runMemory()
	case args[0] == "-h" || args[0] == "--help":
		fmt.Println(memoryHostUsage)
	case args[0] == "snapshot":
		path, _, ok := snapshotArgs("snapshot", args[1:])
		if !ok {
			return
		}
		res, err := memorySnapshot(config.MemoryDBPath(), path)
		if err != nil {
			memoryCLIFatal("snapshot", err)
		}
		fmt.Printf("snapshot: %s (%d rows, %d bytes, schema v%d)\n", res.Path, res.Rows, res.Size, res.UserVersion)
	case args[0] == "restore":
		path, force, ok := snapshotArgs("restore", args[1:])
		if !ok {
			return
		}
		res, err := memoryRestore(restoreParams{SnapshotPath: path, LiveDBPath: config.MemoryDBPath(),
			Force: force, LockPath: config.MemoryLockPath(), Now: time.Now()})
		if err != nil {
			memoryCLIFatal("restore", err)
		}
		fmt.Printf("restored %d memory rows to %s\n", res.Rows, res.LivePath)
		if res.BackupPath != "" {
			fmt.Printf("previous db kept at %s\n", res.BackupPath)
		}
		fmt.Println("start it: pix serve")
	default:
		fmt.Fprintf(os.Stderr, "pix-host memory: unknown subcommand %q\n%s\n", args[0], memoryHostUsage)
		os.Exit(2)
	}
}

// memoryCLIFatal reports a failed subcommand and exits 1.
func memoryCLIFatal(sub string, err error) {
	fmt.Fprintf(os.Stderr, "pix-host memory %s: %v\n", sub, err)
	os.Exit(1)
}

// snapshotArgs parses the shared `<sub> PATH [--force]` argv: -h/--help prints
// usage and reports ok=false; anything missing, duplicated or unknown is a usage
// error, exit 2.
func snapshotArgs(sub string, args []string) (path string, force, ok bool) {
	for _, a := range args {
		switch {
		case a == "-h" || a == "--help":
			fmt.Println(memoryHostUsage)
			return "", false, false
		case sub == "restore" && (a == "--force" || a == "-f"):
			force = true
		case strings.HasPrefix(a, "-") || path != "":
			fmt.Fprintf(os.Stderr, "pix-host memory %s: unexpected argument %q\n%s\n", sub, a, memoryHostUsage)
			os.Exit(2)
		default:
			path = a
		}
	}
	if path == "" {
		fmt.Fprintf(os.Stderr, "pix-host memory %s: missing PATH\n%s\n", sub, memoryHostUsage)
		os.Exit(2)
	}
	return path, force, true
}
