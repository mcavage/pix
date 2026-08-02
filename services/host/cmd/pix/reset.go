package main

import (
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pix/host/config"
	"pix/host/rpc"
	"pix/host/workspace"
)

// reset is the destructive-but-reversible lifecycle verb. Nothing is ever
// hard-removed. State is MOVED aside to a
// timestamped `<dir>.bak-<unixts>` sibling, so a reset can always be undone by
// renaming the backup back. reset wipes the stack's *state* (config + data),
//
// The design is split so the destructive core is unit-testable against a temp
// HOME with no real sbx/pkill:
//
//   - resetPlan(cfg, paths, opts) is PURE — it resolves which paths get moved
//     aside and which sbx actions run, with zero filesystem/exec side effects.
//   - executeReset(...) takes injected fs ops (resetFS) + a shellEnv, so a test
//     drives it against t.TempDir() and asserts files landed in .bak.
//   - runResetCore(...) wires plan -> guard (TTY prompt / --yes) -> execute, and
//     RETURNS an error for the non-TTY-no-yes refusal instead of os.Exit, so the
//     refusal path is testable.

// resetOpts is the parsed reset flag set.
type resetOpts struct {
	keepMemory bool // --keep-memory: preserve <dataRoot>/memory (captured facts)
	sbx        bool // --sbx: also remove pix-* sandboxes + unregister MCP
	assumeYes  bool // --yes: don't prompt (required on a non-TTY)
	force      bool // --force: move the data dir even if serve appears still up
	purgeData  bool // --purge-data: ALSO move aside harvested task artifacts
	help       bool // -h/--help
}

// resetPaths are the resolved host locations reset acts on. Split out so the
// pure planner takes them injected (a test supplies temp-dir paths, no real
// $HOME lookup). memoryDir/knowledgeDir honor MEMORY_DB/KNOWLEDGE_DB.
type resetPaths struct {
	configDir    string // ~/.config/pix (config.toml, op-refs.env, broker-token, knowledge/, knowledge-cache/)
	dataRoot     string // ~/.local/share/pix (memory/ + knowledge/)
	memoryDir    string // <dataRoot>/memory or dir(MEMORY_DB): the user's captured facts
	knowledgeDir string // <dataRoot>/knowledge or dir(KNOWLEDGE_DB): the rebuildable index
	memoryDB     string // the custom MEMORY_DB file path (set ONLY when MEMORY_DB is given); "" for the default
	knowledgeDB  string // the custom KNOWLEDGE_DB file path (set ONLY when KNOWLEDGE_DB is given); "" for the default
	artifactRoot string // taskArtifactRoot(): harvested task docs. Touched only by reset --purge-data.
}

// backupTarget is one path the reset moves aside, with a human label. Dangerous
// marks a move that must not run while `serve` is still up (moving the live data
// dir out from under a sqlite writer splits the db/wal); the config-dir backup is
// always safe.
type backupTarget struct {
	Path      string
	Label     string
	Dangerous bool
	// WithSidecars marks a target that is a DB FILE (not a directory): move only
	// that file plus its -wal/-shm sidecars, never its parent dir. Used for a
	// custom MEMORY_DB/KNOWLEDGE_DB that lives OUTSIDE the pix data root (its
	// parent may hold unrelated files, e.g. ~/Documents).
	WithSidecars bool
}

// resetActions is the pure plan: exactly what will be moved + which sbx actions
// run. It carries no side effects; executeReset consumes it.
type resetActions struct {
	Backups         []backupTarget // paths to move to <path>.bak-<ts>
	KeepMemory      bool           // preserve MemoryDir (sweep DataRoot minus memory)
	MemoryDir       string         // preserved dir when KeepMemory
	MemoryDB        string         // resolved custom MEMORY_DB file path ("" for the default), so the sweep can preserve a db that lives DIRECTLY in DataRoot
	DataRoot        string         // the data root (for the keep-memory sweep)
	ConfigDir       string         // the config dir (~/.config/pix) being backed up, so executeReset can snapshot + restore its 1Password ref files
	PreserveRefs    bool           // lower-level restore option; the public reset command deliberately leaves it false
	RemoveSandboxes bool           // --sbx: remove pix-* sandboxes
	MCPRemove       []string       // --sbx: MCP server names to unregister (cfg.MCP)
	Force           bool           // --force: skip the serve-still-up guard on the data move
	RuntimeFiles    []string       // ephemeral daemon runtime files to HARD-remove (pid/lazy/lock in StateDir) — stale after the stop, not worth a .bak
}

// resetFS is the injected filesystem surface, so executeReset stays hermetic in
// tests (a temp HOME, no real rm). defaultResetFS wires the os-backed ops.
type resetFS struct {
	stat      func(path string) (os.FileInfo, error)
	lstat     func(path string) (os.FileInfo, error)
	readlink  func(path string) (string, error)
	rename    func(oldpath, newpath string) error
	readDir   func(path string) ([]os.DirEntry, error)
	remove    func(path string) error
	readFile  func(path string) ([]byte, error)
	writeFile func(path string, data []byte, perm os.FileMode) error
	mkdirAll  func(path string, perm os.FileMode) error
}

func defaultResetFS() resetFS {
	return resetFS{
		stat:      os.Stat,
		lstat:     os.Lstat,
		readlink:  os.Readlink,
		rename:    os.Rename,
		readDir:   os.ReadDir,
		remove:    os.Remove,
		readFile:  os.ReadFile,
		writeFile: os.WriteFile,
		mkdirAll:  os.MkdirAll,
	}
}

