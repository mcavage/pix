// Package reset is `pix reset`: the clean-slate verb. It puts a host back to
// what a fresh `pix setup` would find, without hard-deleting anything a user
// cannot regenerate.
//
// The disposal rule is the whole design, and it is ONE rule, deliberately:
// every directory reset touches is MOVED ASIDE to a timestamped
// `<path>.bak-<unixts>` sibling, never removed. Renaming it back is a complete
// undo, and that is what makes this verb safe to type. Three dirs go:
//
//   - the config dir (config.toml, op-refs.env, pack-trust.json)
//   - the data root (memory, packs, personal context)
//   - the state dir (the daemon's pidfile/lazy marker/spawn lock/unit
//     snapshot/log, the per-sandbox lease records, the teardown journal)
//
// The state dir looks like pure ephemera and is NOT: `<state>/tasks/<repo>/co/
// <name>` holds real git CHECKOUTS, which can carry uncommitted work. That one
// fact is why this verb has no `rm -rf` in it at all. An enumerated "these
// files are safe to delete" list would have to be re-audited every time
// something new starts writing under the state dir, and the day it falls behind
// it eats someone's work; a rename never can, whatever ends up in there.
//
// Sandboxes go through the CALLER's injected sweep, never a local `sbx rm -f`
// loop: removal stays proof-gated (`pix rm --all`, which needs a
// kernel-verified zero-reference proof per sandbox), so reset can never become
// a second force-removal seam beside the one explicitly-named one.
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
)

// Opts is the parsed reset flag set. The DEFAULT is the whole stack — state,
// memory, config and sandboxes — because that is what someone typing `pix
// reset` is asking for; every flag here only ever makes it do LESS.
type Opts struct {
	keepMemory    bool // --keep-memory: preserve the captured-memory store
	keepSandboxes bool // --keep-sandboxes: leave this host's pix-* boxes alone
	assumeYes     bool // --yes: don't prompt (required on a non-TTY)
	force         bool // --force: move the data dir even if serve won't confirm down
}

// NewOpts carries the parsed flag set in (the flag DECLARATION is the CLI's).
func NewOpts(keepMemory, keepSandboxes, assumeYes, force bool) Opts {
	return Opts{keepMemory: keepMemory, keepSandboxes: keepSandboxes, assumeYes: assumeYes, force: force}
}

// Sweep is the injected sandbox teardown: `pix rm --all` in the command layer's
// words. It is a PARAMETER rather than an import because this package is L3 and
// workflow/launch is its L3 sibling (see arch_test.go's sideways ban) — and
// because the property that matters, "reset removes sandboxes exactly the way
// `pix rm` does, proof-gated and never forced", is best guaranteed by calling
// the same code rather than by a second implementation promising to match it.
type Sweep func(out, errOut io.Writer) error

// Runtime is the injected outside world: the filesystem ops, the host, where
// output goes, the sandbox sweep, and the clock. Bundled so a test drives the
// whole verb against a temp dir with no daemon, no sbx and no real $HOME.
type Runtime struct {
	FS    resetFS
	Env   sys.System
	IO    cli.IO
	ErrW  io.Writer // stderr, for the sandbox sweep's own refusals
	Sweep Sweep
	Now   func() time.Time
}

