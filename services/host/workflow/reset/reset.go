package reset

import (
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/service"
	"pix/host/sys"
	"pix/host/workspace"
)

// reset is the destructive-but-reversible lifecycle verb: nothing is ever
// hard-removed. State is MOVED aside to a timestamped `<dir>.bak-<unixts>`
// sibling, so a reset can always be undone by renaming it back. The stack it
// resets is what the stack now IS — the config dir, the data root (captured
// memory), and the daemon's state-dir runtime files.

// Opts is the parsed reset flag set.
type Opts struct {
	keepMemory bool // --keep-memory: preserve <dataRoot>/memory (captured facts)
	sbx        bool // --sbx: also remove pix-* sandboxes + unregister MCP
	assumeYes  bool // --yes: don't prompt (required on a non-TTY)
	force      bool // --force: move the data dir even if serve appears still up
	Help       bool // -h/--help
}

// Paths are the resolved host locations reset acts on. Split out so the pure
// planner takes them injected (a test supplies temp-dir paths, no real $HOME
// lookup). MemoryDir honors MEMORY_DB.
type Paths struct {
	ConfigDir string // ~/.config/pix (config.toml, op-refs.env)
	DataRoot  string // ~/.local/share/pix (memory/, packs/, context/)
	MemoryDir string // <dataRoot>/memory or dir(MEMORY_DB): the user's captured facts
	memoryDB  string // the custom MEMORY_DB file path (set ONLY when MEMORY_DB is given); "" for the default
	// PidFile is the daemon's pidfile (RuntimeFiles[0]), named separately because
	// it is not just a file to clear: it is the ONLY thing that answers "is a
	// daemon running, and is it ours" — see probeServeUp.
	PidFile string
	// RuntimeFiles are the daemon's ephemeral state-dir files (pid/lazy/lock),
	// resolved HERE (from the same injected env every other field derives from)
	// rather than by Plan reaching for config.ServePidPath() et al. globally —
	// see resolveStateDir. A test host's fake $HOME/$XDG_STATE_HOME must be the
	// only thing that decides where these point, never the real process env.
	RuntimeFiles []string
}

// backupTarget is one path the reset moves aside, with a human label.
type backupTarget struct {
	Path  string
	Label string
	// Dangerous marks a move that must not run while `serve` is still up: moving
	// the live data dir out from under a sqlite writer splits the db from its wal.
	Dangerous bool
	// WithSidecars marks a target that is a DB FILE (not a directory): move only
	// that file plus its -wal/-shm sidecars, never its parent dir. Used for a
	// custom MEMORY_DB that lives OUTSIDE the pix data root, whose parent is the
	// user's own directory and none of reset's business.
	WithSidecars bool
}

// actions is the pure plan: exactly what will be moved + which sbx actions
// run. It carries no side effects; executeReset consumes it.
type actions struct {
	Backups         []backupTarget // paths to move to <path>.bak-<ts>
	KeepMemory      bool           // preserve MemoryDir (sweep DataRoot minus memory)
	MemoryDir       string         // preserved dir when KeepMemory
	MemoryDB        string         // resolved custom MEMORY_DB file path ("" for the default), so the sweep can preserve a db that lives DIRECTLY in DataRoot
	DataRoot        string         // the data root (for the keep-memory sweep)
	ConfigDir       string         // the config dir (~/.config/pix) being backed up
	RemoveSandboxes bool           // --sbx: remove pix-* sandboxes
	MCPRemove       []string       // --sbx: MCP server names to unregister (cfg.MCP)
	Force           bool           // --force: skip the serve-still-up guard on the DATA move only (never on the runtime files)
	RuntimeFiles    []string       // ephemeral daemon runtime files to HARD-remove (pid/lazy/lock in StateDir) — stale after the stop, not worth a .bak
	PidFile         string         // the pidfile the liveness probe classifies (Paths.PidFile)
}

// resetFS is the injected filesystem surface, so executeReset stays hermetic in
// tests (a temp HOME, no real rm). DefaultResetFS wires the os-backed ops.
type resetFS struct {
	stat    func(path string) (os.FileInfo, error)
	lstat   func(path string) (os.FileInfo, error)
	rename  func(oldpath, newpath string) error
	readDir func(path string) ([]os.DirEntry, error)
	remove  func(path string) error
}