// preservedRefFiles are the config-dir files reset snapshots before the config
// dir moves aside and restores into the fresh one: 1Password op:// REFERENCES
// (pointers, not secrets), so a reset doesn't force re-pasting every one of
// them. They also stay in the .bak, so this is purely additive.
var preservedRefFiles = []string{"op-refs.env", "hostmode.env"}

// refFileSnapshot is one preserved-ref-file's captured content, read from the
// config dir before it moves aside and written back after.
type refFileSnapshot struct {
	name string
	data []byte
}

// errResetNeedsYes is returned by runResetCore when it can't prompt (non-TTY)
// and --yes was not given. The CLI wrapper maps it to exit 2. errNotExist is the
// internal "nothing to move" signal from moveAside.
var (
	errResetNeedsYes = errors.New("reset needs --yes on a non-interactive terminal")
	errNotExist      = errors.New("path does not exist")
)

// resolveResetPaths resolves the host paths reset touches from the injected env
// (MEMORY_DB/KNOWLEDGE_DB honored; the data root defaults to
// $XDG_DATA_HOME/pix, else ~/.local/share/pix; the config dir to
// config.Path()'s parent). It resolves the data root from the INJECTED env
// (not config.DataDir()) so tests stay hermetic.
func resolveResetPaths(env shellEnv) resetPaths {
	home := ""
	home = env.HomeDir()
	var dataRoot string
	if xdg := strings.TrimSpace(env.Getenv("XDG_DATA_HOME")); xdg != "" {
		dataRoot = filepath.Join(xdg, "pix")
	}
	if dataRoot == "" {
		dataRoot = filepath.Join(home, ".local", "share", "pix")
	}
	memoryDir := filepath.Join(dataRoot, "memory")
	knowledgeDir := filepath.Join(dataRoot, "knowledge")
	memoryDB := ""
	knowledgeDB := ""
	// Normalize custom db paths to ABSOLUTE at the source so every downstream
	// comparison (dir-move vs file-only, the --keep-memory preserve set, the
	// sweep) agrees. A relative MEMORY_DB (e.g. run from $HOME with
	// MEMORY_DB=.pix/x.db) would otherwise leave relative preserve keys
	// that never match the sweep's absolute entries -> memory swept aside.
	if db := strings.TrimSpace(env.Getenv("MEMORY_DB")); db != "" {
		if abs, err := filepath.Abs(db); err == nil {
			db = abs
		}
		memoryDir = filepath.Dir(db)
		memoryDB = db
	}
	if db := strings.TrimSpace(env.Getenv("KNOWLEDGE_DB")); db != "" {
		if abs, err := filepath.Abs(db); err == nil {
			db = abs
		}
		knowledgeDir = filepath.Dir(db)
		knowledgeDB = db
	}
	return resetPaths{
		configDir:    filepath.Dir(config.Path()),
		dataRoot:     dataRoot,
		memoryDir:    memoryDir,
		knowledgeDir: knowledgeDir,
		memoryDB:     memoryDB,
		knowledgeDB:  knowledgeDB,
		artifactRoot: taskArtifactRoot(),
	}
}

// resetPlan is the PURE planner. It resolves the backup targets + sbx actions
// from the config, paths, and opts — no filesystem, no exec. The config dir is
// always backed up. Without --keep-memory the whole data root (memory included)
// is moved aside; with --keep-memory only the knowledge index is targeted here
// and executeReset sweeps any OTHER non-memory data-root entry.
func resetPlan(cfg *config.Config, paths resetPaths, opts resetOpts) resetActions {
	a := resetActions{
		KeepMemory: opts.keepMemory,
		MemoryDir:  paths.memoryDir,
		MemoryDB:   paths.memoryDB,
		DataRoot:   paths.dataRoot,
		Force:      opts.force,
	}
	a.ConfigDir = paths.configDir
	if paths.configDir != "" {
		a.Backups = append(a.Backups, backupTarget{Path: paths.configDir, Label: "config"})
	}
	if opts.keepMemory {
		// Preserve the captured facts (memory); move the rebuildable index aside.
		// A custom KNOWLEDGE_DB that lives OUTSIDE the data root must move ONLY the
		// db file + its -wal/-shm sidecars, NEVER its parent dir (KNOWLEDGE_DB=
		// ~/Documents/knowledge.db must not drag all of ~/Documents aside) — the same
		// custom-outside-root handling the default path uses. A default in-root index
		// is moved as a whole directory.
		if t, ok := customDBOutsideRoot(paths.knowledgeDir, paths.knowledgeDB, paths.dataRoot, "knowledge database"); ok {
			a.Backups = append(a.Backups, t)
		} else if paths.knowledgeDir != "" {
			a.Backups = append(a.Backups, backupTarget{Path: paths.knowledgeDir, Label: "knowledge database", Dangerous: true})
		}
	} else if paths.dataRoot != "" {
		// Move the whole data root aside (captured memory + knowledge index).
		a.Backups = append(a.Backups, backupTarget{Path: paths.dataRoot, Label: "data (memory, knowledge, skills)", Dangerous: true})
		// Honor a custom MEMORY_DB / KNOWLEDGE_DB that lives OUTSIDE the data root:
		// the data-root move alone would miss it. Move ONLY the db FILE + its
		// -wal/-shm sidecars, NEVER the whole parent dir. A whole directory is moved
		// only for the pix-owned default dir, handled by the data-root move above.
		if t, ok := customDBOutsideRoot(paths.memoryDir, paths.memoryDB, paths.dataRoot, "memory database"); ok {
			a.Backups = append(a.Backups, t)
		}
		if t, ok := customDBOutsideRoot(paths.knowledgeDir, paths.knowledgeDB, paths.dataRoot, "knowledge database"); ok {
			a.Backups = append(a.Backups, t)
		}
	}
	if opts.sbx {
		a.RemoveSandboxes = true
		a.MCPRemove = append([]string(nil), cfg.MCP...)
	}
	// The daemon's ephemeral runtime files live in the STATE dir (not the config/
	// data dirs we move aside), so a plain reset would leave them behind. They are
	// stale the moment serve stops (a lock file, a pidfile, a lazy marker) and have
	// no restore value, so hard-remove them for a truly clean slate.
	a.RuntimeFiles = []string{
		config.ServePidPath(),
		config.ServeLazyMarkerPath(),
		config.ServeSpawnLockPath(),
	}
	// --purge-data moves harvested task artifacts aside too. They
	// live under XDG_DATA_HOME, OUTSIDE every tree reset normally touches, so they
	// survive a plain reset by design — this is the deliberate opt-in to
	// sweep them. Move-aside (.bak), never hard-delete, like every other data path.
	if opts.purgeData && paths.artifactRoot != "" {
		a.Backups = append(a.Backups, backupTarget{Path: paths.artifactRoot, Label: "harvested task artifacts"})
	}
	return a
}