// Paths are the resolved host locations reset acts on. Split out so the pure
// planner takes them injected (a test supplies temp-dir paths, no real $HOME
// lookup). MemoryDir honors MEMORY_DB.
type Paths struct {
	ConfigDir string // ~/.config/pix (config.toml, op-refs.env, pack-trust.json)
	DataRoot  string // ~/.local/share/pix (memory/, packs/, context/, default/)
	MemoryDir string // <dataRoot>/memory or dir(MEMORY_DB): the user's captured facts
	memoryDB  string // the custom MEMORY_DB file path (set ONLY when MEMORY_DB is given); "" for the default
	// StateDir is ~/.local/state/pix: the daemon's runtime files, the
	// per-sandbox lease records, AND the task checkouts (see the package doc for
	// why that last one makes this a move, not a delete).
	StateDir string
	// PidFile is the daemon's pidfile, named separately because it is not just a
	// file to clear: it is the ONLY thing that answers "is a daemon running, and
	// is it ours" — see probeServeUp.
	PidFile string
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

// actions is the pure plan: exactly what will be moved, cleared and removed. It
// carries no side effects; execute consumes it.
type actions struct {
	Backups         []backupTarget // paths to move to <path>.bak-<ts>
	KeepMemory      bool           // preserve MemoryDir (sweep DataRoot minus memory)
	MemoryDir       string         // preserved dir when KeepMemory
	MemoryDB        string         // resolved custom MEMORY_DB file path ("" for the default), so the sweep can preserve a db that lives DIRECTLY in DataRoot
	DataRoot        string         // the data root (for the keep-memory sweep)
	StateDir        string         // the runtime dir, moved aside under its OWN gate (serve must be proven down; --force does not buy it)
	PidFile         string         // the pidfile the liveness probe classifies (Paths.PidFile)
	RemoveSandboxes bool           // sweep this host's pix-* sandboxes (default; --keep-sandboxes turns it off)
	MCPRemove       []string       // MCP server names to unregister from the sbx gateway (cfg.MCP)
	Force           bool           // --force: skip the serve-still-up guard on the DATA move only
}

// resetFS is the injected filesystem surface, so execute stays hermetic in
// tests (a temp HOME, no real rm). DefaultResetFS wires the os-backed ops.
// There is no remove/removeAll here, and that absence is the package's safety
// property rather than an omission: rename is the only destructive-looking op
// reset owns, so no bug in this file can delete a byte of anyone's work.
type resetFS struct {
	stat    func(path string) (os.FileInfo, error)
	lstat   func(path string) (os.FileInfo, error)
	rename  func(oldpath, newpath string) error
	readDir func(path string) ([]os.DirEntry, error)
}

func DefaultResetFS() resetFS {
	return resetFS{
		stat:    os.Stat,
		lstat:   os.Lstat,
		rename:  os.Rename,
		readDir: os.ReadDir,
	}
}

// ErrNeedsYes is returned by Run when it can't prompt (non-TTY) and --yes was
// not given. The CLI wrapper maps it to exit 2. errNotExist is the internal
// "nothing to move" signal from moveAside.
var (
	ErrNeedsYes = errors.New("reset needs --yes on a non-interactive terminal")
	errNotExist = errors.New("path does not exist")
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

// ResolvePaths resolves the host paths reset touches from the injected env:
// MEMORY_DB honored, the data root from $XDG_DATA_HOME/pix else
// ~/.local/share/pix, the config dir per resolveConfigDir, the state dir per
// resolveStateDir.
func ResolvePaths(env sys.System) Paths {
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
	pidFile := "serve.pid"
	if stateDir != "" {
		pidFile = filepath.Join(stateDir, "serve.pid")
	}
	return Paths{
		ConfigDir: resolveConfigDir(env),
		DataRoot:  dataRoot,
		MemoryDir: memoryDir,
		memoryDB:  memoryDB,
		StateDir:  stateDir,
		PidFile:   pidFile,
	}
}

// plan is the PURE planner: it resolves the backup targets, the clear target
// and the sbx actions from the config, paths and opts — no filesystem, no exec.
// The config dir is always backed up. Without --keep-memory the whole data root
// (captured memory included) goes too; with it, only the config dir is an
// explicit target and execute's sweep moves the data root's non-memory entries.
func plan(cfg *config.Config, paths Paths, opts Opts) actions {
	a := actions{
		KeepMemory: opts.keepMemory,
		MemoryDir:  paths.MemoryDir,
		MemoryDB:   paths.memoryDB,
		DataRoot:   paths.DataRoot,
		StateDir:   paths.StateDir,
		PidFile:    paths.PidFile,
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
	if !opts.keepSandboxes {
		a.RemoveSandboxes = true
		if cfg != nil {
			a.MCPRemove = append([]string(nil), cfg.MCP...)
		}
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
			fmt.Fprintf(out, "  x %s: could not move %s (%v)\n", b.Label, src, err)
			errs = append(errs, fmt.Errorf("move %s: %w", src, err))
		default:
			moved[src] = true
			created = append(created, dest)
			anyMoved = true
			fmt.Fprintf(out, "  ok %s: %s -> %s\n", b.Label, src, dest)
		}
	}
	if !anyMoved && len(errs) == 0 {
		fmt.Fprintf(out, "  . %s: %s (nothing to move)\n", b.Label, b.Path)
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

// resettableStateDir guards the state-dir move: it must be provably the dir
// this program owns, an ABSOLUTE path whose last component is "pix" (every
// branch of resolveStateDir produces exactly that). A relative or oddly-named
// answer — the shape an unresolvable $HOME produces, where config.StateDir()
// itself falls back to bare filenames — moves NOTHING rather than renaming a
// directory that might be the user's own.
func resettableStateDir(dir string) bool {
	if strings.TrimSpace(dir) == "" {
		return false
	}
	clean := filepath.Clean(dir)
	return filepath.IsAbs(clean) && filepath.Base(clean) == "pix"
}

// probeServeUp answers "is a `pix-host serve` daemon running right now" from the
// daemon's IDENTITY: a loaded managed unit, or the pidfile — the one resolved from
// the INJECTED env, never config's globals — naming a live process that is not
// provably a stranger's. It is deliberately NOT a MEMORY_PORT health probe, which
// gets this wrong in both directions: silent for a memory-crashed daemon (reset
// would stop it and never bring it back) and answering for any stranger holding
// :11435 (reset would "restart" a daemon that never ran, and would trust a
// stranger's silence to mean our data is safe to move). settle > 0 waits,
// bounded, for a just-stopped daemon to actually exit. Indirected through a
// package var so tests drive both answers without a daemon on the machine.
var probeServeUp = func(pidPath string, settle time.Duration) (up bool, pid int) {
	return service.ServeIdentityUp(service.ManagedActive, pidPath, settle)
}

// stopServeForReset is the serve-stop the executor uses, indirected through a
// package var so a test can stub it (and so the real path never signals a live
// serve during unit tests). It is MODE-AWARE: a managed service (launchd) is
// stopped THROUGH its supervisor, never by a bare SIGTERM KeepAlive would undo
// (AGENTS.md invariant #3).
var stopServeForReset = func(out io.Writer) (bool, error) {
	return service.StopAnyMode(service.ManagedActive, service.StopManaged, service.DefaultCtl(), out)
}

// restartServeForReset brings a fresh daemon up after a reset, indirected
// through a package var so tests can stub it. It loads the (now-default) config
// and lazy-starts serve; service.Ensure is flock-guarded and health-waited.
var restartServeForReset = func(out io.Writer) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	return service.Ensure(service.DefaultStarter(out), cfg, service.EnsureOpts{})
}

// execute performs the plan, in the one order the dependencies allow:
//
//  1. sandboxes, FIRST — their teardown reads the per-sandbox lease records
//     that live in the state dir this same run is about to move, so sweeping
//     afterwards would remove boxes with no reference proof to consult.
//  2. stop the daemon, and PROVE it is down.
//  3. move the config dir and data root aside.
//  4. move the state dir aside, under its own serve-must-be-down gate.
//  5. unregister the MCP servers the moved-aside config declared.
//  6. restart the daemon on the clean slate if one was running before.
//
// It returns the created .bak paths.
func execute(a actions, rt Runtime, out io.Writer) ([]string, error) {
	ts := rt.Now().Unix()
	var errs []error

	// 1. Sandboxes, before anything touches the state dir their lease records
	// live in. Provider secrets (sbx secret) are intentionally LEFT ALONE — those
	// are keys, not stack state, and re-entering them is friction with no upside.
	if a.RemoveSandboxes {
		if err := removeSandboxes(a, rt, out); err != nil {
			errs = append(errs, err)
		}
	}

	// Was a daemon running BEFORE we tear down? Asked of its identity, so a
	// memory-crashed daemon still counts as running: it gets a fresh one back on
	// the clean slate at the end (step 6) instead of being silently stopped for
	// good.
	wasUp, _ := probeServeUp(a.PidFile, 0)

	// 2. Best-effort stop of the host services so they don't hold the db files.
	// A stop that actually SIGNALLED something is itself proof a daemon was
	// running: the pidfile-less ORPHAN (a previous reset cleared the pidfile
	// while the daemon kept running) is invisible to the probe above but is
	// exactly what stop's discovery finds and kills — and it deserves the same
	// restart.
	stopped, stopErr := stopHostServices(out)
	wasUp = wasUp || stopped

	// PROVE the daemon is down before anything destructive. Two independent
	// facts can deny it: the stop itself failed, and the post-stop identity probe
	// still finding a live process. Only a proven-dead daemon may have its data
	// dir renamed (doing that under a live sqlite writer splits the db from its
	// wal) or its state dir cleared (deleting the pidfile of a LIVE daemon
	// orphans it from the only handle `pix serve stop` has on it — AGENTS.md
	// invariant #4).
	//
	// A stop returns once the signal is delivered / the unit is booted out, not
	// once the process is reaped, so give it a bounded moment to actually exit.
	// A launchd boot-out returns once the unit is unloaded, NOT once the process
	// has exited — and this daemon has a sqlite writer to close and a supervised
	// go-plugin child to reap, so it can outlive the stop by several seconds. Two
	// seconds was not enough on a real machine: reset refused to move the data
	// directory, printed "serve is STILL running (pid N)", and the pid was gone
	// moments later — leaving a HALF reset (config moved, data and state kept).
	//
	// The probe POLLS at 100ms and returns the instant the process is gone, so a
	// generous ceiling costs nothing when the exit is quick; it is only ever paid
	// by the case that would otherwise fail.
	const stopSettle = 15 * time.Second
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
			// Name the WHOLE command. "or re-run with --force" reads as a flag
			// on the command just named, and the first person to hit this typed
			// `pix serve stop --force` twice — which is not a flag that exists.
			msg := why + ": refusing to move the data directory (a live sqlite writer would be" +
				" split from its db/wal). Run `pix serve stop`, then `pix reset` again — or" +
				" `pix reset --force` to move it anyway"
			fmt.Fprintf(out, "  x %s\n", msg)
			errs = append(errs, errors.New(msg))
		} else {
			// --force overrides the DATA move only. The state dir stays (step 4)
			// either way: a live daemon keeps its pidfile.
			fmt.Fprintf(out, "  . %s: --force, moving state anyway and keeping its pid/lock files so `pix serve stop` can still reach it\n", why)
		}
	}

	// 3. Move each durable target aside. The config dir's 1Password op:// refs go
	// with it: reset is a clean slate, and the refs stay recoverable in the .bak.
	var created []string
	moved := map[string]bool{}
	fmt.Fprintln(out, "Backing up state (moved aside, not deleted):")
	for _, b := range a.Backups {
		if b.Dangerous && dataBlocked {
			fmt.Fprintf(out, "  . %s: %s SKIPPED (serve still up)\n", b.Label, b.Path)
			continue
		}
		if b.WithSidecars {
			// A custom db FILE outside the data root: move the file + -wal/-shm only.
			c, e := moveFileWithSidecars(rt.FS, b, ts, moved, out)
			created = append(created, c...)
			errs = append(errs, e...)
			continue
		}
		dest, err := moveAside(rt.FS, b.Path, ts)
		switch {
		case errors.Is(err, errNotExist):
			fmt.Fprintf(out, "  . %s: %s (nothing to move)\n", b.Label, b.Path)
		case err != nil:
			fmt.Fprintf(out, "  x %s: could not move %s (%v)\n", b.Label, b.Path, err)
			errs = append(errs, fmt.Errorf("move %s: %w", b.Path, err))
		default:
			moved[b.Path] = true
			created = append(created, dest)
			fmt.Fprintf(out, "  ok %s: %s -> %s\n", b.Label, b.Path, dest)
		}
	}

	// --keep-memory: sweep the data root, moving aside every top-level entry that
	// is NOT part of the captured memory (the resolved memory db + its -wal/-shm
	// sidecars when they sit directly in the data root, else the dedicated memory
	// subdir).
	if a.KeepMemory && a.DataRoot != "" && !dataBlocked {
		c, e := sweepDataRootKeepingMemory(a, rt.FS, ts, moved, out)
		created = append(created, c...)
		errs = append(errs, e...)
	}

	// 4. Move the state dir aside (daemon runtime files, sandbox lease records,
	// task checkouts, teardown journal). Gated on serveDown and NOT on
	// dataBlocked: while a daemon is still alive its pidfile is not stale, it is
	// the only handle `pix serve stop` has on it, and renaming it out from under
	// a live daemon orphans exactly that (AGENTS.md invariant #4). --force does
	// not buy this one.
	if serveDown {
		dest, err := moveStateDir(a, rt.FS, ts, out)
		if err != nil {
			errs = append(errs, err)
		} else if dest != "" {
			created = append(created, dest)
		}
	} else if a.StateDir != "" {
		fmt.Fprintf(out, "  . runtime state: %s KEPT (serve still up)\n", a.StateDir)
	}

	// 5. Unregister the MCP servers the (now moved-aside) config declared, so the
	// gateway's registration list matches the clean slate pix now believes in.
	if a.RemoveSandboxes && len(a.MCPRemove) > 0 {
		unregisterMCP(a, rt.Env, out)
	}

	// 6. Restart the daemon on the clean slate if one was running before (and we
	// actually tore down). It comes up fresh against default config (the file was
	// moved aside) with an empty store — the intended clean-slate running state.
	if wasUp && serveDown {
		fmt.Fprintln(out, "Restarting host services on the clean slate:")
		if err := restartServeForReset(out); err != nil {
			fmt.Fprintf(out, "  . could not restart services (%v); `pix run` will start them\n", err)
		} else {
			fmt.Fprintln(out, "  ok host services restarted")
		}
	}
	return created, errors.Join(errs...)
}

// sweepDataRootKeepingMemory is the --keep-memory half of step 3: every
// top-level data-root entry that is not a captured-memory artifact (and not an
// existing .bak-) moves aside, and the preserved artifact is named.
func sweepDataRootKeepingMemory(a actions, fsys resetFS, ts int64, moved map[string]bool, out io.Writer) ([]string, []error) {
	var created []string
	var errs []error
	preserve, preservedLabel := keepMemoryPreserve(a.DataRoot, a.MemoryDir, a.MemoryDB)
	entries, rdErr := fsys.readDir(a.DataRoot)
	if rdErr != nil && !os.IsNotExist(rdErr) {
		// A real read failure (permissions, IO) MUST surface — do not report
		// preservation/success over a directory we could not even scan.
		fmt.Fprintf(out, "  x could not read data dir %s (%v)\n", a.DataRoot, rdErr)
		return nil, []error{fmt.Errorf("read data dir %s: %w", a.DataRoot, rdErr)}
	}
	for _, e := range entries {
		name := e.Name()
		p := filepath.Join(a.DataRoot, name)
		if preserve[filepath.Clean(p)] || strings.Contains(name, ".bak-") {
			continue // preserve the captured-memory artifacts (and earlier backups)
		}
		if moved[p] {
			continue // already handled as an explicit target
		}
		dest, mErr := moveAside(fsys, p, ts)
		if mErr != nil {
			if !errors.Is(mErr, errNotExist) {
				fmt.Fprintf(out, "  x could not move %s (%v)\n", p, mErr)
				errs = append(errs, fmt.Errorf("move %s: %w", p, mErr))
			}
			continue
		}
		created = append(created, dest)
		fmt.Fprintf(out, "  ok %s -> %s\n", p, dest)
	}
	fmt.Fprintf(out, "  ok preserved captured memory: %s\n", preservedLabel)
	return created, errs
}

// moveStateDir renames the runtime dir aside, returning the backup path ("" if
// there was nothing to move). Quiet about a missing dir: on a clean machine
// there is nothing there, which is the expected case, not a problem to report.
func moveStateDir(a actions, fsys resetFS, ts int64, out io.Writer) (string, error) {
	if !resettableStateDir(a.StateDir) {
		if a.StateDir != "" {
			fmt.Fprintf(out, "  . runtime state: %s left alone (not a resolvable pix state dir)\n", a.StateDir)
		}
		return "", nil
	}
	dest, err := moveAside(fsys, a.StateDir, ts)
	switch {
	case errors.Is(err, errNotExist):
		return "", nil
	case err != nil:
		fmt.Fprintf(out, "  x could not move runtime state %s (%v)\n", a.StateDir, err)
		return "", fmt.Errorf("move %s: %w", a.StateDir, err)
	}
	fmt.Fprintf(out, "  ok runtime state: %s -> %s\n", a.StateDir, dest)
	return dest, nil
}

// removeSandboxes runs the caller's proof-gated sweep. A failure is REPORTED and
// returned but never aborts the reset: a sandbox that refuses to go (a live
// shell still references it) must not leave the host half-reset with no message
// about the durable state it came here for.
func removeSandboxes(a actions, rt Runtime, out io.Writer) error {
	fmt.Fprintln(out, "Sandboxes:")
	if rt.Sweep == nil {
		fmt.Fprintln(out, "  . no sandbox teardown available; remove them with `pix rm --all`")
		return nil
	}
	if err := rt.Sweep(out, rt.ErrW); err != nil {
		fmt.Fprintln(out, "  x some sandboxes were not removed (see above); `pix rm --all` retries, `pix rm <name> --force` overrides")
		return fmt.Errorf("remove sandboxes: %w", err)
	}
	return nil
}

// unregisterMCP drops the configured MCP servers from the sbx gateway's HOST
// registration list. Best-effort throughout: each action is reported, and if sbx
// is absent (or its verb differs) it prints the command for the user to run —
// an unregistration pix cannot perform is not a reason to fail a reset whose
// durable half already succeeded.
func unregisterMCP(a actions, env sys.System, out io.Writer) {
	fmt.Fprintln(out, "MCP registrations (sbx gateway):")
	if _, err := env.LookPath("sbx"); err != nil {
		fmt.Fprintln(out, "  . sbx not found; run these on your host:")
		for _, name := range a.MCPRemove {
			fmt.Fprintf(out, "      sbx mcp rm %s\n", name)
		}
		return
	}
	for _, name := range a.MCPRemove {
		if _, err := env.Run("sbx", "mcp", "rm", name); err != nil {
			fmt.Fprintf(out, "  x sbx mcp rm %s (%v); run it yourself if the verb differs\n", name, err)
			continue
		}
		fmt.Fprintf(out, "  ok unregistered MCP %s\n", name)
	}
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
		fmt.Fprintf(out, "  . could not stop 'pix-host serve' (%v); stop it yourself if it is running\n", err)
	}
	return stopped, err
}