func DefaultResetFS() resetFS {
	return resetFS{
		stat:    os.Stat,
		lstat:   os.Lstat,
		rename:  os.Rename,
		readDir: os.ReadDir,
		remove:  os.Remove,
	}
}

// ErrResetNeedsYes is returned by RunCore when it can't prompt (non-TTY)
// and --yes was not given. The CLI wrapper maps it to exit 2. errNotExist is the
// internal "nothing to move" signal from moveAside.
var (
	ErrResetNeedsYes = errors.New("reset needs --yes on a non-interactive terminal")
	errNotExist      = errors.New("path does not exist")
)

// resolveConfigDir mirrors config.configDir()'s precedence (PIX_CONFIG's parent
// dir, else $XDG_CONFIG_HOME/pix, else ~/.config/pix) but reads it through the
// INJECTED sys.System. Calling config.Path() instead would read os.Getenv/
// os.UserHomeDir and silently ignore the host a caller injected — for a verb
// that MOVES DIRECTORIES that is the difference between a test's temp tree and
// the operator's real one. sys.Real resolves byte-identically to config.Path().
func resolveConfigDir(env sys.System) string {
	if p := strings.TrimSpace(env.Getenv("PIX_CONFIG")); p != "" {
		return filepath.Dir(p)
	}
	if xdg := strings.TrimSpace(env.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, "pix")
	}
	if home := env.HomeDir(); home != "" {
		return filepath.Join(home, ".config", "pix")
	}
	// Mirrors config.Path()'s fallback when the home dir can't be resolved:
	// config.Path() returns the bare relative "config.toml", whose dir is ".".
	return "."
}

// resolveStateDir mirrors config.StateDir()'s precedence ($XDG_STATE_HOME/pix,
// else ~/.local/state/pix) through the injected env — see resolveConfigDir for
// why. An empty result mirrors config.StateDir()'s unresolvable-home case.
func resolveStateDir(env sys.System) string {
	if xdg := strings.TrimSpace(env.Getenv("XDG_STATE_HOME")); xdg != "" {
		return filepath.Join(xdg, "pix")
	}
	if home := env.HomeDir(); home != "" {
		return filepath.Join(home, ".local", "state", "pix")
	}
	return ""
}

// stateFilePath joins name under stateDir, or returns name bare when stateDir
// couldn't be resolved — matching config.ServePidPath() et al.'s fallback.
func stateFilePath(stateDir, name string) string {
	if stateDir == "" {
		return name
	}
	return filepath.Join(stateDir, name)
}

// ResolveResetPaths resolves the host paths reset touches from the injected env:
// MEMORY_DB honored, the data root from $XDG_DATA_HOME/pix else
// ~/.local/share/pix, the config dir per resolveConfigDir. KNOWLEDGE_DB is NOT
// read — the built-in knowledge service is retired, so a leftover value points
// at a file that is now the user's own.
func ResolveResetPaths(env sys.System) Paths {
	dataRoot := filepath.Join(env.HomeDir(), ".local", "share", "pix")
	if xdg := strings.TrimSpace(env.Getenv("XDG_DATA_HOME")); xdg != "" {
		dataRoot = filepath.Join(xdg, "pix")
	}
	memoryDir := filepath.Join(dataRoot, "memory")
	memoryDB := ""
	// Normalize a custom db path to ABSOLUTE at the source so every downstream
	// comparison (dir-move vs file-only, the --keep-memory preserve set, the
	// sweep) agrees on one spelling of it.
	if db := strings.TrimSpace(env.Getenv("MEMORY_DB")); db != "" {
		if abs, err := filepath.Abs(db); err == nil {
			db = abs
		}
		memoryDir = filepath.Dir(db)
		memoryDB = db
	}
	stateDir := resolveStateDir(env)
	pidFile := stateFilePath(stateDir, "serve.pid")
	return Paths{
		ConfigDir: resolveConfigDir(env),
		DataRoot:  dataRoot,
		MemoryDir: memoryDir,
		memoryDB:  memoryDB,
		PidFile:   pidFile,
		RuntimeFiles: []string{
			pidFile,
			stateFilePath(stateDir, "serve.lazy"),
			stateFilePath(stateDir, "serve.spawn.lock"),
		},
	}
}

