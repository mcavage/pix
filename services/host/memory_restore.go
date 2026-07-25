// pix-host `restore` — the safety-critical counterpart to `backup`
// (memory_backup.go). It takes a backup tar.gz and restores the FULL pix
// state WITHOUT ever corrupting or silently clobbering what is already there:
//
//   - MEMORY (the safety-critical part): swaps the archived memory.db in as the
//     live database via the hardened flow below.
//   - CONFIG + OP-REFS (plain files): moves the CURRENT config.toml / op-refs.env
//     aside to a unique .bak-<ts> (reversible) then writes the archived versions
//     into place. This is how profiles come back. Absent-in-archive = skipped.
//
// Only the memory swap carries the corruption risk (a live sqlite writer), so
// the heavy machinery below is about MEMORY; the config/op-refs restore is a
// simple reversible move-aside.
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
// This lives in pix-host (it needs the sqlite driver); the launcher
// `pix memory restore` execs it. The core is split into pure/logic steps
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

	"pix/host/config"

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
// written by a newer pix and cannot be safely restored here.
const restoreSchemaVersion = 1

// restoreParams are the fully-resolved inputs to the restore core, so it stays
// hermetic and testable (no env/home lookups, no real socket dial inside).
type restoreParams struct {
	ArchivePath string      // source backup tar.gz
	LiveDBPath  string      // dest live memory.db to swap in
	Force       bool        // overwrite an existing live db
	ConfigPath  string      // dest config.toml to restore ("" -> skip config restore)
	OpRefsPath  string      // dest op-refs.env to restore ("" -> skip op-refs restore)
	Now         time.Time   // timestamp source for .bak/.restore names (zero -> time.Now())
	ServeProbe  func() bool // reports whether a serve daemon holds the db (nil -> assume down)
	// LockPath is the shared advisory flock both this restore and the memory daemon
	// take to be mutually exclusive around the store (config.MemoryLockPath()).
	// Empty -> derived from LiveDBPath's dir (<dir>/.memory.lock), the SAME path
	// the daemon resolves. This lock — not the port probe — is the authority.
	LockPath string
	// acquireLockFn takes the LockPath (nil -> acquireLock). Tests inject a stub to
	// simulate a held lock without a second process.
	acquireLockFn func(path string) (func(), error)
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

	// Config + op-refs restore outcome (plain-file move-aside). Restored is false
	// when the archive did not carry that file; Bak is "" when there was no current
	// file to move aside.
	ConfigRestored bool
	ConfigBak      string
	OpRefsRestored bool
	OpRefsBak      string

	// Profiles present in the RESTORED config (parsed back after the write), so the
	// CLI can report exactly which contexts came back. Empty for a config-less
	// archive.
	Profiles []string

	// Knowledge notes copied from the manifest: where each configured bundle lived
	// (path + git remote). Content is NOT restored (git is its backup); the CLI
	// prints these so the user knows how to bring the bundle back.
	Knowledge []knowledgeNote
}