// printPlan shows EXACTLY what will be moved, cleared and removed before any
// change, so the guard prompt below is an informed one.
func printPlan(a actions, out io.Writer) {
	fmt.Fprintln(out, "pix reset: a clean slate. Everything is MOVED ASIDE, nothing is deleted.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Moving to <path>.bak-<timestamp> (rename back to restore):")
	for _, b := range a.Backups {
		fmt.Fprintf(out, "  - %s: %s\n", b.Label, b.Path)
	}
	if a.StateDir != "" {
		fmt.Fprintf(out, "  - runtime state: %s (daemon pid/lock/log, sandbox leases, task checkouts)\n", a.StateDir)
	}
	if a.KeepMemory {
		fmt.Fprintf(out, "  (keeping captured memory: %s)\n", a.MemoryDir)
	}
	fmt.Fprintln(out, "Stops the host services first, and restarts them if they were running.")
	if a.RemoveSandboxes {
		fmt.Fprintln(out, "Removing:")
		fmt.Fprintln(out, "  - every pix-* sandbox on this host (not forced: each needs a zero-reference proof)")
		for _, name := range a.MCPRemove {
			fmt.Fprintf(out, "  - MCP registration %q (sbx mcp rm)\n", name)
		}
	}
	fmt.Fprintln(out, "Leaving alone: your provider keys (sbx secret / 1Password) and any git repo.")
	fmt.Fprintln(out)
}