// moveAside renames path to a UNIQUE "<path>.bak-<ts>" sibling if it exists,
// returning the backup path. A missing path yields errNotExist (a soft "nothing
// to move"). The destination is never allowed to overwrite an existing backup.
func moveAside(fsys resetFS, path string, ts int64) (string, error) {
	if path == "" {
		return "", errNotExist
	}
	if _, err := fsys.stat(path); err != nil {
		if os.IsNotExist(err) {
			return "", errNotExist
		}
		return "", err
	}
	dest := uniqueBackupPath(fsys, fmt.Sprintf("%s.bak-%d", path, ts))
	if err := fsys.rename(path, dest); err != nil {
		return "", err
	}
	return dest, nil
}

// moveFileWithSidecars moves a db FILE plus its -wal/-shm sidecars aside (each to
// its own unique .bak), without ever touching the parent directory. Absent
// sidecars are a soft skip; a real move error is collected. It records every
// moved source in `moved` and returns the created .bak paths + any errors.
func moveFileWithSidecars(fsys resetFS, b backupTarget, ts int64, moved map[string]bool, out io.Writer) ([]string, []error) {
	var created []string
	var errs []error
	anyMoved := false
	for _, sc := range []string{"", "-wal", "-shm"} {
		src := b.Path + sc
		dest, err := moveAside(fsys, src, ts)
		switch {
		case errors.Is(err, errNotExist):
			// sidecar (or file) absent — nothing to move for this suffix.
		case err != nil:
			fmt.Fprintf(out, "  ✗ %s: could not move %s — %v\n", b.Label, src, err)
			errs = append(errs, fmt.Errorf("move %s: %w", src, err))
		default:
			moved[src] = true
			created = append(created, dest)
			anyMoved = true
			fmt.Fprintf(out, "  ✓ %s: %s -> %s\n", b.Label, src, dest)
		}
	}
	if !anyMoved && len(errs) == 0 {
		fmt.Fprintf(out, "  · %s: %s — nothing to move\n", b.Label, b.Path)
	}
	return created, errs
}

// uniqueBackupPath returns base if nothing exists there, else base-1, base-2, …
// (and finally a random suffix as a safety net) so a second-resolution timestamp
// collision never overwrites an existing backup.
func uniqueBackupPath(fsys resetFS, base string) string {
	if !fsPathExists(fsys, base) {
		return base
	}
	for i := 1; i < 10000; i++ {
		cand := fmt.Sprintf("%s-%d", base, i)
		if !fsPathExists(fsys, cand) {
			return cand
		}
	}
	return fmt.Sprintf("%s-%d", base, rand.Int63())
}

// fsPathExists reports whether anything (file, dir, or symlink) is present at
// path, using lstat so a symlink counts and is never followed.
func fsPathExists(fsys resetFS, path string) bool {
	if fsys.lstat != nil {
		_, err := fsys.lstat(path)
		return err == nil
	}
	if fsys.stat != nil {
		_, err := fsys.stat(path)
		return err == nil
	}
	return false
}

// customDBOutsideRoot decides dir-move vs file-only-with-sidecars for a memory/
// knowledge db, shared by BOTH the default and --keep-memory paths so neither can
// drift. When a custom MEMORY_DB/KNOWLEDGE_DB resolves OUTSIDE the pix data
// root, its parent dir may hold unrelated files (e.g. ~/Documents), so moving the
// directory would drag them aside; return a file-only target that moves just the
// db file + its -wal/-shm sidecars. For a default in-root db it returns false and
// the caller moves the owning directory instead.
func customDBOutsideRoot(dir, dbPath, dataRoot, label string) (backupTarget, bool) {
	if dbPath != "" && !underDir(dir, dataRoot) {
		return backupTarget{Path: dbPath, Label: label, Dangerous: true, WithSidecars: true}, true
	}
	return backupTarget{}, false
}

