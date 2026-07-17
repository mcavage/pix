package main

import (
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"pi-stack/host/config"
)

// reset + uninstall are the destructive-but-reversible lifecycle verbs. Both are
// built to be SAFE: nothing is ever hard-removed. State is MOVED aside to a
// timestamped `<dir>.bak-<unixts>` sibling, so a reset can always be undone by
// renaming the backup back. reset wipes the stack's *state* (config + data),
// uninstall does that AND removes the installed bin symlinks.
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

// resetOpts is the parsed reset/uninstall flag set.
type resetOpts struct {
	keepMemory bool // --keep-memory: preserve <dataRoot>/memory (captured facts)
	sbx        bool // --sbx: also remove pi-stack-* sandboxes + unregister MCP
	assumeYes  bool // --yes: don't prompt (required on a non-TTY)
	force      bool // --force: move the data dir even if serve appears still up
	help       bool // -h/--help
}

// resetPaths are the resolved host locations reset acts on. Split out so the
// pure planner takes them injected (a test supplies temp-dir paths, no real
// $HOME lookup). memoryDir/knowledgeDir honor MEMORY_DB/KNOWLEDGE_DB.
//
// The XDG reconciliation split storage across three bases, so reset sweeps all
// of them: configDir (CONFIG), dataDir (DATA: memory/knowledge/backups), stateDir
// (STATE: index/caches/tasks/serve.pid), PLUS the legacy dataRoot (~/.pi-stack)
// and any legacy config-sibling strays a pre-XDG install left behind.
type resetPaths struct {
	configDir    string         // ~/.config/pi-stack (config.toml, op-refs.env, broker-token, legacy knowledge/, knowledge-cache/)
	dataRoot     string         // legacy ~/.pi-stack (memory/ + knowledge/ + backups/)
	dataDir      string         // new DATA base (config.DataDir(): memory/ + knowledge/ + backups/)
	stateDir     string         // new STATE base (config.StateDir(): knowledge/ index + knowledge-cache/ + tasks/ + serve.pid)
	memoryDir    string         // <dataRoot>/memory or dir(MEMORY_DB): the user's captured facts
	knowledgeDir string         // <dataRoot>/knowledge or dir(KNOWLEDGE_DB): the rebuildable index
	memoryDB     string         // the custom MEMORY_DB file path (set ONLY when MEMORY_DB is given); "" for the default
	knowledgeDB  string         // the custom KNOWLEDGE_DB file path (set ONLY when KNOWLEDGE_DB is given); "" for the default
	legacyStrays []backupTarget // pre-XDG config-sibling strays (~/.config/pi-stack/{knowledge,knowledge-cache,serve.pid}) when configDir moved elsewhere
}

// sweepRoot is one directory the --keep-memory sweep walks top-level, moving
// every entry aside EXCEPT the memory authorities named by its preserve inputs.
// One per storage root that may hold BOTH memory and regenerable data (the new
// DATA base and the legacy ~/.pi-stack), so captured facts survive in every
// location while indexes/caches/backups are reset.
type sweepRoot struct {
	Root          string   // dir to sweep
	MemoryDir     string   // the memory dir/db to preserve (keepMemoryPreserve input)
	MemoryDB      string   // custom MEMORY_DB when loose in Root (keepMemoryPreserve input)
	ExtraPreserve []string // additional explicit paths to always preserve (e.g. Root/memory)
	PreserveGlobs []string // globs to preserve (e.g. Root/memory.pre-xdg-* safety copies)
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
	// custom MEMORY_DB/KNOWLEDGE_DB that lives OUTSIDE the pi-stack data root (its
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
	RemoveSandboxes bool           // --sbx: remove pi-stack-* sandboxes
	MCPRemove       []string       // --sbx: MCP server names to unregister (cfg.MCP)
	Force           bool           // --force: skip the serve-still-up guard on the data move
	SweepRoots      []sweepRoot    // --keep-memory: roots to sweep (memory preserved), one per DATA + legacy root
}

// resetFS is the injected filesystem surface, so executeReset stays hermetic in
// tests (a temp HOME, no real rm). defaultResetFS wires the os-backed ops.
type resetFS struct {
	stat     func(path string) (os.FileInfo, error)
	lstat    func(path string) (os.FileInfo, error)
	readlink func(path string) (string, error)
	rename   func(oldpath, newpath string) error
	readDir  func(path string) ([]os.DirEntry, error)
	remove   func(path string) error
}

