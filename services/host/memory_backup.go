// pi-stack-host `memory backup` — a HOT, consistent snapshot of the precious
// artifact (~/.pi-stack/memory/memory.db, the captured facts) that is safe to
// take WITHOUT stopping `serve`.
//
// The snapshot is produced with sqlite's `VACUUM INTO`, which reads a consistent
// view of the live database (WAL permits concurrent readers) and writes a single
// defragmented file — so we never plain-cp the live db, and never copy the
// -wal/-shm sidecars (their content is already folded into the snapshot). The
// snapshot is then verified (`PRAGMA integrity_check` must be "ok") and packed
// into a tar.gz alongside config.toml / op-refs.env (refs only, safe) and a
// manifest.json describing the backup.
//
// This lives in pi-stack-host (not the dependency-light launcher) because it
// needs the sqlite driver. The launcher `pi-stack memory backup` execs this.

package main

import (
	"archive/tar"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const backupFormatVersion = 1

// backupManifest is serialized to manifest.json inside the archive. It records
// enough to detect version/model drift on a future restore.
type backupManifest struct {
	FormatVersion     int      `json:"format_version"`
	PiStackVersion    string   `json:"pi_stack_version"`
	SqliteUserVersion int      `json:"sqlite_user_version"`
	CreatedAt         string   `json:"created_at"`
	Hostname          string   `json:"hostname"`
	MemoryRowCount    int      `json:"memory_row_count"`
	MemoryEmbedModel  string   `json:"memory_embed_model"`
	Contents          []string `json:"contents"`
}

// backupParams are the fully-resolved inputs to the backup core, so the core is
// hermetic and testable (no env/home lookups inside).
type backupParams struct {
	DBPath     string    // source live memory.db
	OutPath    string    // full archive path to write
	Keep       int       // retention: keep newest N pi-stack-backup-*.tar.gz in OutPath's dir
	Version    string    // pi_stack_version for the manifest
	EmbedModel string    // memory_embed_model for the manifest
	ConfigPath string    // config.toml to include if it exists ("" to skip)
	OpRefsPath string    // op-refs.env to include if it exists ("" to skip)
	Now        time.Time // timestamp source (zero -> time.Now())
}

// backupResult reports what was written, for the CLI to print.
type backupResult struct {
	Path        string
	RowCount    int
	Size        int64
	UserVersion int
	Contents    []string
}

// memoryBackup is the testable core: VACUUM INTO a temp snapshot, verify it,
// assemble a tar.gz at p.OutPath, then apply retention. It never mutates the
// source db and never stops a running serve.
func memoryBackup(p backupParams) (backupResult, error) {
	now := p.Now
	if now.IsZero() {
		now = time.Now()
	}

	// 0. NEVER clobber live state. If --out resolves to the live memory.db, the
	// config.toml, or the op-refs.env, refuse before writing a single byte — an
	// atomic rename onto one of those would destroy the very thing we back up.
	if err := refuseClobberLive(p.OutPath, p.DBPath, p.ConfigPath, p.OpRefsPath); err != nil {
		return backupResult{}, err
	}

	// 1. HOT snapshot via VACUUM INTO into a private temp dir. VACUUM INTO refuses
	// to overwrite an existing target, so a fresh temp dir guarantees a clean path.
	tmpDir, err := os.MkdirTemp("", "pi-stack-backup-")
	if err != nil {
		return backupResult{}, fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	snapPath := filepath.Join(tmpDir, "memory.db")

	if err := vacuumInto(p.DBPath, snapPath); err != nil {
		return backupResult{}, err
	}

	// 2. Verify the snapshot and read the metadata for the manifest.
	userVersion, rowCount, err := verifySnapshot(snapPath)
	if err != nil {
		return backupResult{}, err
	}

	// 3. Assemble the archive.
	manifest := backupManifest{
		FormatVersion:     backupFormatVersion,
		PiStackVersion:    orDefault(p.Version, "dev"),
		SqliteUserVersion: userVersion,
		CreatedAt:         now.UTC().Format(time.RFC3339),
		Hostname:          backupHostname(),
		MemoryRowCount:    rowCount,
		MemoryEmbedModel:  p.EmbedModel,
	}
	// contents[] mirrors what actually lands in the tar, in write order.
	manifest.Contents = []string{"memory.db"}
	if p.ConfigPath != "" && fileExists(p.ConfigPath) {
		manifest.Contents = append(manifest.Contents, "config.toml")
	}
	if p.OpRefsPath != "" && fileExists(p.OpRefsPath) {
		manifest.Contents = append(manifest.Contents, "op-refs.env")
	}
	manifest.Contents = append(manifest.Contents, "manifest.json")

	if err := os.MkdirAll(filepath.Dir(p.OutPath), 0o700); err != nil {
		return backupResult{}, fmt.Errorf("backup dir: %w", err)
	}
	if err := writeBackupArchive(p.OutPath, snapPath, p.ConfigPath, p.OpRefsPath, manifest); err != nil {
		return backupResult{}, err
	}

	fi, err := os.Stat(p.OutPath)
	if err != nil {
		return backupResult{}, err
	}

	// 4. Retention: keep the newest N pi-stack-backup-*.tar.gz in the out dir.
	if err := pruneBackups(filepath.Dir(p.OutPath), p.Keep); err != nil {
		return backupResult{}, err
	}

	return backupResult{
		Path:        p.OutPath,
		RowCount:    rowCount,
		Size:        fi.Size(),
		UserVersion: userVersion,
		Contents:    manifest.Contents,
	}, nil
}

// refuseClobberLive fails if outPath (after resolving to an absolute, symlink-
// cleaned path) equals any live artifact we must not destroy. Comparing resolved
// absolute paths defends against `./x` vs an absolute form of the same file.
func refuseClobberLive(outPath string, live ...string) error {
	out := resolvePath(outPath)
	for _, l := range live {
		if l == "" {
			continue
		}
		if resolvePath(l) == out {
			return fmt.Errorf("--out %s would overwrite live state (%s); choose a different path", outPath, l)
		}
	}
	return nil
}

// resolvePath makes a best-effort absolute, symlink-resolved form for comparison.
// It never fails: on any error it falls back to filepath.Clean so two spellings
// of the same path still tend to compare equal.
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

// readOnlyDSN builds a modernc-sqlite DSN that opens the db READ-ONLY, so a
// backup can never create or mutate the source. It appends mode=ro with the
// correct separator for an existing query string.
func readOnlyDSN(path string) string {
	if strings.Contains(path, "?") {
		return path + "&mode=ro"
	}
	return path + "?mode=ro"
}

// vacuumInto opens the live db READ-ONLY and writes a consistent single-file
// snapshot to dst via `VACUUM INTO`. A busy_timeout lets it wait briefly for the
// writer's lock instead of failing immediately under a concurrent serve.
func vacuumInto(srcPath, dst string) error {
	db, err := sql.Open("sqlite", readOnlyDSN(srcPath))
	if err != nil {
		return fmt.Errorf("open source db: %w", err)
	}
	defer db.Close()
	if _, err := db.Exec("PRAGMA busy_timeout=10000;"); err != nil {
		return fmt.Errorf("busy_timeout: %w", err)
	}
	// VACUUM INTO takes an expression; a bound parameter is accepted and avoids
	// any single-quote escaping in the destination path.
	if _, err := db.Exec("VACUUM INTO ?", dst); err != nil {
		return fmt.Errorf("VACUUM INTO snapshot: %w", err)
	}
	return nil
}

// verifySnapshot opens the snapshot standalone and asserts it is intact,
// returning its schema version and the count of live (non-deleted) memories.
func verifySnapshot(path string) (userVersion, rowCount int, err error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return 0, 0, fmt.Errorf("open snapshot: %w", err)
	}
	defer db.Close()

	var integrity string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		return 0, 0, fmt.Errorf("integrity_check: %w", err)
	}
	if integrity != "ok" {
		return 0, 0, fmt.Errorf("snapshot failed integrity_check: %s", integrity)
	}
	if err := db.QueryRow("PRAGMA user_version").Scan(&userVersion); err != nil {
		return 0, 0, fmt.Errorf("user_version: %w", err)
	}
	if err := db.QueryRow("SELECT count(*) FROM memories WHERE deleted_at IS NULL").Scan(&rowCount); err != nil {
		return 0, 0, fmt.Errorf("row count: %w", err)
	}
	return userVersion, rowCount, nil
}