// keepMemoryPreserve computes the captured-memory artifacts the --keep-memory
// sweep must NOT move aside, as a set of cleaned absolute top-level dataRoot
// paths, plus a human label for the summary. Two shapes:
//
//   - db in a DEDICATED subdir of dataRoot (default dataRoot/memory/memory.db):
//     preserve the top-level subdir entry on the path to it (e.g. dataRoot/memory).
//   - db sitting DIRECTLY in dataRoot (MEMORY_DB=dataRoot/custom-memory.db, so
//     memoryDir == dataRoot): preserve the db FILE + its -wal/-shm sidecars, and
//     move everything else (incl. the knowledge db) aside.
//
// Matching by cleaned path (never a hardcoded "memory" name) is what fixes the
// data-loss edge where a loose custom db in the root was swept away while the
// code still reported it preserved.
func keepMemoryPreserve(dataRoot, memoryDir, memoryDB string) (map[string]bool, string) {
	preserve := map[string]bool{}
	root := filepath.Clean(dataRoot)
	dir := filepath.Clean(memoryDir)
	db := memoryDB
	if db == "" {
		db = filepath.Join(memoryDir, "memory.db")
	}
	db = filepath.Clean(db)
	if dir != root && underDir(dir, root) {
		// Dedicated subdir: preserve the first path segment under dataRoot.
		top := topLevelUnder(root, dir)
		preserve[top] = true
		return preserve, top
	}
	// The db is a loose file in the data root (or its dir resolves to the root):
	// preserve just the db file + its -wal/-shm sidecars.
	preserve[db] = true
	preserve[db+"-wal"] = true
	preserve[db+"-shm"] = true
	return preserve, db
}

// topLevelUnder returns the cleaned path of the FIRST path segment of `path`
// beneath `root` (root/a for root/a/b/c). Used to preserve the top-level data-root
// entry that contains the memory db. Falls back to path when it is not under root.
func topLevelUnder(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.Clean(path)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	return filepath.Join(root, parts[0])
}

// underDir reports whether path is dir itself or nested inside it.
func underDir(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".." && !filepath.IsAbs(rel))
}

// serveStillUp probes whether `pix-host serve` is still answering on EITHER
// service port (memory AND knowledge, env-aware), so the executor can refuse the
// dangerous data move after a best-effort stop failed to bring it down. A
// knowledge-only serve still holds a live sqlite writer under the data root, so
// checking only the memory port would let its db be split mid-move.
func serveStillUp(env shellEnv) bool {

	if env.DialLocal(servePort(env, "MEMORY_PORT", rpc.MemoryPortDefault)) {
		return true
	}
	return env.DialLocal(servePort(env, "KNOWLEDGE_PORT", rpc.KnowledgePortDefault))
}