// printSummary closes with the restore/cleanup guidance and the next step.
func printSummary(created []string, out io.Writer) {
	fmt.Fprintln(out)
	if len(created) > 0 {
		fmt.Fprintln(out, "Backups created (rename any back to restore):")
		for _, p := range created {
			fmt.Fprintf(out, "  %s\n", p)
		}
		fmt.Fprintln(out, "  to restore one: `pix serve stop`, rename it back, then `pix run`.")
		fmt.Fprintln(out, "  delete them once you're sure:  rm -rf <path>.bak-*")
	} else {
		fmt.Fprintln(out, "Nothing to back up: the stack was already clean.")
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Next: pix setup")
}

// Run wires plan -> guard -> execute against injected deps. It returns
// ErrNeedsYes on the non-TTY-no-yes refusal (the CLI maps it to exit 2); an
// interactive "no" aborts cleanly with a nil error.
func Run(cfg *config.Config, paths Paths, opts Opts, rt Runtime) error {
	a := plan(cfg, paths, opts)
	printPlan(a, rt.IO.Out)

	if !opts.assumeYes {
		if !rt.IO.IsTTY {
			return ErrNeedsYes
		}
		if !cli.ConfirmYN(rt.IO.In, rt.IO.Out, "Proceed? [y/N]: ", false) {
			fmt.Fprintln(rt.IO.Out, "Aborted: nothing changed.")
			return nil
		}
	}

	created, execErr := execute(a, rt, rt.IO.Out)
	printSummary(created, rt.IO.Out)
	return execErr
}

// Description is `pix reset`'s help body, owned here rather than at the command
// layer for the same reason launch owns Ls/RmDescription: the words that
// describe what a verb does belong beside the code that decides it.
const Description = `Reset this host to a clean slate. REVERSIBLE: nothing is deleted.

Three directories are MOVED ASIDE to a timestamped <path>.bak-<unixts> sibling,
so renaming one back is a complete undo: the config dir, the data root (memory,
packs, personal context), and the runtime state dir (daemon pid/lock/log,
sandbox leases, and your 'pix task' checkouts, which is why this is a rename and
not a delete). Every pix-* sandbox is removed the same proof-gated way
'pix rm --all' removes them, never forced.

Left alone: your provider keys (sbx secret / 1Password) and your git repos.

  pix reset                    the whole stack, with a confirmation prompt
  pix reset --keep-memory      everything except the captured-memory store
  pix reset --keep-sandboxes   host state only; leave the pix-* boxes running
  pix reset --yes              no prompt (required on a non-interactive terminal)

Then: pix setup`