// writeBackupArchive packs the snapshot, optional config.toml / op-refs.env, and
// the manifest into a 0600 tar.gz. Only the VACUUMed snapshot is included as
// memory.db — the -wal/-shm sidecars are deliberately never copied.
//
// The write is ATOMIC and PRIVATE: it builds the archive in a same-dir temp file
// (os.CreateTemp, 0600), fsyncs it, then os.Renames it into place. The final
// path is never O_TRUNC'd, so a mid-write failure never leaves a partial or
// mode-widened archive at outPath, and a re-run in the same second cannot
// corrupt an existing backup.
func writeBackupArchive(outPath, snapPath, configPath, opRefsPath string, manifest backupManifest) (err error) {
	dir := filepath.Dir(outPath)
	tmp, err := os.CreateTemp(dir, ".pi-stack-backup-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp archive: %w", err)
	}
	tmpPath := tmp.Name()
	// On any failure, remove the temp; on success it is renamed away first so the
	// remove is a harmless no-op.
	committed := false
	defer func() {
		tmp.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod temp archive: %w", err)
	}

	gz := gzip.NewWriter(tmp)
	tw := tar.NewWriter(gz)

	if err := tarAddFile(tw, "memory.db", snapPath); err != nil {
		return err
	}
	if configPath != "" && fileExists(configPath) {
		if err := tarAddFile(tw, "config.toml", configPath); err != nil {
			return err
		}
	}
	if opRefsPath != "" && fileExists(opRefsPath) {
		if err := tarAddFile(tw, "op-refs.env", opRefsPath); err != nil {
			return err
		}
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	if err := tarAddBytes(tw, "manifest.json", manifestBytes); err != nil {
		return err
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("close gzip: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("fsync archive: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp archive: %w", err)
	}
	if err := os.Rename(tmpPath, outPath); err != nil {
		return fmt.Errorf("rename archive into place: %w", err)
	}
	committed = true
	return nil
}

func tarAddFile(tw *tar.Writer, name, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	return tarAddBytes(tw, name, data)
}

func tarAddBytes(tw *tar.Writer, name string, data []byte) error {
	hdr := &tar.Header{
		Name:    name,
		Mode:    0o600,
		Size:    int64(len(data)),
		ModTime: time.Now(),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("tar header %s: %w", name, err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("tar write %s: %w", name, err)
	}
	return nil
}

// backupNameRe matches ONLY the filenames memoryBackup generates:
// pi-stack-backup-<8 digits>-<6 digits>.tar.gz. Retention uses this strict match
// so a hand-placed file (e.g. keepme.tar.gz, or a backup from another tool) is
// NEVER deleted — a loose *.tar.gz glob would have swept those away.
var backupNameRe = regexp.MustCompile(`^pi-stack-backup-\d{8}-\d{6}\.tar\.gz$`)

// pruneBackups deletes the oldest of OUR backups in dir beyond keep. It matches
// only files whose name is exactly the generated pattern; anything else is left
// untouched. The timestamped filename makes lexicographic order == chronological.
func pruneBackups(dir string, keep int) error {
	if keep <= 0 {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var matches []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if backupNameRe.MatchString(e.Name()) {
			matches = append(matches, filepath.Join(dir, e.Name()))
		}
	}
	if len(matches) <= keep {
		return nil
	}
	sort.Strings(matches) // oldest first
	for _, old := range matches[:len(matches)-keep] {
		if err := os.Remove(old); err != nil {
			return fmt.Errorf("prune %s: %w", old, err)
		}
	}
	return nil
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

func backupHostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return h
}

// --- CLI wiring --------------------------------------------------------------

const memoryHostUsage = `usage: pi-stack-host memory [backup|restore]

  (no subcommand)   run the memory daemon (:11435, JSON-RPC)
  backup            hot, consistent snapshot of the memory DB -> tar.gz
  restore <archive> restore the memory DB from a backup tar.gz (safe swap)`

// runMemoryHost dispatches `pi-stack-host memory [backup|restore]`. Bare
// `pi-stack-host memory` (no args) runs the daemon, preserving back-compat with
// the service entry. An UNKNOWN first token is a typo, not a silent daemon
// start: print usage and exit 2 (never fall through to ListenAndServe).
func runMemoryHost(args []string) {
	if len(args) == 0 {
		runMemory()
		return
	}
	switch args[0] {
	case "backup":
		runMemoryBackupCLI(args[1:])
	case "restore":
		runMemoryRestoreCLI(args[1:])
	case "-h", "--help":
		fmt.Println(memoryHostUsage)
	default:
		fmt.Fprintf(os.Stderr, "pi-stack-host memory: unknown subcommand %q\n%s\n", args[0], memoryHostUsage)
		os.Exit(2)
	}
}

const memoryBackupUsage = `usage: pi-stack-host memory backup [--out PATH] [--keep N]

  Take a hot, consistent snapshot of ~/.pi-stack/memory/memory.db (honors
  MEMORY_DB) via VACUUM INTO — safe while serve holds the db open — verify it,
  and pack it into a tar.gz with config.toml/op-refs.env (if present) and a
  manifest.json.

  --out PATH   archive path (default ~/.pi-stack/backups/pi-stack-backup-<ts>.tar.gz)
  --keep N     keep only the newest N backups in the out dir (default 7)`

func runMemoryBackupCLI(args []string) {
	outPath := ""
	keep := 7
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-h" || a == "--help":
			fmt.Println(memoryBackupUsage)
			return
		case a == "--out" || a == "-o":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "pi-stack-host memory backup: --out needs a value")
				os.Exit(2)
			}
			i++
			outPath = args[i]
		case strings.HasPrefix(a, "--out="):
			outPath = strings.TrimPrefix(a, "--out=")
		case a == "--keep":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "pi-stack-host memory backup: --keep needs a value")
				os.Exit(2)
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil {
				fmt.Fprintf(os.Stderr, "pi-stack-host memory backup: --keep needs an integer, got %q\n", args[i])
				os.Exit(2)
			}
			keep = n
		case strings.HasPrefix(a, "--keep="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--keep="))
			if err != nil {
				fmt.Fprintf(os.Stderr, "pi-stack-host memory backup: --keep needs an integer\n")
				os.Exit(2)
			}
			keep = n
		default:
			fmt.Fprintf(os.Stderr, "pi-stack-host memory backup: unknown argument %q\n%s\n", a, memoryBackupUsage)
			os.Exit(2)
		}
	}

	res, err := memoryBackup(resolveBackupParams(outPath, keep, time.Now()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack-host memory backup: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("backup: %s (%d rows, %s)\n", res.Path, res.RowCount, humanSize(res.Size))
}

// resolveBackupParams fills backupParams from the environment/home, so
// memoryBackup itself stays hermetic.
func resolveBackupParams(outPath string, keep int, now time.Time) backupParams {
	home, _ := os.UserHomeDir()

	dbPath := strings.TrimSpace(os.Getenv("MEMORY_DB"))
	if dbPath == "" {
		dbPath = filepath.Join(home, ".pi-stack", "memory", "memory.db")
	}

	if outPath == "" {
		name := "pi-stack-backup-" + now.Format("20060102-150405") + ".tar.gz"
		outPath = filepath.Join(home, ".pi-stack", "backups", name)
	}

	embedModel := strings.TrimSpace(os.Getenv("MEMORY_EMBED_MODEL"))
	if embedModel == "" {
		embedModel = "nomic-embed-text"
	}

	return backupParams{
		DBPath:     dbPath,
		OutPath:    outPath,
		Keep:       keep,
		Version:    version,
		EmbedModel: embedModel,
		ConfigPath: backupConfigPath(home),
		OpRefsPath: backupOpRefsPath(home),
		Now:        now,
	}
}

// backupConfigPath resolves the config.toml to include, honoring the same
// overrides config.Path() uses, without importing the config package here.
func backupConfigPath(home string) string {
	if p := strings.TrimSpace(os.Getenv("PI_STACK_CONFIG")); p != "" {
		return p
	}
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, "pi-stack", "config.toml")
	}
	return filepath.Join(home, ".config", "pi-stack", "config.toml")
}

// backupOpRefsPath resolves the op-refs.env to include (refs only, safe to
// archive). $PI_STACK_OP_REFS wins; otherwise the repo-relative config/op-refs.env.
func backupOpRefsPath(home string) string {
	if p := strings.TrimSpace(os.Getenv("PI_STACK_OP_REFS")); p != "" {
		return p
	}
	return filepath.Join("config", "op-refs.env")
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