// executeReset performs the plan: stop services (best-effort), move each backup
// aside, sweep the data root for non-memory entries under --keep-memory, then
// run the sbx removals. It never hard-removes. Returns the created .bak paths
// (for the summary's restore hint).
// It returns the created .bak paths AND a non-nil error if ANY move-aside or
// sweep failed (or the serve-still-up guard blocked the data move). A partial
// reset must NOT report success: the caller uses the error to exit non-zero and
// to avoid reporting a partial reset as successful.
func executeReset(a resetActions, fsys resetFS, env shellEnv, out io.Writer, now func() time.Time) ([]string, error) {
	ts := now().Unix()
	var errs []error

	// Was a daemon running BEFORE we tear down? If so, we bring a fresh one up on
	// the clean slate at the end (step 5), so reset lands you running, not down.
	wasUp := serveStillUp(env)

	// 1. Best-effort stop of the host services so they don't hold the db files.
	stopHostServices(env, out)

	// 1b. Verify serve is ACTUALLY down before we move the data dir. Renaming
	// ~/.local/share/pix out from under a live sqlite writer splits the db from its wal.
	// The config-dir backup stays safe; only the DATA moves are gated. --force
	// overrides (the user accepts the risk).
	dataBlocked := false
	if !a.Force && serveStillUp(env) {
		dataBlocked = true
		msg := "serve is still running after the stop attempt — refusing to move the data directory (a live sqlite writer would be split from its db/wal); stop it with 'pix serve stop' or re-run with --force"
		fmt.Fprintf(out, "  ✗ %s\n", msg)
		errs = append(errs, errors.New(msg))
	}

	// Snapshot the 1Password ref files (op-refs.env, hostmode.env) BEFORE the
	// config dir moves aside. They are op:// POINTERS, not secrets — resolvable
	// only with the user's own 1Password — so re-pasting every one of them after
	// every reset is pure friction. They stay in the .bak too, so restoring them
	// into the fresh config dir below is purely additive, never a data loss risk.
	var refSnapshots []refFileSnapshot
	if a.PreserveRefs && a.ConfigDir != "" && fsys.readFile != nil {
		for _, name := range preservedRefFiles {
			p := filepath.Join(a.ConfigDir, name)
			data, err := fsys.readFile(p)
			if err != nil {
				continue // missing (or unreadable) — nothing to preserve
			}
			refSnapshots = append(refSnapshots, refFileSnapshot{name: name, data: data})
		}
	}

	// 2. Move each explicit backup aside.
	var created []string
	moved := map[string]bool{}
	fmt.Fprintln(out, "Backing up state (moved aside, not deleted):")
	for _, b := range a.Backups {
		if b.Dangerous && dataBlocked {
			fmt.Fprintf(out, "  · %s: %s — SKIPPED (serve still up)\n", b.Label, b.Path)
			continue
		}
		if b.WithSidecars {
			// A custom db FILE outside the data root: move the file + -wal/-shm only.
			c, e := moveFileWithSidecars(fsys, b, ts, moved, out)
			created = append(created, c...)
			errs = append(errs, e...)
			continue
		}
		dest, err := moveAside(fsys, b.Path, ts)
		switch {
		case errors.Is(err, errNotExist):
			fmt.Fprintf(out, "  · %s: %s — nothing to move\n", b.Label, b.Path)
		case err != nil:
			fmt.Fprintf(out, "  ✗ %s: could not move %s — %v\n", b.Label, b.Path, err)
			errs = append(errs, fmt.Errorf("move %s: %w", b.Path, err))
		default:
			moved[b.Path] = true
			created = append(created, dest)
			fmt.Fprintf(out, "  ✓ %s: %s -> %s\n", b.Label, b.Path, dest)
		}
	}

	// 2b. Restore the snapshotted 1Password refs into a fresh config dir, now that
	// the old one has actually moved aside. Best-effort: a restore failure is
	// reported but never fails the reset (the ref is still safe in the .bak).
	if len(refSnapshots) > 0 && moved[a.ConfigDir] {
		mkdirErr := fsys.mkdirAll(a.ConfigDir, 0o755)
		allOK := true
		for _, snap := range refSnapshots {
			if mkdirErr != nil {
				fmt.Fprintf(out, "  ✗ could not preserve %s\n", snap.name)
				allOK = false
				continue
			}
			dest := filepath.Join(a.ConfigDir, snap.name)
			if err := fsys.writeFile(dest, snap.data, 0o600); err != nil {
				fmt.Fprintf(out, "  ✗ could not preserve %s\n", snap.name)
				allOK = false
			}
		}
		if allOK {
			fmt.Fprintln(out, "  ✓ kept 1Password refs (op-refs.env, hostmode.env) so you don't re-paste")
		}
	}

	// 3. --keep-memory: sweep the data root, moving aside every top-level entry
	// that is NOT part of the captured memory (the resolved memory db + its
	// -wal/-shm sidecars when they sit directly in the data root, or the dedicated
	// memory subdir when the db lives in one) and not a backup we just created, so
	// "anything else" beside the preserved facts is reset too. Preservation is by
	// cleaned absolute path (honoring a custom MEMORY_DB), never a hardcoded name.
	// Skipped when the data move was blocked.
	if a.KeepMemory && a.DataRoot != "" && !dataBlocked {
		preserve, preservedLabel := keepMemoryPreserve(a.DataRoot, a.MemoryDir, a.MemoryDB)
		entries, rdErr := fsys.readDir(a.DataRoot)
		if rdErr != nil && !os.IsNotExist(rdErr) {
			// A real read failure (permissions, IO) MUST surface — do not report
			// preservation/success over a directory we could not even scan.
			fmt.Fprintf(out, "  ✗ could not read data dir %s — %v\n", a.DataRoot, rdErr)
			errs = append(errs, fmt.Errorf("read data dir %s: %w", a.DataRoot, rdErr))
		} else {
			for _, e := range entries {
				name := e.Name()
				p := filepath.Join(a.DataRoot, name)
				if preserve[filepath.Clean(p)] || strings.Contains(name, ".bak-") {
					continue // preserve the captured-memory artifacts
				}
				if moved[p] {
					continue // already handled (the explicit knowledge target)
				}
				dest, mErr := moveAside(fsys, p, ts)
				if mErr != nil {
					if !errors.Is(mErr, errNotExist) {
						fmt.Fprintf(out, "  ✗ could not move %s — %v\n", p, mErr)
						errs = append(errs, fmt.Errorf("move %s: %w", p, mErr))
					}
					continue
				}
				created = append(created, dest)
				fmt.Fprintf(out, "  ✓ %s -> %s\n", p, dest)
			}
			fmt.Fprintf(out, "  ✓ preserved captured memory: %s\n", preservedLabel)
		}
	}

	// 3b. Clear the daemon's ephemeral runtime files (pid/lazy/lock in the STATE
	// dir). They are stale after the stop and live OUTSIDE the moved dirs, so a
	// plain reset would strand them. Hard-remove (best-effort; a missing file is
	// fine). Skipped if the daemon wouldn't stop (data-move blocked) so we never
	// yank the pidfile out from under a still-live daemon.
	if !dataBlocked {
		for _, p := range a.RuntimeFiles {
			if err := fsys.remove(p); err != nil && !os.IsNotExist(err) {
				fmt.Fprintf(out, "  · could not clear runtime file %s — %v\n", p, err)
			}
		}
	}

	// 4. sbx: remove pix-* sandboxes + unregister the configured MCP servers.
	// Provider secrets (sbx secret) are intentionally LEFT ALONE — those are just
	// keys, not stack state, and re-entering them is friction with no upside here.
	if a.RemoveSandboxes {
		executeSbxReset(a, env, out)
	}

	// 5. Restart the daemon on the clean slate if one was running before (and we
	// actually tore down). It comes up fresh against default config (the file was
	// moved aside) with an empty store — the intended clean-slate running state.
	// Best-effort: a failure just leaves services down and the next `pix run`
	// lazy-starts them.
	if wasUp && !dataBlocked {
		fmt.Fprintln(out, "Restarting host services on the clean slate:")
		if err := restartServeForReset(out); err != nil {
			fmt.Fprintf(out, "  · could not restart services (%v) — `pix run` will start them\n", err)
		} else {
			fmt.Fprintln(out, "  ✓ host services restarted")
		}
	}
	return created, errors.Join(errs...)
}