func defaultResetFS() resetFS {
	return resetFS{
		stat:     os.Stat,
		lstat:    os.Lstat,
		readlink: os.Readlink,
		rename:   os.Rename,
		readDir:  os.ReadDir,
		remove:   os.Remove,
	}
}

// errResetNeedsYes is returned by runResetCore when it can't prompt (non-TTY)
// and --yes was not given. The CLI wrapper maps it to exit 2. errNotExist is the
// internal "nothing to move" signal from moveAside.
var (
	errResetNeedsYes = errors.New("reset needs --yes on a non-interactive terminal")
	errNotExist      = errors.New("path does not exist")
)

// resolveResetPaths resolves the host paths reset touches from the injected env
// (MEMORY_DB/KNOWLEDGE_DB honored; the data root defaults to ~/.pi-stack, the
// config dir to config.Path()'s parent).
func resolveResetPaths(env shellEnv) resetPaths {
	home := ""
	if env.homeDir != nil {
		home = env.homeDir()
	}
	dataRoot := filepath.Join(home, ".pi-stack")
	memoryDir := filepath.Join(dataRoot, "memory")
	knowledgeDir := filepath.Join(dataRoot, "knowledge")
	memoryDB := ""
	knowledgeDB := ""
	if env.getenv != nil {
		// Normalize custom db paths to ABSOLUTE at the source so every downstream
		// comparison (dir-move vs file-only, the --keep-memory preserve set, the
		// sweep) agrees. A relative MEMORY_DB (e.g. run from $HOME with
		// MEMORY_DB=.pi-stack/x.db) would otherwise leave relative preserve keys
		// that never match the sweep's absolute entries -> memory swept aside.
		if db := strings.TrimSpace(env.getenv("MEMORY_DB")); db != "" {
			if abs, err := filepath.Abs(db); err == nil {
				db = abs
			}
			memoryDir = filepath.Dir(db)
			memoryDB = db
		}
		if db := strings.TrimSpace(env.getenv("KNOWLEDGE_DB")); db != "" {
			if abs, err := filepath.Abs(db); err == nil {
				db = abs
			}
			knowledgeDir = filepath.Dir(db)
			knowledgeDB = db
		}
	}
	configDir := filepath.Dir(config.Path())
	dataDir, _ := config.DataDir()
	stateDir, _ := config.StateDir()

	// Pre-XDG installs that used a CUSTOM config dir (PI_STACK_CONFIG / XDG_CONFIG_HOME)
	// may still have knowledge/knowledge-cache/serve.pid under the LITERAL
	// ~/.config/pi-stack. When the config dir moved elsewhere, moving it aside
	// misses those strays, so target them explicitly. When configDir IS the literal
	// path they travel with the config-dir move and this list stays empty.
	var legacyStrays []backupTarget
	if home != "" {
		legacyConfig := filepath.Join(home, ".config", "pi-stack")
		if filepath.Clean(configDir) != filepath.Clean(legacyConfig) {
			legacyStrays = []backupTarget{
				{Path: filepath.Join(legacyConfig, "knowledge"), Label: "legacy knowledge bundle"},
				{Path: filepath.Join(legacyConfig, "knowledge-cache"), Label: "legacy knowledge cache"},
				{Path: filepath.Join(legacyConfig, "serve.pid"), Label: "legacy serve pidfile"},
			}
		}
	}

	return resetPaths{
		configDir:    configDir,
		dataRoot:     dataRoot,
		dataDir:      dataDir,
		stateDir:     stateDir,
		memoryDir:    memoryDir,
		knowledgeDir: knowledgeDir,
		memoryDB:     memoryDB,
		knowledgeDB:  knowledgeDB,
		legacyStrays: legacyStrays,
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
	// A custom MEMORY_DB can resolve UNDER any of the XDG bases (CONFIG/STATE),
	// not just the data roots. When --keep-memory is set and the resolved memory
	// authority lives under a base we would otherwise move aside WHOLESALE
	// (configDir, stateDir), we must instead SWEEP that base (preserving memory)
	// so the captured facts survive wherever MEMORY_DB points.
	configHoldsMemory := opts.keepMemory && memoryAuthorityUnder(paths.configDir, paths.memoryDir, paths.memoryDB)
	if paths.configDir != "" && !configHoldsMemory {
		a.Backups = append(a.Backups, backupTarget{Path: paths.configDir, Label: "config directory"})
	}
	// Pre-XDG config-sibling strays (only populated when the config dir moved
	// elsewhere). Normally none hold captured memory, so they move aside wholesale
	// in BOTH plans. But a custom MEMORY_DB can resolve UNDER a stray dir (e.g.
	// MEMORY_DB=~/.config/pi-stack/knowledge/memory.db); under --keep-memory such a
	// stray is SWEPT instead (keepMemorySweepRoots adds it) so the authority is
	// preserved rather than dragged aside on a successful flock (R3-1).
	for _, s := range paths.legacyStrays {
		if opts.keepMemory && memoryAuthorityUnder(s.Path, paths.memoryDir, paths.memoryDB) {
			continue
		}
		a.Backups = append(a.Backups, s)
	}
	if opts.keepMemory {
		// Preserve the captured facts (memory) in EVERY location; move the
		// regenerable STATE (index, caches, tasks, serve.pid) aside wholesale, and
		// SWEEP the DATA roots (legacy ~/.pi-stack + new DATA base) so knowledge +
		// backups reset while memory survives. When a custom MEMORY_DB lives under
		// STATE, sweep STATE instead of moving it wholesale (keepMemorySweepRoots
		// adds it as a sweep root) so the db is never dragged aside.
		if paths.stateDir != "" && !memoryAuthorityUnder(paths.stateDir, paths.memoryDir, paths.memoryDB) {
			a.Backups = append(a.Backups, backupTarget{Path: paths.stateDir, Label: "state directory (index, caches, tasks)", Dangerous: true})
		}
		// A custom KNOWLEDGE_DB that lives OUTSIDE the data root must move ONLY the
		// db file + its -wal/-shm sidecars, NEVER its parent dir (KNOWLEDGE_DB=
		// ~/Documents/knowledge.db must not drag all of ~/Documents aside) — the same
		// custom-outside-root handling the default path uses. A default in-root index
		// is moved as a whole directory.
		if t, ok := customDBOutsideRoot(paths.knowledgeDir, paths.knowledgeDB, paths.dataRoot, "knowledge database"); ok {
			a.Backups = append(a.Backups, t)
		} else if paths.knowledgeDir != "" && !memoryAuthorityUnder(paths.knowledgeDir, paths.memoryDir, paths.memoryDB) {
			// A custom MEMORY_DB can nest UNDER the knowledge dir (e.g.
			// MEMORY_DB=<dataRoot>/knowledge/memory.db). Moving the knowledge dir
			// wholesale would drag that live db aside on a successful flock (R3-1), so
			// when it holds the authority SWEEP it instead (keepMemorySweepRoots adds
			// it) — the index resets while the nested memory db stays put.
			a.Backups = append(a.Backups, backupTarget{Path: paths.knowledgeDir, Label: "knowledge database", Dangerous: true})
		}
		a.SweepRoots = keepMemorySweepRoots(paths)
	} else {
		// Default reset: move every base aside wholesale (memory included).
		if paths.dataRoot != "" {
			a.Backups = append(a.Backups, backupTarget{Path: paths.dataRoot, Label: "data directory (memory + knowledge)", Dangerous: true})
		}
		if paths.dataDir != "" && filepath.Clean(paths.dataDir) != filepath.Clean(paths.dataRoot) {
			a.Backups = append(a.Backups, backupTarget{Path: paths.dataDir, Label: "data directory (XDG: memory + knowledge + backups)", Dangerous: true})
		}
		if paths.stateDir != "" && filepath.Clean(paths.stateDir) != filepath.Clean(paths.dataRoot) && filepath.Clean(paths.stateDir) != filepath.Clean(paths.dataDir) {
			a.Backups = append(a.Backups, backupTarget{Path: paths.stateDir, Label: "state directory (index, caches, tasks)", Dangerous: true})
		}
		// Honor a custom MEMORY_DB / KNOWLEDGE_DB that lives OUTSIDE the data root:
		// the data-root move alone would miss it. Move ONLY the db FILE + its
		// -wal/-shm sidecars, NEVER the whole parent dir. A whole directory is moved
		// only for the pi-stack-owned default dir, handled by the data-root move above.
		if t, ok := customDBOutsideRoot(paths.memoryDir, paths.memoryDB, paths.dataRoot, "memory database"); ok {
			a.Backups = append(a.Backups, t)
		}
		if t, ok := customDBOutsideRoot(paths.knowledgeDir, paths.knowledgeDB, paths.dataRoot, "knowledge database"); ok {
			a.Backups = append(a.Backups, t)
		}
	}
	// Fail-closed protection (R2-1): the resolved memory authority (its dir, or a
	// custom MEMORY_DB file) can live UNDER a target we would otherwise treat as
	// SAFE — the config dir, or a legacy config-sibling stray. Moving such a target
	// while a serve holds the memory flock would split the live sqlite writer from
	// its db/wal, exactly what the Dangerous flag guards. So mark ANY target that
	// CONTAINS the memory authority Dangerous, routing it through executeReset's
	// fail-closed flock-acquire refusal + the serveStillUp guard. Data/state moves
	// are already Dangerous; this closes the config-dir and stray gap.
	for i := range a.Backups {
		if memoryAuthorityUnder(a.Backups[i].Path, paths.memoryDir, paths.memoryDB) {
			a.Backups[i].Dangerous = true
		}
	}
	if opts.sbx {
		a.RemoveSandboxes = true
		a.MCPRemove = append([]string(nil), cfg.MCP...)
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
// drift. When a custom MEMORY_DB/KNOWLEDGE_DB resolves OUTSIDE the pi-stack data
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
//     memoryDir == dataRoot): preserve the db FILE + its -wal/-shm sidecars AND
//     the sibling .memory.lock (all top-level entries in the sweep root), and
//     move everything else (incl. the knowledge db) aside.
//
// Either way the .memory.lock flock file next to the db is preserved IN PLACE: in
// the subdir case it rides along inside the preserved subdir; in the loose case it
// is a top-level sweep-root entry that must be named explicitly. Sweeping it aside
// while reset still holds the flock on that inode would let a concurrent
// serve/restore create a fresh lock at the canonical path and acquire it, breaking
// the held-flock exclusion (R4-1).
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
	// preserve the db file + its -wal/-shm sidecars, plus the sibling .memory.lock
	// so the flock reset holds is never swept out from under it (R4-1).
	preserve[db] = true
	preserve[db+"-wal"] = true
	preserve[db+"-shm"] = true
	preserve[filepath.Join(filepath.Dir(db), ".memory.lock")] = true
	return preserve, db
}

// keepMemorySweepRoots builds the set of roots the --keep-memory sweep walks, one
// per location that may hold BOTH captured memory and regenerable data: the legacy
// ~/.pi-stack, the new DATA base, and — when a custom MEMORY_DB resolves under
// them — the STATE and CONFIG bases, the knowledge dir, and any legacy config-
// sibling stray too. Each root always preserves its own
// `memory` subdir + any `memory.pre-xdg-*` safety copy migration left behind; a
// root that actually holds the resolved (possibly custom) memory authority ALSO
// preserves that exact db/dir. So captured facts survive in EVERY location
// (finding 10, and a custom MEMORY_DB under any XDG base) while knowledge +
// backups reset. Dedup keeps two coincident bases from sweeping twice.
func keepMemorySweepRoots(paths resetPaths) []sweepRoot {
	var roots []sweepRoot
	seen := map[string]bool{}
	add := func(root string) {
		if root == "" {
			return
		}
		c := filepath.Clean(root)
		if seen[c] {
			return
		}
		seen[c] = true
		sr := sweepRoot{
			Root:          root,
			MemoryDir:     filepath.Join(root, "memory"),
			ExtraPreserve: []string{filepath.Join(root, "memory")},
			PreserveGlobs: []string{filepath.Join(root, "memory.pre-xdg-*")},
		}
		// When the resolved memory authority (a custom MEMORY_DB dir/file) lives under
		// this root, preserve THAT exact location, not just the default `memory` subdir.
		if memoryAuthorityUnder(root, paths.memoryDir, paths.memoryDB) {
			sr.MemoryDir = paths.memoryDir
			sr.MemoryDB = paths.memoryDB
		}
		roots = append(roots, sr)
	}
	add(paths.dataRoot)
	add(paths.dataDir)
	// STATE/CONFIG are normally moved aside WHOLESALE; sweep them only when a custom
	// MEMORY_DB lives under them, so the db is preserved rather than dragged aside.
	if memoryAuthorityUnder(paths.stateDir, paths.memoryDir, paths.memoryDB) {
		add(paths.stateDir)
	}
	if memoryAuthorityUnder(paths.configDir, paths.memoryDir, paths.memoryDB) {
		add(paths.configDir)
	}
	// The knowledge dir + legacy config-sibling strays are normally moved aside
	// WHOLESALE; sweep them only when a custom MEMORY_DB nests under one, so the
	// authority is preserved rather than dragged aside on a successful flock (R3-1).
	if memoryAuthorityUnder(paths.knowledgeDir, paths.memoryDir, paths.memoryDB) {
		add(paths.knowledgeDir)
	}
	for _, s := range paths.legacyStrays {
		if memoryAuthorityUnder(s.Path, paths.memoryDir, paths.memoryDB) {
			add(s.Path)
		}
	}
	return roots
}

// memoryAuthorityUnder reports whether the resolved memory authority (its
// directory, or a custom MEMORY_DB file) lives at or under root. Used to decide
// whether a base must be SWEPT (memory preserved) rather than moved aside
// wholesale under --keep-memory.
func memoryAuthorityUnder(root, memoryDir, memoryDB string) bool {
	if root == "" {
		return false
	}
	if memoryDB != "" && underDir(memoryDB, root) {
		return true
	}
	return memoryDir != "" && underDir(memoryDir, root)
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

// acquireMemoryFlock takes the EXCLUSIVE, NON-BLOCKING memory flock and HOLDS it
// (returning a release func) so `reset` can move the data dir with NO serve able
// to start mid-move (LOW-2). ok=false means the lock could NOT be cleanly
// acquired, so reset must refuse the dangerous data move (unless --force). Unlike
// the memoryFlockHeld probe it CREATES the lock file/dir if absent, because reset
// must own the lock for the whole move rather than merely detect contention.
//
// It FAILS CLOSED: ANY acquisition error — not just EWOULDBLOCK (a running serve
// holds it), but also a mkdir/open/flock failure — yields ok=false. A failed
// acquire means we canNOT prove no writer is present, and moving a live sqlite db
// out from under a writer splits it from its wal, so treating an error as "go
// ahead" (the old fail-OPEN behavior) risked exactly the corruption this lock
// guards against. Only a clean acquire returns ok=true with a real release; on
// refusal the release is nil (the caller never calls it when !ok).
func acquireMemoryFlock() (release func(), ok bool) {
	path := config.MemoryLockPath()
	if path == "" {
		return nil, false
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		// EWOULDBLOCK (held by a running serve) AND any other flock error both
		// refuse: we cannot prove the db is safe to move.
		return nil, false
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, true
}

// serveStillUp probes whether `pi-stack-host serve` is still answering on EITHER
// service port (memory AND knowledge, env-aware), so the executor can refuse the
// dangerous data move after a best-effort stop failed to bring it down. A
// knowledge-only serve still holds a live sqlite writer under the data root, so
// checking only the memory port would let its db be split mid-move.
func serveStillUp(env shellEnv) bool {
	if env.dial == nil {
		return false
	}
	if env.dial(servePort(env, "MEMORY_PORT", memoryPortDefault)) {
		return true
	}
	return env.dial(servePort(env, "KNOWLEDGE_PORT", knowledgePortDefault))
}

// executeReset performs the plan: stop services (best-effort), move each backup
// aside, sweep the data root for non-memory entries under --keep-memory, then
// run the sbx removals. It never hard-removes. Returns the created .bak paths
// (for the summary's restore hint).
// It returns the created .bak paths AND a non-nil error if ANY move-aside or
// sweep failed (or the serve-still-up guard blocked the data move). A partial
// reset must NOT report success: the caller uses the error to exit non-zero and
// (for uninstall) to abort removing the bin symlinks.
func executeReset(a resetActions, fsys resetFS, env shellEnv, out io.Writer, now func() time.Time) ([]string, error) {
	ts := now().Unix()
	var errs []error

	// 1. Best-effort stop of the host services so they don't hold the db files.
	stopHostServices(env, out)

	// 1b. Verify serve is ACTUALLY down before we move the data dir. Renaming
	// ~/.pi-stack out from under a live sqlite writer splits the db from its wal.
	// The config-dir backup stays safe; only the DATA moves are gated. --force
	// overrides (the user accepts the risk).
	// LOW-2: ACQUIRE AND HOLD the memory flock across the ENTIRE data move (the
	// daemon takes the same lock before binding its port), so a serve cannot start
	// mid-move and split a live sqlite writer from its db/wal. serveStillUp's port
	// probe stays a secondary signal. The lock is released (deferred) after every
	// move completes. --force overrides both guards.
	dataBlocked := false
	if !a.Force {
		acquire := env.memLockAcquire
		if acquire == nil {
			acquire = func() (func(), bool) { return func() {}, true }
		}
		rel, gotLock := acquire()
		switch {
		case !gotLock:
			dataBlocked = true
		case serveStillUp(env):
			rel() // acquired, but the port probe still sees serve — drop it and refuse
			dataBlocked = true
		default:
			defer rel() // HOLD it until executeReset returns, after every move
		}
		if dataBlocked {
			msg := "serve is still running (or the memory database is locked) after the stop attempt — refusing to move the data directory (a live sqlite writer would be split from its db/wal); stop it with 'pi-stack serve stop' or re-run with --force"
			fmt.Fprintf(out, "  ✗ %s\n", msg)
			errs = append(errs, errors.New(msg))
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

	// 3. --keep-memory: sweep the data root, moving aside every top-level entry
	// that is NOT part of the captured memory (the resolved memory db + its
	// -wal/-shm sidecars when they sit directly in the data root, or the dedicated
	// memory subdir when the db lives in one) and not a backup we just created, so
	// "anything else" beside the preserved facts is reset too. Preservation is by
	// cleaned absolute path (honoring a custom MEMORY_DB), never a hardcoded name.
	// Skipped when the data move was blocked.
	if a.KeepMemory && !dataBlocked {
		for _, sr := range a.SweepRoots {
			if sr.Root == "" {
				continue
			}
			preserve, preservedLabel := keepMemoryPreserve(sr.Root, sr.MemoryDir, sr.MemoryDB)
			for _, extra := range sr.ExtraPreserve {
				preserve[filepath.Clean(extra)] = true
			}
			for _, g := range sr.PreserveGlobs {
				if matches, gErr := filepath.Glob(g); gErr == nil {
					for _, mm := range matches {
						preserve[filepath.Clean(mm)] = true
					}
				}
			}
			entries, rdErr := fsys.readDir(sr.Root)
			if rdErr != nil && !os.IsNotExist(rdErr) {
				// A real read failure (permissions, IO) MUST surface — do not report
				// preservation/success over a directory we could not even scan.
				fmt.Fprintf(out, "  ✗ could not read data dir %s — %v\n", sr.Root, rdErr)
				errs = append(errs, fmt.Errorf("read data dir %s: %w", sr.Root, rdErr))
				continue
			}
			for _, e := range entries {
				name := e.Name()
				p := filepath.Join(sr.Root, name)
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

	// 4. sbx: remove pi-stack-* sandboxes + unregister the configured MCP servers.
	// Provider secrets (sbx secret) are intentionally LEFT ALONE — those are just
	// keys, not stack state, and re-entering them is friction with no upside here.
	if a.RemoveSandboxes {
		executeSbxReset(a, env, out)
	}
	return created, errors.Join(errs...)
}

// stopServeForReset is the serve-stop the reset executor uses, indirected through
// a package var so a test can stub it (and so the real path never signals a live
// serve during unit tests). It defaults to the pidfile-based stopServe — the SAFE
// replacement for the old `pkill -f 'pi-stack-host serve'`.
var stopServeForReset = func(out io.Writer) (bool, error) {
	return stopServe(defaultServeCtl(), out)
}

// stopHostServices best-effort stops any running `pi-stack-host serve` so it
// releases the db files before they move. It delegates to the pidfile-based
// stopServe (which verifies the pid is ours before signalling); stopServe prints
// exactly what it did (stopped / not running / stale / refused). Best-effort: a
// hard error is reported but never aborts the reset, keeping the "cannot stop, do
// it yourself" degradation.
func stopHostServices(_ shellEnv, out io.Writer) {
	fmt.Fprintln(out, "Stopping host services:")
	if _, err := stopServeForReset(out); err != nil {
		fmt.Fprintf(out, "  · could not stop 'pi-stack-host serve' (%v) — stop it yourself if running\n", err)
	}
}

// executeSbxReset removes pi-stack-* sandboxes and unregisters the configured
// local MCP servers. Best-effort throughout: each action is reported, and if sbx
// is absent (e.g. inside a sandbox) it prints the commands for the user to run.
func executeSbxReset(a resetActions, env shellEnv, out io.Writer) {
	fmt.Fprintln(out, "Sandboxes + MCP (sbx):")
	haveSbx := false
	if env.lookPath != nil {
		_, err := env.lookPath("sbx")
		haveSbx = err == nil
	}
	if !haveSbx {
		fmt.Fprintln(out, "  · sbx not found — run these on your host:")
		fmt.Fprintln(out, "      sbx ls   # then: sbx rm -f <each pi-stack-* sandbox>")
		for _, name := range a.MCPRemove {
			fmt.Fprintf(out, "      sbx mcp rm %s\n", name)
		}
		return
	}
	// Remove each pi-stack-* sandbox parsed from `sbx ls`.
	if lsOut, err := env.run("sbx", "ls"); err == nil {
		boxes := parseSandboxes(lsOut)
		if len(boxes) == 0 {
			fmt.Fprintln(out, "  · no pi-stack-* sandboxes to remove")
		}
		for _, sb := range boxes {
			if _, err := env.run("sbx", "rm", "-f", sb.Name); err != nil {
				fmt.Fprintf(out, "  ✗ sbx rm -f %s — %v\n", sb.Name, err)
			} else {
				fmt.Fprintf(out, "  ✓ removed sandbox %s\n", sb.Name)
			}
		}
	} else {
		fmt.Fprintf(out, "  ✗ sbx ls failed — %v\n", err)
	}
	// Unregister the configured local MCP servers. The remove verb couldn't be
	// confirmed from inside a sandbox (no sbx there); `sbx mcp rm <name>` is the
	// expected form — if your sbx differs, run the printed command by hand.
	for _, name := range a.MCPRemove {
		if _, err := env.run("sbx", "mcp", "rm", name); err != nil {
			fmt.Fprintf(out, "  ✗ sbx mcp rm %s — %v (run it yourself if the verb differs)\n", name, err)
		} else {
			fmt.Fprintf(out, "  ✓ unregistered MCP %s\n", name)
		}
	}
}

// printResetPlan shows EXACTLY what will be moved/removed before any change, so
// the guard prompt is informed.
func printResetPlan(a resetActions, out io.Writer) {
	fmt.Fprintln(out, "pi-stack reset — moves state aside (reversible), never hard-deletes.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Will move aside (to <path>.bak-<timestamp>):")
	for _, b := range a.Backups {
		fmt.Fprintf(out, "  - %s: %s\n", b.Label, b.Path)
	}
	if a.KeepMemory {
		fmt.Fprintf(out, "  (preserving captured memory: %s)\n", a.MemoryDir)
	}
	if a.RemoveSandboxes {
		fmt.Fprintln(out, "Will remove (sbx):")
		fmt.Fprintln(out, "  - every pi-stack-* sandbox (sbx rm -f)")
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
	} else {
		fmt.Fprintln(out, "Nothing to back up — the stack was already clean.")
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Next: pi-stack setup")
}

// runResetCore wires plan -> guard -> execute against injected deps. It returns
// errResetNeedsYes on the non-TTY-no-yes refusal (the CLI maps it to exit 2);
// an interactive "no" aborts cleanly with a nil error.
func runResetCore(cfg *config.Config, paths resetPaths, opts resetOpts,
	fsys resetFS, env shellEnv, rio setupIO, now func() time.Time) error {

	a := resetPlan(cfg, paths, opts)
	printResetPlan(a, rio.out)

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

// parseResetArgs parses the reset/uninstall flag set (shared: both accept
// --keep-memory / --yes; reset also accepts --sbx).
func parseResetArgs(argv []string, allowSbx bool) (resetOpts, error) {
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
		default:
			return o, fmt.Errorf("unknown flag %q", a)
		}
	}
	return o, nil
}

// runReset is the `reset` verb entry point.
func runReset(argv []string) {
	opts, err := parseResetArgs(argv, true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack reset: %v\n\n%s", err, resetUsage)
		os.Exit(2)
	}
	if opts.help {
		fmt.Print(resetUsage)
		return
	}
	cfg, _, err := loadResolvedConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack reset: %v\n", err)
		os.Exit(1)
	}
	env := defaultShellEnv()
	rio := setupIO{in: os.Stdin, out: os.Stdout, isTTY: isTTY(os.Stdin)}
	if err := runResetCore(cfg, resolveResetPaths(env), opts, defaultResetFS(), env, rio, time.Now); err != nil {
		if errors.Is(err, errResetNeedsYes) {
			fmt.Fprintln(os.Stderr, "pi-stack reset: refusing to reset a non-interactive terminal without confirmation")
			fmt.Fprintln(os.Stderr, "re-run with --yes to reset non-interactively")
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "pi-stack reset: %v\n", err)
		os.Exit(1)
	}
}

// resolveBinPaths returns the installed launcher symlinks (~/.local/bin/pi-stack
// + pi-stack-host) from the injected env's home dir.
func resolveBinPaths(env shellEnv) []string {
	home := ""
	if env.homeDir != nil {
		home = env.homeDir()
	}
	bin := filepath.Join(home, ".local", "bin")
	return []string{
		filepath.Join(bin, "pi-stack"),
		filepath.Join(bin, "pi-stack-host"),
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
			fmt.Fprintf(out, "  · %s -> %s — not a pi-stack binary, left in place\n", p, target)
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
// `pi-stack` or `pi-stack-host` — NOT a substring of the path. A deceptive
// target like /opt/pi-stack-ish-wrapper or /repo/pi-stack/bin/other-tool merely
// CONTAINS "pi-stack" and must NOT be treated as ours (we'd delete an unrelated
// binary). A real repo checkout's launcher is out/pi-stack, whose basename is
// exactly `pi-stack`, so it still matches.
func isOurBinTarget(target string) bool {
	base := filepath.Base(target)
	return base == "pi-stack" || base == "pi-stack-host"
}

// runUninstallCore runs the full reset, then removes the bin symlinks. Split for
// testability (temp HOME, injected bins/fs/env). It returns runResetCore's error
// (notably errResetNeedsYes) unchanged so the CLI maps it to the same exit code.
func runUninstallCore(cfg *config.Config, paths resetPaths, bins []string, opts resetOpts,
	fsys resetFS, env shellEnv, rio setupIO, now func() time.Time) error {

	a := resetPlan(cfg, paths, opts)
	printResetPlan(a, rio.out)
	fmt.Fprintln(rio.out, "Will also remove the installed pi-stack + pi-stack-host bin symlinks.")
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
		fmt.Fprintln(rio.out, "Reset backup failed — leaving the pi-stack + pi-stack-host bin symlinks in place (not uninstalling).")
		printResetSummary(created, rio.out)
		return execErr
	}
	removeBinSymlinks(bins, fsys, rio.out)
	removeInstalledManPage(env, fsys, rio.out)
	printResetSummary(created, rio.out)
	return nil
}

// removeInstalledManPage removes the man page `make install` drops on the user
// manpath (~/.local/share/man/man1/pi-stack.1) — and ONLY that file. The embed
// in the binary remains the guarantee, so this is best-effort cleanup: a missing
// file is fine and never fails the uninstall.
func removeInstalledManPage(env shellEnv, fsys resetFS, out io.Writer) {
	home := ""
	if env.homeDir != nil {
		home = env.homeDir()
	}
	p := filepath.Join(home, ".local", "share", "man", "man1", "pi-stack.1")
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

// runUninstall is the `uninstall` verb entry point (replaces the old stub).
func runUninstall(argv []string) {
	opts, err := parseResetArgs(argv, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack uninstall: %v\n\n%s", err, uninstallUsage)
		os.Exit(2)
	}
	if opts.help {
		fmt.Print(uninstallUsage)
		return
	}
	cfg, _, err := loadResolvedConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack uninstall: %v\n", err)
		os.Exit(1)
	}
	env := defaultShellEnv()
	rio := setupIO{in: os.Stdin, out: os.Stdout, isTTY: isTTY(os.Stdin)}
	if err := runUninstallCore(cfg, resolveResetPaths(env), resolveBinPaths(env), opts,
		defaultResetFS(), env, rio, time.Now); err != nil {
		if errors.Is(err, errResetNeedsYes) {
			fmt.Fprintln(os.Stderr, "pi-stack uninstall: refusing to run non-interactively without confirmation")
			fmt.Fprintln(os.Stderr, "re-run with --yes to uninstall non-interactively")
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "pi-stack uninstall: %v\n", err)
		os.Exit(1)
	}
}
