// pi-stack-host `backup` — a HOT, consistent snapshot of the FULL pi-stack state
// that is safe to take WITHOUT stopping `serve`:
//
//   - the precious artifact: ~/.local/share/pi-stack/memory/memory.db (the captured facts)
//   - config.toml (profiles + all runtime settings)
//   - op-refs.env (1Password REFS only — no secret values ever touch disk)
//   - a manifest.json describing the backup, including the profile names it
//     carries and a NOTE (path + git remote) for each configured knowledge
//     bundle. Bundle CONTENT is NOT archived: a bundle is a git repo and git IS
//     its backup; the manifest only records WHERE it lives so restore can tell
//     you how to bring it back.
//
// The memory snapshot is produced with sqlite's `VACUUM INTO`, which reads a
// consistent view of the live database (WAL permits concurrent readers) and
// writes a single defragmented file — so we never plain-cp the live db, and
// never copy the -wal/-shm sidecars (their content is already folded into the
// snapshot). The snapshot is then verified (`PRAGMA integrity_check` must be
// "ok") before it is packed.
//
// This lives in pi-stack-host (not the dependency-light launcher) because it
// needs the sqlite driver. The launcher `pi-stack backup` execs this.

package main

import (
	"archive/tar"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"pi-stack/host/config"

	_ "modernc.org/sqlite"
)

// backupFormatVersion is the archive format this binary writes. v1 was
// memory-only-manifest (config.toml + op-refs.env still travelled in the tar,
// but the manifest recorded neither profiles nor knowledge). v2 adds the
// `profiles` list and the `knowledge` notes. Restore reads BOTH: a v1 archive
// still restores memory (and its config/op-refs) fine — the new fields simply
// default to empty.
const backupFormatVersion = 2

// knowledgeNote records WHERE a configured knowledge bundle lives so a restore
// can point you back at it. The bundle content is a git repo and is NOT archived
// (git is its backup); this is provenance only.
type knowledgeNote struct {
	Path   string `json:"path"`
	Remote string `json:"remote,omitempty"`
}

// backupManifest is serialized to manifest.json inside the archive. It records
// enough to detect version/model drift on a future restore AND to tell the user
// which profiles + knowledge bundles the backup covered.
type backupManifest struct {
	FormatVersion     int             `json:"format_version"`
	PiStackVersion    string          `json:"pi_stack_version"`
	SqliteUserVersion int             `json:"sqlite_user_version"`
	CreatedAt         string          `json:"created_at"`
	Hostname          string          `json:"hostname"`
	MemoryRowCount    int             `json:"memory_row_count"`
	MemoryEmbedModel  string          `json:"memory_embed_model"`
	Profiles          []string        `json:"profiles,omitempty"`
	Knowledge         []knowledgeNote `json:"knowledge,omitempty"`
	Contents          []string        `json:"contents"`
}