// restartServeForReset brings a fresh daemon up after a reset, indirected through
// a package var so tests can stub it. It loads the (now-default) config and lazy-
// starts serve; ensureServe is flock-guarded and health-waited.
var restartServeForReset = func(out io.Writer) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	return ensureServe(defaultServeStarter(), cfg, ensureServeOpts{})
}

// stopServeForReset is the serve-stop the reset executor uses, indirected through
// a package var so a test can stub it (and so the real path never signals a live
// serve during unit tests). It is MODE-AWARE: a managed service (launchd/systemd)
// is stopped via its supervisor so KeepAlive/Restart= cannot respawn it mid-reset
// (which would trip the data-move guard); otherwise it falls through to the
// pidfile-based stopServe (with its discovery fallback for an orphaned daemon).
var stopServeForReset = func(out io.Writer) (bool, error) {
	return stopServeAnyMode(managedServiceActive, stopManagedService, defaultServeCtl(), out)
}

// stopHostServices best-effort stops any running `pix-host serve` so it
// releases the db files before they move. It delegates to the pidfile-based
// stopServe (which verifies the pid is ours before signalling); stopServe prints
// exactly what it did (stopped / not running / stale / refused). Best-effort: a
// hard error is reported but never aborts the reset, keeping the "cannot stop, do
// it yourself" degradation.
func stopHostServices(_ shellEnv, out io.Writer) {
	fmt.Fprintln(out, "Stopping host services:")
	if _, err := stopServeForReset(out); err != nil {
		fmt.Fprintf(out, "  · could not stop 'pix-host serve' (%v) — stop it yourself if running\n", err)
	}
}

// executeSbxReset removes pix-* sandboxes and unregisters the configured
// local MCP servers. Best-effort throughout: each action is reported, and if sbx
// is absent (e.g. inside a sandbox) it prints the commands for the user to run.
func executeSbxReset(a resetActions, env shellEnv, out io.Writer) {
	fmt.Fprintln(out, "Sandboxes + MCP (sbx):")
	haveSbx := false
	_, err := env.LookPath("sbx")
	haveSbx = err == nil
	if !haveSbx {
		fmt.Fprintln(out, "  · sbx not found — run these on your host:")
		fmt.Fprintln(out, "      sbx ls   # then: sbx rm -f <each pix-* sandbox>")
		for _, name := range a.MCPRemove {
			fmt.Fprintf(out, "      sbx mcp rm %s\n", name)
		}
		return
	}
	// Remove each pix-* sandbox parsed from `sbx ls`. The LISTING is a
	// read-only probe and BOUNDED (probeRun) — a hung sbx degrades to the
	// failed-listing message; the `sbx rm -f` removals below are mutating
	// lifecycle commands and stay on env.Run.
	if lsOut, timedOut, err := env.RunTimed("sbx", "ls"); err == nil && !timedOut {
		boxes := parseSandboxes(lsOut)
		if len(boxes) == 0 {
			fmt.Fprintln(out, "  · no pix-* sandboxes to remove")
		}
		for _, sb := range boxes {
			if _, err := env.Run("sbx", "rm", "-f", sb.Name); err != nil {
				// Removal FAILED (or unknowable): the receipt is RETAINED —
				// evidence is discarded only on positive proof of removal.
				fmt.Fprintf(out, "  ✗ sbx rm -f %s — %v\n", sb.Name, err)
			} else {
				fmt.Fprintf(out, "  ✓ removed sandbox %s\n", sb.Name)
				// Positive removal success: clear the launcher's per-sandbox MCP
				// receipt via the SAME hardened helper `pix rm`/task teardown
				// use (lock-serialized, symlink-safe). Best-effort: the removal
				// DID succeed, so a clear failure is reported, never fatal — the
				// next launcher create's pre-create clear is the backstop.
				if cerr := workspace.ClearRemovedReceipt(sb.Name); cerr != nil {
					fmt.Fprintf(out, "  · could not clear the mcp receipt for %s — %v\n", sb.Name, cerr)
				}
			}
		}
	} else {
		fmt.Fprintf(out, "  ✗ sbx ls failed — %v\n", err)
	}
	// Unregister the configured local MCP servers. The remove verb couldn't be
	// confirmed from inside a sandbox (no sbx there); `sbx mcp rm <name>` is the
	// expected form — if your sbx differs, run the printed command by hand.
	for _, name := range a.MCPRemove {
		if _, err := env.Run("sbx", "mcp", "rm", name); err != nil {
			fmt.Fprintf(out, "  ✗ sbx mcp rm %s — %v (run it yourself if the verb differs)\n", name, err)
		} else {
			fmt.Fprintf(out, "  ✓ unregistered MCP %s\n", name)
		}
	}
}

// printResetPlan shows EXACTLY what will be moved/removed before any change, so
// the guard prompt is informed.
func printResetPlan(a resetActions, out io.Writer) {
	fmt.Fprintln(out, "pix reset: moves state aside (reversible), never deletes.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Moving to <path>.bak-<timestamp> (rename back to restore):")
	for _, b := range a.Backups {
		fmt.Fprintf(out, "  - %s: %s\n", b.Label, b.Path)
	}
	if a.PreserveRefs {
		for _, b := range a.Backups {
			if b.Path == a.ConfigDir {
				fmt.Fprintln(out, "  (keeping your 1Password refs: op-refs.env, hostmode.env)")
				break
			}
		}
	}
	if a.KeepMemory {
		fmt.Fprintf(out, "  (keeping captured memory: %s)\n", a.MemoryDir)
	}
	if len(a.RuntimeFiles) > 0 {
		fmt.Fprintln(out, "Stops the daemon, clears its lock/pid files, and restarts it if it was running.")
	}
	if a.RemoveSandboxes {
		fmt.Fprintln(out, "Will remove (sbx):")
		fmt.Fprintln(out, "  - every pix-* sandbox (sbx rm -f)")
		for _, name := range a.MCPRemove {
			fmt.Fprintf(out, "  - MCP server %q (sbx mcp rm)\n", name)
		}
		fmt.Fprintln(out, "  (provider secrets are left alone)")
	}
	fmt.Fprintln(out)
}