// plan is the PURE planner: it resolves the backup targets + sbx actions from
// the config, paths, and opts — no filesystem, no exec. The config dir is always
// backed up. Without --keep-memory the whole data root (captured memory
// included) goes too; with it, only the config dir is an explicit target and
// executeReset's sweep moves the data root's non-memory entries.
func plan(cfg *config.Config, paths Paths, opts Opts) actions {
	a := actions{
		KeepMemory: opts.keepMemory,
		MemoryDir:  paths.MemoryDir,
		MemoryDB:   paths.memoryDB,
		DataRoot:   paths.DataRoot,
		ConfigDir:  paths.ConfigDir,
		Force:      opts.force,
	}
	if paths.ConfigDir != "" {
		a.Backups = append(a.Backups, backupTarget{Path: paths.ConfigDir, Label: "config"})
	}
	if !opts.keepMemory && paths.DataRoot != "" {
		a.Backups = append(a.Backups, backupTarget{Path: paths.DataRoot, Label: "data (memory, packs, context)", Dangerous: true})
		// A custom MEMORY_DB can live OUTSIDE the data root, in a directory that is
		// the user's own: move the db FILE plus its -wal/-shm sidecars, never the
		// parent dir or an unrelated sibling.
		if paths.memoryDB != "" && !underDir(paths.MemoryDir, paths.DataRoot) {
			a.Backups = append(a.Backups, backupTarget{
				Path: paths.memoryDB, Label: "memory database", Dangerous: true, WithSidecars: true})
		}
	}
	if opts.sbx {
		a.RemoveSandboxes = true
		a.MCPRemove = append([]string(nil), cfg.MCP...)
	}
	// The daemon's ephemeral runtime files live in the STATE dir (not the config/
	// data dirs we move aside), so a plain reset would leave them behind. They are
	// stale the moment serve stops (a lock file, a pidfile, a lazy marker) and have
	// already been resolved (from the SAME injected env every other Paths field
	// came from) in ResolveResetPaths — not re-derived here from config's
	// globals, which would reach past whatever host the caller injected.
	a.RuntimeFiles = paths.RuntimeFiles
	a.PidFile = paths.PidFile
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
// sidecars are a soft skip; a real move error is collected.
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

// keepMemoryPreserve computes the captured-memory artifacts the --keep-memory
// sweep must NOT move aside, as a set of cleaned absolute top-level dataRoot
// paths, plus a human label for the summary. Two shapes: a dedicated memory
// subdir, or a loose db file (+ sidecars) sitting in the data root itself.
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

// probeServeUp answers "is a `pix-host serve` daemon running right now" from the
// daemon's IDENTITY: a loaded managed unit, or the pidfile — the one resolved from
// the INJECTED env, never config's globals — naming a live process that is not
// provably a stranger's. It is deliberately NOT a MEMORY_PORT health probe, which
// gets this wrong in both directions: silent for a monitor-only or
// memory-crashed daemon (reset would stop it and never bring it back) and
// answering for any stranger holding :11435 (reset would "restart" a daemon that
// never ran, and would trust a stranger's silence to mean our data is safe to
// move). settle > 0 waits, bounded, for a just-stopped daemon to actually exit.
// Indirected through a package var so tests drive both answers without a daemon
// on the developer's machine.
var probeServeUp = func(pidPath string, settle time.Duration) (up bool, pid int) {
	return service.ServeIdentityUp(service.ManagedActive, pidPath, settle)
}

// executeReset performs the plan: stop services (best-effort), move each backup
// aside, sweep the data root for non-memory entries under --keep-memory, clear
// the state-dir runtime files, then run the sbx removals. Nothing is ever
// hard-removed except those runtime files. Returns the created .bak paths.
func executeReset(a actions, fsys resetFS, env sys.System, out io.Writer, now func() time.Time) ([]string, error) {
	ts := now().Unix()
	var errs []error

	// Was a daemon running BEFORE we tear down? Asked of its identity, so a
	// monitor-only or memory-crashed daemon still counts as running: it gets a
	// fresh one back on the clean slate at the end (step 5) instead of being
	// silently stopped for good.
	wasUp, _ := probeServeUp(a.PidFile, 0)

	// 1. Best-effort stop of the host services so they don't hold the db files.
	stopped, stopErr := stopHostServices(out)
	// A stop that actually SIGNALLED something is itself proof a daemon was
	// running: the pidfile-less ORPHAN (a previous reset moved the pidfile aside
	// while the daemon kept running) is invisible to the probe above but is exactly
	// what stop's discovery finds and kills — and it deserves the same restart.
	wasUp = wasUp || stopped

	// 1b. PROVE the daemon is down before anything destructive. Two independent
	// facts can deny it: the stop itself failed (a refusal, a signal error), and
	// the post-stop identity probe still finding a live process. Only a
	// proven-dead daemon may have its data dir renamed (doing that under a live
	// sqlite writer splits the db from its wal) or its runtime files deleted
	// (deleting the pidfile of a LIVE daemon orphans it from the only handle
	// `pix serve stop` has on it).
	//
	// A stop returns once the signal is delivered / the unit booted out, not once
	// the process is reaped, so give it a bounded moment to actually exit.
	const stopSettle = 2 * time.Second
	stillUp, stillPid := probeServeUp(a.PidFile, stopSettle)
	serveDown := !stillUp && stopErr == nil
	dataBlocked := !serveDown && !a.Force
	if !serveDown {
		why := fmt.Sprintf("could not confirm 'pix-host serve' is down (the stop attempt failed: %v)", stopErr)
		if stillUp {
			why = "serve is STILL running after the stop attempt"
			if stillPid > 0 {
				why = fmt.Sprintf("serve is STILL running (pid %d) after the stop attempt", stillPid)
			}
		}
		if dataBlocked {
			msg := why + " — refusing to move the data directory (a live sqlite writer would be split from its db/wal); stop it with 'pix serve stop' or re-run with --force"
			fmt.Fprintf(out, "  ✗ %s\n", msg)
			errs = append(errs, errors.New(msg))
		} else {
			// --force is an override of the DATA move only. The runtime files stay
			// (step 3b) either way: the live daemon keeps its pidfile.
			fmt.Fprintf(out, "  · %s — --force: moving state anyway, keeping its pid/lock files so `pix serve stop` can still reach it\n", why)
		}
	}

	// 2. Move each explicit backup aside. The config dir's 1Password op:// refs go
	// with it: reset is a clean slate, and the refs stay recoverable in the .bak.

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
	// -wal/-shm sidecars when they sit directly in the data root, else the
	// dedicated memory subdir).
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
	// plain reset would strand them. Hard-removed best-effort: a missing file is
	// the expected case, and a stale lock is worth more noise than a .bak.
	//
	// Gated on serveDown, NOT on dataBlocked: while a daemon is still alive those
	// files are not stale, they are its live state — removing the pidfile orphans
	// it from `pix serve stop` (AGENTS.md invariant #4). --force does not buy this.
	if serveDown {
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
	if wasUp && serveDown {
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
// starts serve; service.Ensure is flock-guarded and health-waited.
var restartServeForReset = func(out io.Writer) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	return service.Ensure(service.DefaultStarter(), cfg, service.EnsureOpts{})
}

// stopServeForReset is the serve-stop the reset executor uses, indirected through
// a package var so a test can stub it (and so the real path never signals a live
// serve during unit tests). It is MODE-AWARE: a managed service (launchd) is
// stopped THROUGH its supervisor, never by a bare SIGTERM KeepAlive would undo.
var stopServeForReset = func(out io.Writer) (bool, error) {
	return service.StopAnyMode(service.ManagedActive, service.StopManaged, service.DefaultCtl(), out)
}

// stopHostServices best-effort stops any running `pix-host serve` so it releases
// the db files before they move, verifying the pid is ours before signalling. It
// RETURNS its outcome instead of only printing it: a failed stop gates every
// destructive step below, and printing it while carrying on is exactly how a live
// daemon's data got moved out from under it. The bool means "we signalled
// something of ours and it exited" (evidence a daemon WAS running); whether one
// SURVIVED is the identity probe's question, never stop's — "nothing of ours to
// stop" is the normal answer on a clean machine.
func stopHostServices(out io.Writer) (bool, error) {
	fmt.Fprintln(out, "Stopping host services:")
	stopped, err := stopServeForReset(out)
	if err != nil {
		fmt.Fprintf(out, "  · could not stop 'pix-host serve' (%v) — stop it yourself if running\n", err)
	}
	return stopped, err
}

// executeSbxReset removes pix-* sandboxes and unregisters the configured
// local MCP servers. Best-effort throughout: each action is reported, and if sbx
// is absent (e.g. inside a sandbox) it prints the commands for the user to run.
func executeSbxReset(a actions, env sys.System, out io.Writer) {
	fmt.Fprintln(out, "Sandboxes + MCP (sbx):")
	if _, err := env.LookPath("sbx"); err != nil {
		fmt.Fprintln(out, "  · sbx not found — run these on your host:")
		fmt.Fprintln(out, "      sbx ls   # then: sbx rm -f <each pix-* sandbox>")
		for _, name := range a.MCPRemove {
			fmt.Fprintf(out, "      sbx mcp rm %s\n", name)
		}
		return
	}
	// Remove each pix-* sandbox parsed from `sbx ls`. The LISTING is a read-only
	// probe and BOUNDED — a hung sbx degrades to the failed-listing message.
	if lsOut, timedOut, err := env.RunTimed("sbx", "ls"); err == nil && !timedOut {
		boxes := workspace.ParseSandboxes(lsOut)
		if len(boxes) == 0 {
			fmt.Fprintln(out, "  · no pix-* sandboxes to remove")
		}
		for _, sb := range boxes {
			if _, err := env.Run("sbx", "rm", "-f", sb.Name); err != nil {
				fmt.Fprintf(out, "  ✗ sbx rm -f %s — %v\n", sb.Name, err)
			} else {
				fmt.Fprintf(out, "  ✓ removed sandbox %s\n", sb.Name)
			}
		}
	} else {
		fmt.Fprintf(out, "  ✗ sbx ls failed — %v\n", err)
	}
	// Unregister the configured local MCP servers. `sbx mcp rm <name>` is the
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
func printResetPlan(a actions, out io.Writer) {
	fmt.Fprintln(out, "pix reset: moves state aside (reversible), never deletes.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Moving to <path>.bak-<timestamp> (rename back to restore):")
	for _, b := range a.Backups {
		fmt.Fprintf(out, "  - %s: %s\n", b.Label, b.Path)
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

// RunCore wires plan -> guard -> execute against injected deps. It returns
// ErrResetNeedsYes on the non-TTY-no-yes refusal (the CLI maps it to exit 2);
// an interactive "no" aborts cleanly with a nil error.
func RunCore(cfg *config.Config, paths Paths, opts Opts,
	fsys resetFS, env sys.System, rio cli.IO, now func() time.Time) error {

	a := plan(cfg, paths, opts)
	printResetPlan(a, rio.Out)

	if !opts.assumeYes {
		if !rio.IsTTY {
			return ErrResetNeedsYes
		}
		ans := strings.ToLower(cli.PromptLine(rio, "Proceed? [y/N]: "))
		if ans != "y" && ans != "yes" {
			fmt.Fprintln(rio.Out, "Aborted — nothing changed.")
			return nil
		}
	}

	created, execErr := executeReset(a, fsys, env, rio.Out, now)
	printResetSummary(created, rio.Out)
	return execErr
}

// NewOpts carries the parsed flag set in (the flag DECLARATION is the CLI's).
func NewOpts(keepMemory, sbx, assumeYes, force bool) Opts {
	return Opts{keepMemory: keepMemory, sbx: sbx, assumeYes: assumeYes, force: force}
}