// backupParams are the fully-resolved inputs to the backup core, so the core is
// hermetic and testable (no env/home lookups inside).
type backupParams struct {
	DBPath     string          // source live memory.db
	OutPath    string          // full archive path to write
	Keep       int             // retention: keep newest N pi-stack-backup-*.tar.gz in OutPath's dir
	Version    string          // pi_stack_version for the manifest
	EmbedModel string          // memory_embed_model for the manifest
	ConfigPath string          // config.toml to include if it exists ("" to skip)
	OpRefsPath string          // op-refs.env to include if it exists ("" to skip)
	Profiles   []string        // profile names in the config being backed up (manifest note)
	Knowledge  []knowledgeNote // configured knowledge bundle locations (manifest note; content NOT archived)
	Now        time.Time       // timestamp source (zero -> time.Now())
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

	// 0b. NEVER overwrite an existing archive. An explicit --out pointed at a file
	// that already exists (or the vanishingly-rare default-name collision) would
	// destroy a previous backup. Refuse with a clear message BEFORE writing; the
	// no-clobber os.Link in writeBackupArchive is the atomic backstop for the TOCTOU
	// window between this check and the commit.
	if pathExists(p.OutPath) {
		return backupResult{}, fmt.Errorf("refusing to overwrite existing archive %s; choose a different --out or remove it first", p.OutPath)
	}

	// 0c. Best-effort: warn (to stderr, never blocking, never echoing the value) if
	// op-refs.env appears to carry a pasted literal secret rather than op:// refs.
	if p.OpRefsPath != "" && fileExists(p.OpRefsPath) && opRefsHasPastedSecret(p.OpRefsPath) {
		fmt.Fprintln(os.Stderr, "warning: op-refs.env appears to contain a pasted secret VALUE; the backup will include it. Prefer op:// references.")
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
		Profiles:          p.Profiles,
		Knowledge:         p.Knowledge,
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

	// 4. Retention: keep the newest N pi-stack-backup-*.tar.gz in the out dir. Pass
	// the archive we just wrote so retention NEVER prunes it (see pruneBackups).
	if err := pruneBackups(filepath.Dir(p.OutPath), p.Keep, p.OutPath); err != nil {
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
// The write is ATOMIC, PRIVATE, and NO-CLOBBER: it builds the archive in a
// same-dir temp file (os.CreateTemp, 0600), fsyncs it, then hard-LINKs it into
// place (os.Link fails EEXIST if outPath already exists, giving atomic no-clobber
// semantics that os.Rename lacks on POSIX). The final path is never O_TRUNC'd, so
// a mid-write failure never leaves a partial or mode-widened archive at outPath,
// and a re-run cannot destroy an existing backup.
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
	// Hard-link (not rename) so an existing outPath is NOT clobbered: os.Link fails
	// with EEXIST if the target exists, which is exactly the atomic no-clobber
	// commit we want. On success the temp name is dropped (the inode survives under
	// outPath).
	if err := os.Link(tmpPath, outPath); err != nil {
		return fmt.Errorf("finalize archive (refusing to clobber existing %s): %w", outPath, err)
	}
	committed = true
	_ = os.Remove(tmpPath)
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
// pi-stack-backup-<8 digits>-<6 digits>[-<hex rand>].tar.gz. The random suffix is
// OPTIONAL so v1 (no-suffix) names still prune. Retention uses this strict match
// so a hand-placed file (e.g. keepme.tar.gz, or a backup from another tool) is
// NEVER deleted — a loose *.tar.gz glob would have swept those away.
var backupNameRe = regexp.MustCompile(`^pi-stack-backup-\d{8}-\d{6}(-[0-9a-f]+)?\.tar\.gz$`)

// pruneBackups deletes the oldest of OUR backups in dir beyond keep. It matches
// only files whose name is exactly the generated pattern; anything else is left
// untouched.
//
// Retention sorts by FILE MODIFICATION TIME (newest first), NOT by filename. The
// default archive name now carries a RANDOM suffix, so a lexical filename sort no
// longer equals chronological order — a lexical prune could delete the
// just-written backup while sparing an older one. Sorting by mtime keeps the
// newest N regardless of the random suffix.
//
// keepPath is the archive THIS run just wrote ("" when called standalone). It is
// EXCLUDED from the deletion candidates UNCONDITIONALLY — we never prune the file
// we just created, regardless of mtime/name ordering. On a coarse-timestamp
// filesystem the fresh archive can tie an older one on mtime, lose the name
// tie-break, and be pruned while the command still reports success (its size was
// captured pre-prune). Excluding it by path closes that data-loss window; the
// newest-N-by-mtime behavior still governs the REST.
func pruneBackups(dir string, keep int, keepPath string) error {
	if keep <= 0 {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	keepResolved := resolvePath(keepPath)
	type backupFile struct {
		path    string
		modTime time.Time
	}
	var matches []backupFile
	excludedFresh := false
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !backupNameRe.MatchString(e.Name()) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		// NEVER prune the archive we just wrote — exclude it from the candidates
		// before any mtime/name ordering can single it out.
		if keepResolved != "" && resolvePath(path) == keepResolved {
			excludedFresh = true
			continue
		}
		fi, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("stat backup %s: %w", path, err)
		}
		matches = append(matches, backupFile{path: path, modTime: fi.ModTime()})
	}
	// The fresh archive already occupies one of the keep slots, so keep one fewer
	// of the REST when it was excluded.
	effKeep := keep
	if excludedFresh {
		effKeep = keep - 1
		if effKeep < 0 {
			effKeep = 0
		}
	}
	if len(matches) <= effKeep {
		return nil
	}
	// Newest first by mtime; break ties by name (descending) so the order is stable
	// and deterministic for two files written in the same second.
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].modTime.Equal(matches[j].modTime) {
			return matches[i].path > matches[j].path
		}
		return matches[i].modTime.After(matches[j].modTime)
	})
	for _, old := range matches[effKeep:] { // everything after the newest kept
		if err := os.Remove(old.path); err != nil {
			return fmt.Errorf("prune %s: %w", old.path, err)
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

const memoryHostUsage = `usage: pi-stack-host memory

  (no subcommand)   run the memory daemon (:11435, JSON-RPC)

The backup/restore commands are now TOP-LEVEL verbs (they cover config + op-refs
+ memory, not memory alone): pi-stack-host backup|restore.`

// runMemoryHost dispatches `pi-stack-host memory`. Bare `pi-stack-host memory`
// (no args) runs the daemon, preserving back-compat with the service entry.
// backup/restore were PROMOTED to top-level verbs (they cover config + op-refs +
// memory now, not memory alone), so they are no longer memory subcommands. An
// UNKNOWN first token is a typo, not a silent daemon start: print usage and exit
// 2 (never fall through to ListenAndServe).
func runMemoryHost(args []string) {
	if len(args) == 0 {
		runMemory()
		return
	}
	switch args[0] {
	case "-h", "--help":
		fmt.Println(memoryHostUsage)
	default:
		fmt.Fprintf(os.Stderr, "pi-stack-host memory: unknown subcommand %q\n%s\n", args[0], memoryHostUsage)
		os.Exit(2)
	}
}

const backupUsage = `usage: pi-stack-host backup [--out PATH] [--keep N]

  Take a hot, consistent FULL backup — safe while serve holds the db open. Packs
  a VACUUM INTO snapshot of ~/.local/share/pi-stack/memory/memory.db (honors MEMORY_DB),
  config.toml, op-refs.env (refs only), and a manifest.json (profiles +
  knowledge-bundle notes) into a tar.gz.

  --out PATH   archive path (default ~/.local/share/pi-stack/backups/pi-stack-backup-<ts>.tar.gz)
  --keep N     keep only the newest N backups in the out dir (default 7)`

func runBackupCLI(args []string) {
	outPath := ""
	keep := 7
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-h" || a == "--help":
			fmt.Println(backupUsage)
			return
		case a == "--out" || a == "-o":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "pi-stack-host backup: --out needs a value")
				os.Exit(2)
			}
			i++
			outPath = args[i]
		case strings.HasPrefix(a, "--out="):
			outPath = strings.TrimPrefix(a, "--out=")
		case a == "--keep":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "pi-stack-host backup: --keep needs a value")
				os.Exit(2)
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil {
				fmt.Fprintf(os.Stderr, "pi-stack-host backup: --keep needs an integer, got %q\n", args[i])
				os.Exit(2)
			}
			keep = n
		case strings.HasPrefix(a, "--keep="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--keep="))
			if err != nil {
				fmt.Fprintf(os.Stderr, "pi-stack-host backup: --keep needs an integer\n")
				os.Exit(2)
			}
			keep = n
		default:
			fmt.Fprintf(os.Stderr, "pi-stack-host backup: unknown argument %q\n%s\n", a, backupUsage)
			os.Exit(2)
		}
	}

	res, err := memoryBackup(resolveBackupParams(outPath, keep, time.Now()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack-host backup: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("backup: %s (%d rows, %s)\n", res.Path, res.RowCount, humanSize(res.Size))
	if len(res.Contents) > 0 {
		fmt.Printf("contents: %s\n", strings.Join(res.Contents, ", "))
	}
}

// resolveBackupParams fills backupParams from the environment/home + the loaded
// config, so memoryBackup itself stays hermetic. It loads config best-effort to
// record the profile names and knowledge-bundle notes in the manifest — a config
// error never aborts a backup (the memory db is the precious part).
func resolveBackupParams(outPath string, keep int, now time.Time) backupParams {
	dbPath := config.MemoryDBPath()

	if outPath == "" {
		// A short random suffix makes the default name collision-proof: two backups
		// in the same second (the timestamp's resolution) get distinct names instead
		// of the second destroying the first.
		name := "pi-stack-backup-" + now.Format("20060102-150405")
		if tok, err := restoreToken(); err == nil {
			name += "-" + tok
		}
		name += ".tar.gz"
		outPath = filepath.Join(config.BackupsDir(), name)
	}

	embedModel := strings.TrimSpace(os.Getenv("MEMORY_EMBED_MODEL"))
	if embedModel == "" {
		embedModel = "nomic-embed-text"
	}

	var profiles []string // profiles were removed; field kept for old-archive compat
	var knowledge []knowledgeNote
	if cfg, err := config.Load(); err == nil && cfg != nil {
		for _, b := range cfg.AllKnowledgeBundles() {
			// Redact any userinfo/token in the recorded remote so a manifest note
			// (later PRINTED at restore) never leaks a credential embedded in the URL.
			knowledge = append(knowledge, knowledgeNote{Path: b, Remote: config.RedactURL(bundleGitRemote(b))})
		}
	}

	return backupParams{
		DBPath:     dbPath,
		OutPath:    outPath,
		Keep:       keep,
		Version:    version,
		EmbedModel: embedModel,
		// Use the CANONICAL config paths (XDG config dir), not a CWD-relative
		// config/op-refs.env. On a repo-less install those are the ONLY real files.
		ConfigPath: config.Path(),
		OpRefsPath: config.OpRefsPath(),
		Profiles:   profiles,
		Knowledge:  knowledge,
		Now:        now,
	}
}

// opRefsHasPastedSecret best-effort reports whether op-refs.env carries a pasted
// literal secret VALUE (rather than an op:// reference). It skips blank lines,
// comments, op:// refs, and the documented non-secret allowlist, then judges the
// remaining values with the shared config.LooksSecretShaped heuristic. It NEVER
// returns or logs the value — only whether one looks secret-shaped.
func opRefsHasPastedSecret(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = strings.Trim(val, `"'`)
		if val == "" || strings.HasPrefix(val, "op://") || config.NonSecretOpRefsKeys[key] {
			continue
		}
		if config.LooksSecretShaped(key, val) {
			return true
		}
	}
	return false
}

// bundleGitRemote best-effort reports a knowledge bundle's `origin` git remote
// URL for the manifest note. Any error (not a git repo, no git binary, no
// origin) yields "" — this is provenance-only and must never fail a backup.
func bundleGitRemote(path string) string {
	if path == "" {
		return ""
	}
	cmd := exec.Command("git", "-C", path, "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
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