// printResetSummary closes with the restore/cleanup guidance + next step.
func printResetSummary(created []string, out io.Writer) {
	fmt.Fprintln(out)
	if len(created) > 0 {
		fmt.Fprintln(out, "Backups created (rename any back to restore):")
		for _, p := range created {
			fmt.Fprintf(out, "  %s\n", p)
		}
		fmt.Fprintln(out, "  delete them once you're sure:  rm -rf <path>.bak-*")
		fmt.Fprintln(out, "  to restore one: `pix serve stop`, rename it back, then `pix run`.")
	} else {
		fmt.Fprintln(out, "Nothing to back up — the stack was already clean.")
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Next: pix setup")
}

// runResetCore wires plan -> guard -> execute against injected deps. It returns
// errResetNeedsYes on the non-TTY-no-yes refusal (the CLI maps it to exit 2);
// an interactive "no" aborts cleanly with a nil error.
func runResetCore(cfg *config.Config, paths resetPaths, opts resetOpts,
	fsys resetFS, env shellEnv, rio setupIO, now func() time.Time) error {

	a := resetPlan(cfg, paths, opts)
	printResetPlan(a, rio.out)
	if !opts.purgeData && paths.artifactRoot != "" {
		if _, size := artifactDirSize(paths.artifactRoot); size > 0 {
			fmt.Fprintf(rio.out, "Keeping harvested task artifacts (%s) at %s — pass --purge-data to move them aside too.\n\n",
				humanBytes(size), paths.artifactRoot)
		}
	}

	if !opts.assumeYes {
		if !rio.isTTY {
			return errResetNeedsYes
		}
		ans := strings.ToLower(promptLine(rio, "Proceed? [y/N]: "))
		if ans != "y" && ans != "yes" {
			fmt.Fprintln(rio.out, "Aborted — nothing changed.")
			return nil
		}
	}

	created, execErr := executeReset(a, fsys, env, rio.out, now)
	printResetSummary(created, rio.out)
	return execErr
}

// parseResetArgs parses the reset flag set.
func parseResetArgs(argv []string, allowSbx, allowPurge bool) (resetOpts, error) {
	var o resetOpts
	for _, a := range argv {
		switch a {
		case "-h", "--help":
			o.help = true
			return o, nil
		case "--keep-memory":
			o.keepMemory = true
		case "--yes", "-y":
			o.assumeYes = true
		case "--force":
			o.force = true
		case "--sbx":
			if !allowSbx {
				return o, fmt.Errorf("unknown flag %q", a)
			}
			o.sbx = true
		case "--purge-data":
			if !allowPurge {
				return o, fmt.Errorf("unknown flag %q", a)
			}
			o.purgeData = true
		default:
			return o, fmt.Errorf("unknown flag %q", a)
		}
	}
	return o, nil
}

// runReset is the `reset` verb entry point.
func runReset(argv []string) {
	opts, err := parseResetArgs(argv, true, true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix reset: %v\n\n%s", err, resetUsage)
		os.Exit(2)
	}
	if opts.help {
		fmt.Print(resetUsage)
		return
	}
	cfg, _, err := workspace.LoadResolvedConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix reset: %v\n", err)
		os.Exit(1)
	}
	env := defaultShellEnv()
	rio := setupIO{in: os.Stdin, out: os.Stdout, isTTY: isTTY(os.Stdin)}
	if err := runResetCore(cfg, resolveResetPaths(env), opts, defaultResetFS(), env, rio, time.Now); err != nil {
		if errors.Is(err, errResetNeedsYes) {
			fmt.Fprintln(os.Stderr, "pix reset: refusing to reset a non-interactive terminal without confirmation")
			fmt.Fprintln(os.Stderr, "re-run with --yes to reset non-interactively")
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "pix reset: %v\n", err)
		os.Exit(1)
	}
}

// resolveBinPaths returns the installed launcher symlinks (~/.local/bin/pix
// + pix-host) from the injected env's home dir.
func resolveBinPaths(env shellEnv) []string {
	home := ""
	home = env.HomeDir()
	bin := filepath.Join(home, ".local", "bin")
	return []string{
		filepath.Join(bin, "pix"),
		filepath.Join(bin, "pix-host"),
	}
}

