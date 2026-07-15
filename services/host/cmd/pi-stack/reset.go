package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	help       bool // -h/--help
}

// resetPaths are the resolved host locations reset acts on. Split out so the
// pure planner takes them injected (a test supplies temp-dir paths, no real
// $HOME lookup). memoryDir/knowledgeDir honor MEMORY_DB/KNOWLEDGE_DB.
type resetPaths struct {
	configDir    string // ~/.config/pi-stack (config.toml, op-refs.env, broker-token, knowledge/, knowledge-cache/)
	dataRoot     string // ~/.pi-stack (memory/ + knowledge/)
	memoryDir    string // <dataRoot>/memory or dir(MEMORY_DB): the user's captured facts
	knowledgeDir string // <dataRoot>/knowledge or dir(KNOWLEDGE_DB): the rebuildable index
}

// backupTarget is one path the reset moves aside, with a human label.
type backupTarget struct {
	Path  string
	Label string
}

// resetActions is the pure plan: exactly what will be moved + which sbx actions
// run. It carries no side effects; executeReset consumes it.
type resetActions struct {
	Backups         []backupTarget // paths to move to <path>.bak-<ts>
	KeepMemory      bool           // preserve MemoryDir (sweep DataRoot minus memory)
	MemoryDir       string         // preserved dir when KeepMemory
	DataRoot        string         // the data root (for the keep-memory sweep)
	RemoveSandboxes bool           // --sbx: remove pi-stack-* sandboxes
	MCPRemove       []string       // --sbx: MCP server names to unregister (cfg.MCP)
}

// resetFS is the injected filesystem surface, so executeReset stays hermetic in
// tests (a temp HOME, no real rm). defaultResetFS wires the os-backed ops.
type resetFS struct {
	stat    func(path string) (os.FileInfo, error)
	lstat   func(path string) (os.FileInfo, error)
	rename  func(oldpath, newpath string) error
	readDir func(path string) ([]os.DirEntry, error)
	remove  func(path string) error
}

func defaultResetFS() resetFS {
	return resetFS{
		stat:    os.Stat,
		lstat:   os.Lstat,
		rename:  os.Rename,
		readDir: os.ReadDir,
		remove:  os.Remove,
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
	if env.getenv != nil {
		if db := strings.TrimSpace(env.getenv("MEMORY_DB")); db != "" {
			memoryDir = filepath.Dir(db)
		}
		if db := strings.TrimSpace(env.getenv("KNOWLEDGE_DB")); db != "" {
			knowledgeDir = filepath.Dir(db)
		}
	}
	return resetPaths{
		configDir:    filepath.Dir(config.Path()),
		dataRoot:     dataRoot,
		memoryDir:    memoryDir,
		knowledgeDir: knowledgeDir,
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
		DataRoot:   paths.dataRoot,
	}
	if paths.configDir != "" {
		a.Backups = append(a.Backups, backupTarget{paths.configDir, "config directory"})
	}
	if opts.keepMemory {
		// Preserve the captured facts (memory); move the rebuildable index aside.
		if paths.knowledgeDir != "" {
			a.Backups = append(a.Backups, backupTarget{paths.knowledgeDir, "knowledge database"})
		}
	} else if paths.dataRoot != "" {
		// Move the whole data root aside (captured memory + knowledge index).
		a.Backups = append(a.Backups, backupTarget{paths.dataRoot, "data directory (memory + knowledge)"})
	}
	if opts.sbx {
		a.RemoveSandboxes = true
		a.MCPRemove = append([]string(nil), cfg.MCP...)
	}
	return a
}

// moveAside renames path to "<path>.bak-<ts>" if it exists, returning the backup
// path. A missing path yields errNotExist (a soft "nothing to move").
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
	dest := fmt.Sprintf("%s.bak-%d", path, ts)
	if err := fsys.rename(path, dest); err != nil {
		return "", err
	}
	return dest, nil
}

// executeReset performs the plan: stop services (best-effort), move each backup
// aside, sweep the data root for non-memory entries under --keep-memory, then
// run the sbx removals. It never hard-removes. Returns the created .bak paths
// (for the summary's restore hint).
func executeReset(a resetActions, fsys resetFS, env shellEnv, out io.Writer, now func() time.Time) []string {
	ts := now().Unix()

	// 1. Best-effort stop of the host services so they don't hold the db files.
	stopHostServices(env, out)

	// 2. Move each explicit backup aside.
	var created []string
	moved := map[string]bool{}
	fmt.Fprintln(out, "Backing up state (moved aside, not deleted):")
	for _, b := range a.Backups {
		dest, err := moveAside(fsys, b.Path, ts)
		switch {
		case errors.Is(err, errNotExist):
			fmt.Fprintf(out, "  · %s: %s — nothing to move\n", b.Label, b.Path)
		case err != nil:
			fmt.Fprintf(out, "  ✗ %s: could not move %s — %v\n", b.Label, b.Path, err)
		default:
			moved[b.Path] = true
			created = append(created, dest)
			fmt.Fprintf(out, "  ✓ %s: %s -> %s\n", b.Label, b.Path, dest)
		}
	}

	// 3. --keep-memory: sweep the data root, moving aside every entry that is not
	// `memory` (and not a backup we just created), so "anything else" beside the
	// preserved facts is reset too.
	if a.KeepMemory && a.DataRoot != "" {
		if entries, err := fsys.readDir(a.DataRoot); err == nil {
			for _, e := range entries {
				name := e.Name()
				if name == "memory" || strings.Contains(name, ".bak-") {
					continue
				}
				p := filepath.Join(a.DataRoot, name)
				if moved[p] {
					continue // already handled (the explicit knowledge target)
				}
				if dest, mErr := moveAside(fsys, p, ts); mErr == nil {
					created = append(created, dest)
					fmt.Fprintf(out, "  ✓ %s -> %s\n", p, dest)
				}
			}
		}
		fmt.Fprintf(out, "  ✓ preserved captured memory: %s\n", a.MemoryDir)
	}

	// 4. sbx: remove pi-stack-* sandboxes + unregister the configured MCP servers.
	// Provider secrets (sbx secret) are intentionally LEFT ALONE — those are just
	// keys, not stack state, and re-entering them is friction with no upside here.
	if a.RemoveSandboxes {
		executeSbxReset(a, env, out)
	}
	return created
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

	created := executeReset(a, fsys, env, rio.out, now)
	printResetSummary(created, rio.out)
	return nil
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
		if err := fsys.remove(p); err != nil {
			fmt.Fprintf(out, "  ✗ %s — could not remove: %v\n", p, err)
		} else {
			fmt.Fprintf(out, "  ✓ removed symlink %s\n", p)
		}
	}
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

	created := executeReset(a, fsys, env, rio.out, now)
	removeBinSymlinks(bins, fsys, rio.out)
	printResetSummary(created, rio.out)
	return nil
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