// validateRestoreManifest is the PURE version gate: the archive must be a format
// this binary understands and must NOT carry a schema newer than ours. Kept free
// of any fs/db work so tests can exercise the gate directly.
func validateRestoreManifest(m backupManifest) error {
	// Accept any format from v1 up to the version this binary writes. A v1 archive
	// (memory-only manifest, no profiles/knowledge) still restores memory (and its
	// config/op-refs) fine — the new fields simply default to empty. Only a format
	// NEWER than ours (written by a newer pix) is refused.
	if m.FormatVersion < 1 || m.FormatVersion > backupFormatVersion {
		return fmt.Errorf("archive format_version %d is not understood (this binary handles 1..%d)",
			m.FormatVersion, backupFormatVersion)
	}
	if m.SqliteUserVersion > restoreSchemaVersion {
		return fmt.Errorf("archive sqlite schema version %d is newer than this binary's (%d); upgrade pix before restoring",
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
		return restoreResult{}, fmt.Errorf("memory serve is running; stop it first: pix serve stop")
	}

	// 2a. Extract the archive to a private temp dir (with tar-bomb guards) and read
	// the manifest.
	tmpDir, err := os.MkdirTemp("", "pix-restore-")
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
		return restoreResult{}, fmt.Errorf("archived db schema version %d is newer than this binary's (%d); upgrade pix before restoring",
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

	// 4. PRELIMINARY guard against a silent clobber of an existing live db — a cheap
	// fast-fail BEFORE the expensive lock/stage work. This check is made WITHOUT the
	// lock, so it is only advisory: the AUTHORITATIVE existence/force decision that
	// gates the destructive rename is re-made under the lock at step 4d below.
	liveExists := fileExists(p.LiveDBPath)
	if liveExists && !p.Force {
		return restoreResult{}, fmt.Errorf("live db already exists at %s; pass --force to overwrite (the current db is moved aside to a .bak first)", p.LiveDBPath)
	}

	// 4b. VALIDATE the archived plain files BEFORE committing ANYTHING. A
	// config.toml that does not parse as TOML must abort the whole restore now,
	// while the live state is still fully intact — never install a config that
	// pix cannot then load. op-refs.env carries no parse contract, so it is
	// only validated by being written atomically below.
	stamp := now.Format("20060102-150405")
	archivedConfig := filepath.Join(tmpDir, "config.toml")
	archivedOpRefs := filepath.Join(tmpDir, "op-refs.env")
	var restoredCfg *config.Config
	if p.ConfigPath != "" && fileExists(archivedConfig) {
		cfg, err := config.LoadFrom(archivedConfig)
		if err != nil {
			return restoreResult{}, fmt.Errorf("archived config.toml does not parse as TOML; refusing restore before any change: %w", err)
		}
		restoredCfg = cfg
	}

	// 4c. Take the shared advisory store lock BEFORE mutating ANY live file — the
	// AUTHORITY that makes restore and the memory daemon mutually exclusive, closing
	// the TOCTOU the port probe alone leaves open (the daemon opens the db BEFORE
	// binding its port, so it can hold the live store while the port still reads
	// DOWN). NON-BLOCKING and taken UP FRONT: a held lock means a daemon or another
	// restore owns the store, so REFUSE now — BEFORE the first plain-file move — so a
	// refused restore leaves the live config/op-refs/db byte-for-byte intact. Held
	// ACROSS the ENTIRE commit + rollback sequence and released only at the very end.
	acquire := p.acquireLockFn
	if acquire == nil {
		acquire = acquireLock
	}
	lockPath := p.LockPath
	if lockPath == "" {
		lockPath = filepath.Join(filepath.Dir(p.LiveDBPath), ".memory.lock")
	}
	releaseLock, lockErr := acquire(lockPath)
	if lockErr != nil {
		return restoreResult{}, fmt.Errorf("the memory service (or another restore) is using the database — stop it first: pix serve stop")
	}
	defer releaseLock()

	// 4d. RE-STAT the live db and RE-ENFORCE the --force rule UNDER the lock. The
	// liveExists/force decision at step 4 was made BEFORE the lock, so it is stale:
	// in the TOCTOU window between that check and this acquisition another restore
	// could have installed a db (restore B saw none, paused; restore A took the lock,
	// installed a db, released; B now holds the lock with a stale liveExists=false).
	// Re-evaluating BOTH here — while we hold the store exclusive — ensures the
	// destructive rename below is gated by state we actually own, so B cannot bypass
	// the --force guard and silently clobber A's db. This runs BEFORE the first
	// plain-file move, so a refusal leaves live config/op-refs/db byte-for-byte
	// intact (nothing to roll back). The recomputed liveExists is what swapMemory
	// uses to decide the move-aside, so a db that appeared under --force is kept.
	liveExists = fileExists(p.LiveDBPath)
	if liveExists && !p.Force {
		return restoreResult{}, fmt.Errorf("live db already exists at %s; pass --force to overwrite (the current db is moved aside to a .bak first)", p.LiveDBPath)
	}

	// A stack of undo closures for every COMMITTED step, executed in reverse on any
	// later failure. rollbackSteps attempts every undo and joins their errors so a
	// failed rollback is surfaced loudly rather than swallowed.
	var undos []func() error
	rollbackSteps := func() error {
		var errs []error
		for i := len(undos) - 1; i >= 0; i-- {
			if err := undos[i](); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}

	result := restoreResult{
		LivePath:  p.LiveDBPath,
		Knowledge: manifest.Knowledge,
	}

	// 5. Restore PLAIN FILES FIRST (config.toml, then op-refs.env). They are cheap
	// to roll back, so doing them before the expensive/risky memory swap keeps the
	// failure surface small. Each install moves the current file aside to a unique
	// .bak and writes the archived content atomically (temp-in-same-dir + rename).

	// 5a. config.toml. A failure here has nothing earlier to roll back (installPlainFile
	// undoes its own partial move internally), so return directly.
	if p.ConfigPath != "" {
		bak, restored, undo, err := installPlainFile(archivedConfig, p.ConfigPath, stamp, false, nil, nil)
		if err != nil {
			return restoreResult{}, fmt.Errorf("restore config.toml: %w", err)
		}
		if restored {
			undos = append(undos, undo)
			result.ConfigRestored = true
			result.ConfigBak = bak
			// Profiles were removed; result.Profiles falls back to the manifest note
			// (for reading old archives) below.
			_ = restoredCfg
		}
	}
	if result.Profiles == nil {
		result.Profiles = manifest.Profiles
	}

	// 5b. op-refs.env (0600 file, parent dir tightened to 0700). On failure, roll
	// back the already-restored config so we never leave split state.
	if p.OpRefsPath != "" {
		bak, restored, undo, err := installPlainFile(archivedOpRefs, p.OpRefsPath, stamp, true, nil, nil)
		if err != nil {
			if rbErr := rollbackSteps(); rbErr != nil {
				return restoreResult{}, fmt.Errorf("restore op-refs.env failed (%v) AND rollback FAILED — state may be inconsistent: %w", err, rbErr)
			}
			return restoreResult{}, fmt.Errorf("restore op-refs.env: %w", err)
		}
		if restored {
			undos = append(undos, undo)
			result.OpRefsRestored = true
			result.OpRefsBak = bak
		}
	}

	// 5c. Memory swap LAST — the expensive, corruption-prone sqlite step. On ANY
	// failure, roll back the plain files restored above (reverse order) so a partial
	// restore never leaves the config/op-refs pointing at a db that never landed.
	bakBase, rowCount, err := swapMemory(p, archivedDB, stamp, liveExists, moveRename, rename)
	if err != nil {
		if rbErr := rollbackSteps(); rbErr != nil {
			return restoreResult{}, fmt.Errorf("%v; AND rollback of restored config/op-refs FAILED — state may be inconsistent: %w", err, rbErr)
		}
		return restoreResult{}, err
	}
	result.BackupPath = bakBase
	result.RowCount = rowCount

	return result, nil
}

// swapMemory performs the hardened memory-db swap: stage the validated archived
// db into a same-dir temp, RE-PROBE serve is still down right before the first
// live-file rename (the tightest TOCTOU point), move the CURRENT db + sidecars
// aside into a unique .bak set (kept, never deleted; no stale WAL left to
// replay), then os.Rename the staged file into place LAST (atomic on POSIX). On
// any failure at/before the final rename it rolls the moved-aside set back so the
// live db is never left missing, surfacing a failed rollback LOUDLY. Returns the
// .bak base path ("" when nothing was moved) and the restored row count.
//
// The row count for the report is read from the STAGED/validated ARCHIVED db
// BEFORE the swap, so NOTHING fallible runs after the memory rename commits.
// Counting it post-swap (from the live path) reopened a data-safety window: a
// failure there returned an error while the memory swap stayed committed, leaving
// old (caller-rolled-back) config/op-refs beside a NEW memory db — split state.
// The count is identical either way (the staged file IS what becomes live).
func swapMemory(p restoreParams, archivedDB, stamp string, liveExists bool,
	moveRename, rename func(oldpath, newpath string) error) (bakBase string, rowCount int, err error) {
	destDir := filepath.Dir(p.LiveDBPath)
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return "", 0, fmt.Errorf("dest dir: %w", err)
	}

	// Read the row count for the report from the validated archived db NOW, before
	// any live-state mutation. This is the last fallible read; everything after the
	// swap must be infallible so a post-swap error can never strand a committed
	// memory swap next to rolled-back config/op-refs.
	rowCount, err = countLiveRows(archivedDB)
	if err != nil {
		return "", 0, fmt.Errorf("count archived rows: %w", err)
	}

	// Stage the validated db into a UNIQUE temp in the DEST dir so the final rename
	// is same-filesystem and atomic. This copy can be large; it does NOT touch live
	// state, so it is safe to do BEFORE the final serve re-probe.
	staged, err := stageRestoredDB(destDir, archivedDB)
	if err != nil {
		return "", 0, fmt.Errorf("stage restored db: %w", err)
	}
	defer os.Remove(staged) // best-effort: gone after a successful rename

	// Re-probe serve is STILL down IMMEDIATELY before the first rename that touches
	// live files. The stage copy above can take a while, so the earlier probe would
	// leave a window where serve starts DURING the copy; this is the last safe point
	// to bail with zero live-state mutation.
	if p.ServeProbe != nil && p.ServeProbe() {
		return "", 0, fmt.Errorf("memory serve came up during restore; aborted before any swap — stop it and retry")
	}

	// Move the CURRENT state aside COMPLETELY (db + -wal + -shm) into a unique
	// .bak-<ts>-<rand> set. This runs even when the main db is ABSENT: a prior
	// failed run can leave an ORPHAN -wal/-shm, and installing the restored db
	// beside a stale WAL would replay it.
	var movedAside [][2]string // [from, to] pairs, for rollback
	needMove := liveExists || pathExists(p.LiveDBPath+"-wal") || pathExists(p.LiveDBPath+"-shm")
	if needMove {
		token, err := restoreToken()
		if err != nil {
			return "", 0, err
		}
		bakBase = p.LiveDBPath + ".bak-" + stamp + "-" + token
		for _, sc := range []string{"", "-wal", "-shm"} {
			src := p.LiveDBPath + sc
			if !pathExists(src) {
				continue
			}
			dst := bakBase + sc
			if err := moveRename(src, dst); err != nil {
				if rbErr := rollbackMoves(moveRename, movedAside); rbErr != nil {
					return "", 0, fmt.Errorf("move current %s aside failed (%v) AND rollback FAILED — live db may be missing: %w", src, err, rbErr)
				}
				return "", 0, fmt.Errorf("move current %s aside: %w", src, err)
			}
			movedAside = append(movedAside, [2]string{src, dst})
		}
	}

	// Atomic move into place LAST. On failure, ROLL BACK the moved-aside set so the
	// live db is never left missing; a rollback that itself FAILS is LOUD.
	if err := rename(staged, p.LiveDBPath); err != nil {
		if rbErr := rollbackMoves(moveRename, movedAside); rbErr != nil {
			return "", 0, fmt.Errorf("swap failed (%v) AND rollback FAILED — live db may be missing: %w", err, rbErr)
		}
		return "", 0, fmt.Errorf("swap restored db into place: %w", err)
	}

	// NOTHING fallible runs after the swap: the row count was read pre-swap from the
	// staged archived db (identical to what just became live), and the FTS index
	// travels INSIDE memory.db (content table, copied by VACUUM INTO) — no rebuild.
	return bakBase, rowCount, nil
}

// installPlainFile restores a single plain file (config.toml / op-refs.env) from
// an extracted archive. If src is absent it is a no-op (restored=false, undo=nil).
// If a current dest exists it is moved aside to a UNIQUE <dest>.bak-<stamp>-<rand>
// (reversible, never deleted); then the archived content is written ATOMICALLY
// (temp-in-same-dir + rename, so a crash never leaves a partial file) with 0600.
// When tightenDir is set the parent dir is chmod'd to 0700 (MkdirAll does NOT
// tighten an existing dir), for op-refs.env. Returns the .bak path ("" when there
// was nothing to move aside), whether a file was written, and an undo closure
// that reverses this step (remove the installed file, move the .bak back).
//
// writeFn/renameFn are injection points (nil -> atomicWriteFile / os.Rename) so a
// test can force the write to fail AND its immediate rollback rename to fail, to
// prove that a failed rollback is surfaced LOUDLY rather than swallowed.
func installPlainFile(src, dest, stamp string, tightenDir bool,
	writeFn func(dest string, data []byte, mode os.FileMode) error,
	renameFn func(oldpath, newpath string) error,
) (bak string, restored bool, undo func() error, err error) {
	if writeFn == nil {
		writeFn = atomicWriteFile
	}
	if renameFn == nil {
		renameFn = os.Rename
	}
	if !fileExists(src) {
		return "", false, nil, nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return "", false, nil, fmt.Errorf("read archived file: %w", err)
	}
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", false, nil, fmt.Errorf("dest dir: %w", err)
	}
	if tightenDir {
		if err := os.Chmod(dir, 0o700); err != nil {
			return "", false, nil, fmt.Errorf("tighten dest dir: %w", err)
		}
	}
	if pathExists(dest) {
		token, terr := restoreToken()
		if terr != nil {
			return "", false, nil, terr
		}
		bak = dest + ".bak-" + stamp + "-" + token
		if err := renameFn(dest, bak); err != nil {
			return "", false, nil, fmt.Errorf("move current aside: %w", err)
		}
	}
	if err := writeFn(dest, data, 0o600); err != nil {
		// Roll THIS step's move back so the current file is never left missing. If the
		// rollback rename ITSELF fails, the live file can be left MISSING with nothing
		// at dest — surface that LOUDLY (joined) instead of swallowing it.
		if bak != "" {
			if rbErr := renameFn(bak, dest); rbErr != nil {
				return "", false, nil, fmt.Errorf("install %s failed (%v) AND rollback FAILED — live file may be missing: %w", dest, err, rbErr)
			}
		}
		return "", false, nil, fmt.Errorf("write restored file: %w", err)
	}
	bakCopy := bak
	undo = func() error {
		if err := os.Remove(dest); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("undo restore of %s: %w", dest, err)
		}
		if bakCopy != "" {
			if err := os.Rename(bakCopy, dest); err != nil {
				return fmt.Errorf("undo restore of %s (restore .bak): %w", dest, err)
			}
		}
		return nil
	}
	return bak, true, undo, nil
}