// removeBinSymlinks removes the launcher bin entries, but ONLY when they are
// symlinks (what `make install` / install.sh create). A real regular file there
// is left untouched + reported, so we never nuke something we didn't install.
func removeBinSymlinks(bins []string, fsys resetFS, out io.Writer) {
	fmt.Fprintln(out, "Removing installed binaries:")
	for _, p := range bins {
		fi, err := fsys.lstat(p)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Fprintf(out, "  · %s — not installed\n", p)
			} else {
				fmt.Fprintf(out, "  ✗ %s — %v\n", p, err)
			}
			continue
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			fmt.Fprintf(out, "  · %s — not a symlink (not ours), left in place\n", p)
			continue
		}
		// Only remove a symlink that actually points at OUR binary — an unrelated
		// symlink that happens to sit in a bin slot is left alone + reported.
		target, lerr := fsys.readlink(p)
		if lerr != nil {
			fmt.Fprintf(out, "  ✗ %s — could not read symlink target: %v\n", p, lerr)
			continue
		}
		if !isOurBinTarget(target) {
			fmt.Fprintf(out, "  · %s -> %s — not a pix binary, left in place\n", p, target)
			continue
		}
		if err := fsys.remove(p); err != nil {
			fmt.Fprintf(out, "  ✗ %s — could not remove: %v\n", p, err)
		} else {
			fmt.Fprintf(out, "  ✓ removed symlink %s\n", p)
		}
	}
}

// isOurBinTarget reports whether a bin-slot symlink's target is one of our
// launcher binaries. The match is on the target's BASENAME being EXACTLY
// `pix` or `pix-host` — NOT a substring of the path. A deceptive
// target like /opt/pix-ish-wrapper or /repo/pix/bin/other-tool merely
// CONTAINS "pix" and must NOT be treated as ours (we'd delete an unrelated
// binary). A real repo checkout's launcher is out/pix, whose basename is
// exactly `pix`, so it still matches.
func isOurBinTarget(target string) bool {
	base := filepath.Base(target)
	return base == "pix" || base == "pix-host"
}

// runUninstallCore runs the full reset, then removes the bin symlinks. Split for
// testability (temp HOME, injected bins/fs/env). It returns runResetCore's error
// (notably errResetNeedsYes) unchanged so the CLI maps it to the same exit code.
func runUninstallCore(cfg *config.Config, paths resetPaths, bins []string, opts resetOpts, prov provenance,
	fsys resetFS, env shellEnv, rio setupIO, now func() time.Time) error {

	a := resetPlan(cfg, paths, opts)
	printResetPlan(a, rio.out)
	if prov.Channel == channelHomebrew {
		fmt.Fprintln(rio.out, "Homebrew owns the binaries and man page; pix will not remove them directly.")
	} else {
		fmt.Fprintln(rio.out, "Will also remove the installed pix + pix-host bin symlinks.")
	}
	// Harvested task artifacts are user work product and are NOT removed by
	// default (they live under XDG_DATA_HOME, outside the reset trees). Print where
	// they are + how big so they stay findable; --purge-data is the deliberate
	// opt-in that moves them aside too (added to the plan above).
	if !opts.purgeData && paths.artifactRoot != "" {
		if _, size := artifactDirSize(paths.artifactRoot); size > 0 {
			fmt.Fprintf(rio.out, "Keeping harvested task artifacts (%s) at %s — pass --purge-data to remove them too.\n",
				humanBytes(size), paths.artifactRoot)
		}
	}
	fmt.Fprintln(rio.out)

	if !opts.assumeYes {
		if !rio.isTTY {
			return errResetNeedsYes
		}
		ans := strings.ToLower(promptLine(rio, "Proceed? [y/N]: "))
		if ans != "y" && ans != "yes" {
			fmt.Fprintln(rio.out, "Aborted — nothing changed.")
			return nil
		}
	}

	created, execErr := executeReset(a, fsys, env, rio.out, now)
	if execErr != nil {
		// The state backup failed (or was blocked) — do NOT remove the bin symlinks.
		// Stranding the user with no binaries after a failed backup is the worst
		// outcome; leave the working install in place so they can retry.
		fmt.Fprintln(rio.out, "Reset backup failed — leaving the pix + pix-host bin symlinks in place (not uninstalling).")
		printResetSummary(created, rio.out)
		return execErr
	}
	if prov.Channel == channelHomebrew {
		fmt.Fprintln(rio.out, "Binaries and man page are owned by Homebrew, not pix.")
		fmt.Fprintln(rio.out, "State and managed services were removed first. Finish with:")
		fmt.Fprintln(rio.out, "  brew uninstall mcavage/tap/pix")
		fmt.Fprintln(rio.out, "Removing the formula first leaves launchd configured with a Cellar path that fails on its next launch.")
		if rio.isTTY {
			ans := strings.ToLower(promptLine(rio, "Run brew uninstall now? [y/N]: "))
			if ans == "y" || ans == "yes" {
				if err := env.RunInteractive("brew", "uninstall", "mcavage/tap/pix"); err != nil {
					return fmt.Errorf("brew uninstall failed: %w", err)
				}
			}
		}
	} else {
		removeBinSymlinks(bins, fsys, rio.out)
		removeInstalledManPage(env, fsys, rio.out)
	}
	printResetSummary(created, rio.out)
	return nil
}

// removeInstalledManPage removes the man page `make install` drops on the user
// manpath (~/.local/share/man/man1/pix.1) — and ONLY that file. The embed
// in the binary remains the guarantee, so this is best-effort cleanup: a missing
// file is fine and never fails the uninstall.
func removeInstalledManPage(env shellEnv, fsys resetFS, out io.Writer) {
	home := ""
	home = env.HomeDir()
	p := filepath.Join(home, ".local", "share", "man", "man1", "pix.1")
	if _, err := fsys.lstat(p); err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(out, "  ✗ %s — %v\n", p, err)
		}
		return
	}
	if err := fsys.remove(p); err != nil {
		fmt.Fprintf(out, "  ✗ %s — could not remove: %v\n", p, err)
		return
	}
	fmt.Fprintf(out, "  ✓ removed man page %s\n", p)
}