// atomicWriteFile writes data to dest via a same-dir temp file + rename, so a
// crash mid-write never leaves a partial or truncated file at dest. The temp is
// created 0600, chmod'd to mode, fsynced, then renamed into place.
func atomicWriteFile(dest string, data []byte, mode os.FileMode) (err error) {
	dir := filepath.Dir(dest)
	tmp, err := os.CreateTemp(dir, ".pix-restore-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		tmp.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		return fmt.Errorf("chmod temp: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("fsync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpPath, dest); err != nil {
		return fmt.Errorf("rename temp into place: %w", err)
	}
	committed = true
	return nil
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
// a USABLE memory store — not merely that tables with the right NAMES exist. A
// db can pass integrity_check + a version gate while missing these tables
// entirely (an empty/unrelated sqlite file) OR while carrying a table literally
// named `memories` with the WRONG columns (e.g. `CREATE TABLE memories(x)`);
// both would swap in an unusable live db that only fails LATER at countLiveRows —
// after the live db was clobbered. So instead of checking sqlite_master names,
// run the ACTUAL queries the app relies on, exercising the real columns. A
// schema-shaped-but-wrong table errors on the missing columns and is refused
// BEFORE any swap, leaving the live db untouched.
func verifyArchivedUsable(path string) error {
	db, err := sql.Open("sqlite", readOnlyDSN(path))
	if err != nil {
		return fmt.Errorf("open archived db: %w", err)
	}
	defer db.Close()

	// The exact live-row count query countLiveRows uses — exercises `deleted_at`.
	var n int
	if err := db.QueryRow("SELECT count(*) FROM memories WHERE deleted_at IS NULL").Scan(&n); err != nil {
		return fmt.Errorf("archived db is not a usable memory store (live-row count failed): %w", err)
	}
	// Exercise the actual columns the app reads. A table named `memories` with the
	// wrong shape (missing id/kind/content/durability/project) errors here. LIMIT 1
	// tolerates an empty-but-correctly-shaped table (no rows -> ErrNoRows, fine).
	var (
		id, kind, content, durability, project any
	)
	if err := db.QueryRow(
		"SELECT id, kind, content, durability, project FROM memories LIMIT 1",
	).Scan(&id, &kind, &content, &durability, &project); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("archived db is not a usable memory store (memories schema mismatch): %w", err)
	}
	// The FTS index must also be present (searchable). A missing/wrong memories_fts
	// would break search after restore.
	if err := db.QueryRow("SELECT count(*) FROM memories_fts").Scan(&n); err != nil {
		return fmt.Errorf("archived db is not a usable memory store (memories_fts unusable): %w", err)
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

const restoreUsage = `usage: pix-host restore <archive> [--force]

  Restore a FULL pix backup tar.gz produced by 'backup': memory.db (honors
  MEMORY_DB), config.toml (profiles), and op-refs.env. Refuses to run while
  'serve' holds the db, and refuses to overwrite an existing live db unless
  --force is given (in which case the current db is moved aside to a .bak-<ts>
  first, never deleted). config.toml/op-refs.env are always moved aside to a
  .bak-<ts> before the archived versions are written (reversible).

  <archive>    path to the pix-backup-<ts>.tar.gz to restore
  --force      overwrite an existing live db (current db kept as .bak-<ts>)`

func runRestoreCLI(args []string) {
	archive := ""
	force := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-h" || a == "--help":
			fmt.Println(restoreUsage)
			return
		case a == "--force" || a == "-f":
			force = true
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "pix-host restore: unknown flag %q\n%s\n", a, restoreUsage)
			os.Exit(2)
		default:
			if archive != "" {
				fmt.Fprintf(os.Stderr, "pix-host restore: unexpected argument %q\n%s\n", a, restoreUsage)
				os.Exit(2)
			}
			archive = a
		}
	}
	if archive == "" {
		fmt.Fprintf(os.Stderr, "pix-host restore: missing <archive>\n%s\n", restoreUsage)
		os.Exit(2)
	}

	res, err := memoryRestore(resolveRestoreParams(archive, force, time.Now()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix-host restore: %v\n", err)
		os.Exit(1)
	}
	printRestoreReport(os.Stdout, res)
}

// printRestoreReport prints the full restore outcome: memory, config, op-refs,
// and the knowledge-bundle notes from the manifest, ending with the start hint.
func printRestoreReport(w io.Writer, res restoreResult) {
	fmt.Fprintf(w, "restored %d memory rows to %s\n", res.RowCount, res.LivePath)
	if res.BackupPath != "" {
		fmt.Fprintf(w, "previous db kept at %s\n", res.BackupPath)
	}
	if res.ConfigRestored {
		// Profiles were removed; a restored archive's config is written verbatim but
		// any [profiles.*] tables in it are inert, so don't advertise them.
		fmt.Fprintln(w, "config restored")
		if res.ConfigBak != "" {
			fmt.Fprintf(w, "previous config kept at %s\n", res.ConfigBak)
		}
	}
	if res.OpRefsRestored {
		fmt.Fprintln(w, "op-refs restored")
		if res.OpRefsBak != "" {
			fmt.Fprintf(w, "previous op-refs kept at %s\n", res.OpRefsBak)
		}
	}
	for _, k := range res.Knowledge {
		if k.Remote != "" {
			fmt.Fprintf(w, "your knowledge bundle lives at %s (git remote %s) — restore it with git clone / pix knowledge use\n", k.Path, k.Remote)
		} else {
			fmt.Fprintf(w, "your knowledge bundle lives at %s — restore it with git clone / pix knowledge use\n", k.Path)
		}
	}
	fmt.Fprintln(w, "start it: pix serve")
}

// resolveRestoreParams fills restoreParams from the environment/home, so
// memoryRestore itself stays hermetic. It resolves the SAME config.toml /
// op-refs.env paths backup uses, so a full backup round-trips back into place.
func resolveRestoreParams(archive string, force bool, now time.Time) restoreParams {
	dbPath := config.MemoryDBPath()

	return restoreParams{
		ArchivePath: archive,
		LiveDBPath:  dbPath,
		Force:       force,
		// Restore to the CANONICAL config paths (XDG config dir), matching backup, so
		// a full backup round-trips back into place even on a repo-less install.
		ConfigPath: config.Path(),
		OpRefsPath: config.OpRefsPath(),
		Now:        now,
		ServeProbe: serveIsUp,
		// Resolve the SAME lock path the daemon takes so the two are mutually exclusive.
		LockPath: config.MemoryLockPath(),
	}
}
