//go:build unix

// pi-stack-host `migrate` — the EXPLICIT, host-side relocation of pi-stack
// storage to the standard XDG layout (CONFIG=~/.config, DATA=~/.local/share,
// STATE=~/.local/state). It lives in the HOST binary because it needs the
// sqlite driver (integrity_check + a real reindex); the launcher and `serve`
// never migrate. Existing installs keep working in place via the config
// package's read-fallback until the user runs this.
//
// The core mirrors reset.go's hermetic seam: migrate(migrateEnv) does all of its
// filesystem + lock + sqlite work through injected functions, so tests drive
// every branch (same-fs rename, EXDEV copy, corrupt-copy refusal, flock refusal,
// conflict, resumable) against a t.TempDir() with real files/sqlite/git. The
// production constructor defaultMigrateEnv() fills the seam from the real fs, the
// existing integrityCheckDB, and a reindex that rebuilds the index at its new
// STATE location.
//
// The two structural guarantees the algorithm is built around (Part B of the
// design doc, .scratch/xdg-arch-v2.md):
//
//   - NOTHING PRECIOUS IS DELETED. Every precious artifact is moved (same-fs
//     rename) or copied+verified (EXDEV), and the legacy path is left as a
//     symlink→new (converging any old-binary reader) or a `.pre-xdg-<ts>` safety
//     copy. The only thing os.RemoveAll ever touches is our OWN `.staging-*`
//     scratch. The receipt says "moved"/"rebuilt"/"copied"/"left in place" —
//     never "removed".
//   - The knowledge index is REBUILT, never moved: it is a pure cache, so a
//     reindex at the new STATE path is strictly simpler and safer than moving an
//     unlocked WAL sqlite file.

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"

	"pi-stack/host/config"

	_ "modernc.org/sqlite"
)

// migrateEnv is the hermetic seam. Every filesystem mutation, lock, and sqlite
// touch goes through one of these so migrate() is fully testable. acquireLock
// returns ok=false when the lock is HELD (refuse memory, defer) and err!=nil only
// on a genuine acquire failure a test wants to force.
type migrateEnv struct {
	getenv         func(string) string
	homeDir        func() (string, error)
	stat           func(string) (os.FileInfo, error)
	lstat          func(string) (os.FileInfo, error)
	rename         func(old, new string) error
	symlink        func(old, new string) error
	copyTree       func(old, new string) error
	fsyncDir       func(string) error
	acquireLock    func(path string) (release func() error, ok bool, err error)
	integrityCheck func(dbPath string) error
	reindex        func(bundlePaths []string) error
	now            func() time.Time
	out            io.Writer
}

// migrateArtifact is the per-artifact outcome the receipt renders from.
type migrateArtifact struct {
	name    string // memory | knowledge | index | cache | backups
	action  string // moved | rebuilt | copied | left-in-place | absent | pinned | refused-serve | refused-conflict | refused-corrupt | rebuild-failed
	from    string
	to      string
	symlink bool   // a symlink→new was left at the legacy path
	safety  string // a .pre-xdg-<ts> safety copy path, when one was retained
	note    string // extra detail (counts, error text)
	changed bool   // did this run mutate anything for this artifact
}

// report is what migrate() returns; the CLI renders the Part D receipt from it.
type report struct {
	artifacts        []migrateArtifact
	bundlesRewritten int
}

// --- path resolution (hermetic: driven only by env.getenv + env.homeDir) -----

// migrator holds the resolved legacy + new paths and per-artifact override pins.
type migrator struct {
	env migrateEnv

	dataDir   string
	stateDir  string
	configDir string

	legMemDir  string // ~/.pi-stack/memory
	legBackups string // ~/.pi-stack/backups
	legIndexDB string // ~/.pi-stack/knowledge/knowledge.db
	legBundle  string // ~/.config/pi-stack/knowledge
	legCache   string // ~/.config/pi-stack/knowledge-cache

	newMemDir   string // DATA/memory
	newBackups  string // DATA/backups
	newIndexDir string // STATE/knowledge
	newIndexDB  string // STATE/knowledge/index.db
	newBundle   string // DATA/knowledge
	newCache    string // STATE/knowledge-cache

	memPinned    bool // MEMORY_DB set
	indexPinned  bool // KNOWLEDGE_DB set
	cachePinned  bool // KNOWLEDGE_CACHE_DIR set
	configPinned bool // PI_STACK_CONFIG set
}

// xdgBase mirrors config.DataDir/StateDir: $XDG_*_HOME/pi-stack, else the
// default under home.
func xdgBase(getenv func(string) string, envVar, home string, def ...string) string {
	if v := strings.TrimSpace(getenv(envVar)); v != "" {
		return filepath.Join(v, "pi-stack")
	}
	return filepath.Join(append(append([]string{home}, def...), "pi-stack")...)
}

// resolveMigrateConfigDir mirrors config.ConfigDir: PI_STACK_CONFIG's parent,
// else $XDG_CONFIG_HOME/pi-stack, else ~/.config/pi-stack.
func resolveMigrateConfigDir(getenv func(string) string, home string) string {
	if p := strings.TrimSpace(getenv("PI_STACK_CONFIG")); p != "" {
		return filepath.Dir(p)
	}
	if x := strings.TrimSpace(getenv("XDG_CONFIG_HOME")); x != "" {
		return filepath.Join(x, "pi-stack")
	}
	return filepath.Join(home, ".config", "pi-stack")
}

func newMigrator(env migrateEnv) (*migrator, error) {
	getenv := env.getenv
	home, err := env.homeDir()
	if err != nil {
		return nil, err
	}
	dataDir := xdgBase(getenv, "XDG_DATA_HOME", home, ".local", "share")
	stateDir := xdgBase(getenv, "XDG_STATE_HOME", home, ".local", "state")
	configDir := resolveMigrateConfigDir(getenv, home)

	m := &migrator{
		env:       env,
		dataDir:   dataDir,
		stateDir:  stateDir,
		configDir: configDir,
		// Legacy roots. NOTE the bundle + cache legacy paths use the LITERAL
		// ~/.config/pi-stack (not configDir): they were pre-XDG config siblings and
		// never honored PI_STACK_CONFIG/XDG_CONFIG_HOME, matching config's
		// legacyConfigPath.
		legMemDir:  filepath.Join(home, ".pi-stack", "memory"),
		legBackups: filepath.Join(home, ".pi-stack", "backups"),
		legIndexDB: filepath.Join(home, ".pi-stack", "knowledge", "knowledge.db"),
		legBundle:  filepath.Join(home, ".config", "pi-stack", "knowledge"),
		legCache:   filepath.Join(home, ".config", "pi-stack", "knowledge-cache"),

		newMemDir:   filepath.Join(dataDir, "memory"),
		newBackups:  filepath.Join(dataDir, "backups"),
		newIndexDir: filepath.Join(stateDir, "knowledge"),
		newIndexDB:  filepath.Join(stateDir, "knowledge", "index.db"),
		newBundle:   filepath.Join(dataDir, "knowledge"),
		newCache:    filepath.Join(stateDir, "knowledge-cache"),

		memPinned:    strings.TrimSpace(getenv("MEMORY_DB")) != "",
		indexPinned:  strings.TrimSpace(getenv("KNOWLEDGE_DB")) != "",
		cachePinned:  strings.TrimSpace(getenv("KNOWLEDGE_CACHE_DIR")) != "",
		configPinned: strings.TrimSpace(getenv("PI_STACK_CONFIG")) != "",
	}
	return m, nil
}

// configFilePath is the config.toml migrate loads + rewrites (honors
// PI_STACK_CONFIG, same as config.Path()).
func (m *migrator) configFilePath() string {
	if p := strings.TrimSpace(m.env.getenv("PI_STACK_CONFIG")); p != "" {
		return p
	}
	return filepath.Join(m.configDir, "config.toml")
}

// memoryLockPath mirrors config.MemoryLockPath: dir(resolved memory db)/.memory.lock,
// where the db resolves MEMORY_DB > new (if present) > legacy (if present) > new.
// Pre-migration this is the LEGACY lock a running daemon holds — exactly what B4
// must contend on.
func (m *migrator) memoryLockPath() string {
	db := ""
	if p := strings.TrimSpace(m.env.getenv("MEMORY_DB")); p != "" {
		db = p
	} else if m.realDirExists(m.newMemDir) && m.exists(filepath.Join(m.newMemDir, "memory.db")) {
		db = filepath.Join(m.newMemDir, "memory.db")
	} else if m.realDirExists(m.legMemDir) && m.exists(filepath.Join(m.legMemDir, "memory.db")) {
		db = filepath.Join(m.legMemDir, "memory.db")
	} else {
		db = filepath.Join(m.newMemDir, "memory.db")
	}
	return filepath.Join(filepath.Dir(db), ".memory.lock")
}

func (m *migrator) exists(p string) bool { _, err := m.env.stat(p); return err == nil }

// realDirExists reports whether p is a REAL directory via lstat, so a symlink is
// never followed (LOW-1). An attacker symlink planted at the memory path can then
// never redirect the resolved lock path into an unintended directory.
func (m *migrator) realDirExists(p string) bool {
	fi, err := m.env.lstat(p)
	return err == nil && fi.IsDir()
}

func (m *migrator) isSymlink(p string) bool {
	fi, err := m.env.lstat(p)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

// newDirBlockedNote is the operator-facing message for the R8-1 conflict: a
// NON-DIRECTORY (regular file, fifo, device, or symlink) is sitting at the new
// path, so classifyDir refuses it as a conflict instead of planting/converging
// over it. Empty when new is absent or a real directory.
func (m *migrator) newDirBlockedNote(newDir string) string {
	if (m.exists(newDir) && !m.realDirExists(newDir)) || m.isSymlink(newDir) {
		return fmt.Sprintf("unexpected non-directory at %s; remove it, then re-run", newDir)
	}
	return ""
}

// legacyDirBlockedNote is the operator-facing message for the R9-1 conflict: a
// NON-DIRECTORY (regular file, fifo, device) is sitting at the LEGACY source
// path, so classifyDir refuses it as a conflict instead of renaming it onto the
// new path. Symbols already routed as symlinks (complete/broken/conflict) and
// real directories both return empty — this note is only for a non-dir, non-
// symlink source.
func (m *migrator) legacyDirBlockedNote(legacy string) string {
	fi, err := m.env.lstat(legacy)
	if err != nil || fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return ""
	}
	return fmt.Sprintf("unexpected non-directory at legacy path %s; remove it, then re-run", legacy)
}

// conflictNote picks the right operator message for a classifyDir "conflict":
// the new-side note (non-dir/symlink at new) takes priority, else the legacy-side
// note (non-dir at legacy). Empty falls through to the generic both-real-dirs
// conflict text in the receipt.
func (m *migrator) conflictNote(legacy, newDir string) string {
	if n := m.newDirBlockedNote(newDir); n != "" {
		return n
	}
	return m.legacyDirBlockedNote(legacy)
}

// legacyIsSymlinkTo reports whether legacy is a symlink resolving to target. It is
// only HALF of convergence — the target's textual identity, not its existence —
// so it is used INSIDE classifyDir (which then separately checks whether new
// exists to split "complete" from "broken") and INSIDE converged(). Every gate
// that means "the handoff is done" must route through converged(), never this.
func (m *migrator) legacyIsSymlinkTo(legacy, target string) bool {
	fi, err := m.env.lstat(legacy)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return false
	}
	tgt, err := os.Readlink(legacy)
	if err != nil {
		return false
	}
	if !filepath.IsAbs(tgt) {
		tgt = filepath.Join(filepath.Dir(legacy), tgt)
	}
	return filepath.Clean(tgt) == filepath.Clean(target)
}

// converged is the SINGLE authoritative predicate for "this artifact's handoff is
// done": legacy is a symlink resolving to newDir AND newDir EXISTS AS A REAL
// DIRECTORY (lstat, never followed — a symlink sitting at newDir is a rogue
// artifact, not convergence). classifyDir stays the source of truth for the full
// classification; converged() is what the OTHER gates (the config-rewrite gate
// via artifactConverged, the backups short-circuit, detection's equivalent in
// detectPendingLegacy) route through so they AGREE with it — a broken (dangling)
// or conflicted (rogue-symlink) handoff is NEVER converged.
func (m *migrator) converged(legacy, newDir string) bool {
	if !m.legacyIsSymlinkTo(legacy, newDir) {
		return false
	}
	return m.realDirExists(newDir)
}

// artifactConverged reports whether a dir artifact ended THIS run at its new home,
// so rewriteConfig may rewrite that artifact's configured paths: it either MOVED
// this run (a real success — same-fs, EXDEV, or a replant, all recorded as
// changed) or is already converged() (a prior run's planted symlink->new whose
// new dir exists). A refused-broken / refused-conflict / refused-corrupt /
// pinned / absent artifact is NEVER converged, so its paths are left untouched
// (R7-1: a broken or conflicted handoff must not rewrite config to a bad path).
//
// R9-1: the rewrite gate ALSO requires the destination to be a REAL directory,
// independently of classifyDir's correctness. A "changed" artifact whose new dir
// is not a real directory (e.g. a legacy non-directory source that some future
// path renamed into place) must NOT count as converged and must NOT gate the
// config rewrite — otherwise config would be rewritten to an unusable path while
// detection reports unconverged, and the gates would disagree. converged()
// already implies realDirExists(a.to); this makes the a.changed branch obey it
// too.
func (m *migrator) artifactConverged(a migrateArtifact) bool {
	return m.realDirExists(a.to) && (a.changed || m.converged(a.from, a.to))
}

// --- resumable classification (Part B1) --------------------------------------

// classifyDir returns "complete" | "replant" | "conflict" | "broken" |
// "resumable" for a directory artifact. valid gates "replant" on the new copy
// being usable (dir non-empty; for memory, the db passes integrity_check). A
// converged legacy symlink->new is "complete" regardless of valid — the symlink
// means new IS the authority and the handoff is done, so it must NEVER be
// classified "resumable" (a rerun would try to rename the symlink over the live
// new dir -> a permanent hard failure) even when the new db itself is corrupt;
// that is a separate DB problem to repair, not a migration to redo. A leftover
// `<new>.staging-*` forces "resumable" so a partial copy is never mistaken for a
// finished migration.
//
// A legacy path that is a SYMLINK is NEVER a movable data source (R6-1): renaming
// it onto newDir would plant a self-referential newDir->newDir loop, not migrate
// any data. So a symlink is classified purely on where it points and whether new
// exists, and can only be "complete", "broken", or "conflict" — never "resumable":
//   - symlink -> expected new AND new EXISTS  -> "complete" (converged handoff)
//   - symlink -> expected new AND new ABSENT  -> "broken" (dangling handoff: the
//     user deleted the new dir; refuse with a clear message, never rename the link)
//   - symlink -> anywhere else                -> "conflict" (a link we did not plant)
//
// Only a REAL legacy directory is ever a resumable/movable source.
//
// "replant" is the resumable HANDOFF-REPAIR state: the new copy is valid but the
// legacy path is absent (a prior run moved the data but crashed before, or failed
// at, planting the compatibility symlink — or the legacy path was recreated in the
// gap). Without this, newOK + absent-legacy read as "complete" and the symlink was
// never (re)planted, so an old-binary reader at the legacy path found nothing. The
// caller (re)plants the symlink idempotently, so a rerun converges. The symlink is
// only ever planted when legacy is ABSENT (here) or already the correct symlink
// ("complete"); a real dir with independent content at legacy stays a "conflict".
func (m *migrator) classifyDir(legacy, newDir string, valid func(string) bool) string {
	// LOW-1: we NEVER plant a symlink at the NEW path (only at the legacy path,
	// pointing to new). So a symlink sitting at the new path is an UNEXPECTED
	// artifact (a rogue/attacker link) — refuse rather than stat-follow it.
	if m.isSymlink(newDir) {
		return "conflict"
	}
	// R8-1: "new exists and is usable" must require a REAL DIRECTORY for EVERY
	// artifact, exactly like converged() and detection do — not merely "stat
	// succeeds and is not a symlink". A non-directory (regular file, fifo, device)
	// sitting at the new path is an unexpected artifact: refuse it as a conflict
	// rather than planting a symlink at it, marking it changed, or treating it as
	// converged. Otherwise a stub file at newDir with legacy absent would misclassify
	// as "replant" (valid() never sees a real dir; the cache validator is a constant
	// true), plant a symlink->file, and rewrite config to an unusable path.
	if m.exists(newDir) && !m.realDirExists(newDir) {
		return "conflict"
	}
	newExists := m.realDirExists(newDir)
	newOK := newExists && valid(newDir)
	legInfo, legErr := m.env.lstat(legacy)
	legPresent := legErr == nil
	staging := m.hasStaging(newDir)

	// A legacy SYMLINK is never renamed as data (R6-1). Route it by target only.
	if legPresent && legInfo.Mode()&os.ModeSymlink != 0 {
		if !m.legacyIsSymlinkTo(legacy, newDir) {
			// A symlink we did not plant, pointing somewhere other than the new dir —
			// refuse rather than follow it into an unintended location (LOW-1).
			return "conflict"
		}
		if newExists && !staging {
			// Converged: legacy is the planted symlink->new and new is present, so the
			// handoff is DONE and new is the authority whether or not it passes
			// integrity_check. Never a resumable move over the live new dir.
			return "complete"
		}
		// Dangling: the planted symlink points at a new dir that no longer exists (the
		// user deleted it). Renaming the link over newDir would create a self-
		// referential loop — refuse with a clear message instead.
		return "broken"
	}

	// R9-1: a PRESENT legacy source that is NOT a real directory (regular file, fifo,
	// device — symlinks are already routed above) is never a movable/resumable
	// source. This is the symmetric twin of the R8-1 non-directory-at-new guard.
	// Renaming such a source onto newDir would plant a file (or a symlink over a
	// stub) and rewrite config to an unusable path; the default "resumable" branch
	// below would otherwise happily do exactly that. Refuse it as a conflict so
	// classify + the rewrite gate + detection all agree. Only a REAL legacy
	// directory is ever a movable source.
	if legPresent && !legInfo.IsDir() {
		return "conflict"
	}

	switch {
	case newOK && !legPresent && !staging:
		// New copy done, but the legacy compatibility symlink is missing — (re)plant it.
		return "replant"
	case newOK && legPresent:
		// A real dir at BOTH locations (legacy is not a symlink here): two independent
		// authorities. Refuse — never merge silently.
		return "conflict"
	default:
		return "resumable"
	}
}

// brokenHandoffNote is the operator-facing message for a dangling expected-target
// symlink: the legacy path still points at a new dir that no longer exists.
func brokenHandoffNote(legacy, newDir string) string {
	return fmt.Sprintf("broken handoff: %s points at a missing %s; restore %s from backup or remove the dangling link, then re-run", legacy, newDir, newDir)
}

func (m *migrator) hasStaging(base string) bool {
	matches, _ := filepath.Glob(base + ".staging-*")
	return len(matches) > 0
}

// discardStaging removes our OWN leftover `<base>.staging-*` scratch. This is the
// single place migrate deletes anything, and it is never a precious artifact —
// only a partial copy a prior crashed run left behind.
func (m *migrator) discardStaging(base string) {
	matches, _ := filepath.Glob(base + ".staging-*")
	for _, p := range matches {
		_ = os.RemoveAll(p)
	}
}

// token returns a crypto-random suffix (LOW-4). It backs both the `.pre-xdg-<ts>`
// safety-copy names and the collision-safe backup names, so two moves in the same
// second can never collide on a 1s-granularity timestamp. Falls back to a
// nanosecond clock only if the RNG is somehow unavailable.
func (m *migrator) token() string {
	if t, err := restoreToken(); err == nil {
		return t
	}
	return strconv.FormatInt(m.env.now().UnixNano(), 36)
}

// --- generic directory artifact (knowledge bundle, cache) --------------------

// migrateDir migrates a whole-directory artifact with the same-fs→EXDEV skeleton:
// same-fs is a single atomic rename + symlink handoff; EXDEV copies into a
// staging dir, verifies, atomically renames staging→new, moves the legacy source
// to a `.pre-xdg-<ts>` safety copy, then plants the symlink. Returns the outcome
// + any HARD error (a refusal is not an error).
func (m *migrator) migrateDir(name, legacy, newDir string, pinned bool, valid func(string) bool, verify func(staging string) error) (migrateArtifact, error) {
	art := migrateArtifact{name: name, from: legacy, to: newDir}
	if pinned {
		art.action = "pinned"
		return art, nil
	}
	m.discardStaging(newDir)
	switch m.classifyDir(legacy, newDir, valid) {
	case "complete":
		art.action = "left-in-place"
		art.symlink = m.isSymlink(legacy)
		return art, nil
	case "replant":
		// Handoff repair: new is valid, legacy absent — (re)plant the symlink so an
		// old-binary reader at the legacy path converges. The legacy PARENT may be gone
		// (a fully-migrated tree), so recreate it first. Idempotent.
		if e := os.MkdirAll(filepath.Dir(legacy), 0o755); e != nil {
			return art, fmt.Errorf("%s: mkdir %s: %w", name, filepath.Dir(legacy), e)
		}
		if e := m.env.symlink(newDir, legacy); e != nil {
			return art, fmt.Errorf("%s: replant symlink %s -> %s: %w", name, legacy, newDir, e)
		}
		art.action, art.symlink, art.changed = "moved", true, true
		return art, nil
	case "conflict":
		art.action, art.note = "refused-conflict", m.conflictNote(legacy, newDir)
		return art, nil
	case "broken":
		// A dangling legacy symlink->new (new deleted). Never rename the link.
		art.action, art.note = "refused-broken", brokenHandoffNote(legacy, newDir)
		return art, fmt.Errorf("%s: broken handoff: %s points at a missing %s", name, legacy, newDir)
	}
	// resumable — need a legacy source to move.
	legInfo, err := m.env.lstat(legacy)
	if err != nil {
		art.action = "absent"
		return art, nil
	}
	// Defensive (R6-1): classifyDir already routes a symlink to complete/broken/
	// conflict, but never rename a symlink source as if it were a data dir.
	if legInfo.Mode()&os.ModeSymlink != 0 {
		art.action, art.note = "refused-broken", brokenHandoffNote(legacy, newDir)
		return art, fmt.Errorf("%s: refusing to move a symlink source %s", name, legacy)
	}
	// Defensive (R9-1): classifyDir already routes a non-directory legacy source to
	// conflict, but never rename a regular file / fifo / device as if it were a data
	// dir on any path.
	if !legInfo.IsDir() {
		art.action, art.note = "refused-conflict", m.legacyDirBlockedNote(legacy)
		return art, nil
	}
	if err := os.MkdirAll(filepath.Dir(newDir), 0o755); err != nil {
		return art, fmt.Errorf("%s: mkdir %s: %w", name, filepath.Dir(newDir), err)
	}

	// Same-fs fast path: one atomic rename consumes the legacy dir, then we plant
	// the symlink at the (now free) legacy path.
	err = m.env.rename(legacy, newDir)
	if err == nil {
		_ = m.env.fsyncDir(filepath.Dir(newDir))
		if e := m.env.symlink(newDir, legacy); e != nil {
			return art, fmt.Errorf("%s: symlink %s -> %s: %w", name, legacy, newDir, e)
		}
		art.action, art.symlink, art.changed = "moved", true, true
		return art, nil
	}
	if !errors.Is(err, syscall.EXDEV) {
		return art, fmt.Errorf("%s: rename %s -> %s: %w", name, legacy, newDir, err)
	}

	// EXDEV: copy → verify → atomic rename → retain legacy as a safety copy →
	// symlink. The legacy source is NEVER removed.
	staging := newDir + ".staging-" + m.token()
	if e := m.env.copyTree(legacy, staging); e != nil {
		_ = os.RemoveAll(staging)
		return art, fmt.Errorf("%s: copy %s -> %s: %w", name, legacy, staging, e)
	}
	if verify != nil {
		if e := verify(staging); e != nil {
			_ = os.RemoveAll(staging)
			art.action, art.note = "refused-corrupt", e.Error()
			return art, fmt.Errorf("%s: verify copy: %w", name, e)
		}
	}
	_ = m.env.fsyncDir(staging)
	if e := m.env.rename(staging, newDir); e != nil {
		_ = os.RemoveAll(staging)
		return art, fmt.Errorf("%s: rename staging -> %s: %w", name, newDir, e)
	}
	_ = m.env.fsyncDir(filepath.Dir(newDir))
	safety := legacy + ".pre-xdg-" + m.token()
	if e := m.env.rename(legacy, safety); e != nil {
		return art, fmt.Errorf("%s: retain safety copy %s: %w", name, safety, e)
	}
	if e := m.env.symlink(newDir, legacy); e != nil {
		return art, fmt.Errorf("%s: symlink %s -> %s: %w", name, legacy, newDir, e)
	}
	art.action, art.symlink, art.safety, art.changed = "moved", true, safety, true
	return art, nil
}

func (m *migrator) dirNonEmpty(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) > 0
}

// --- memory (Part B4): flock-guarded, staged, atomic, symlink handoff --------

func (m *migrator) migrateMemory() (migrateArtifact, error) {
	art := migrateArtifact{name: "memory", from: m.legMemDir, to: m.newMemDir}
	if m.memPinned {
		art.action = "pinned"
		return art, nil
	}
	m.discardStaging(m.newMemDir)
	valid := func(dir string) bool { return m.env.integrityCheck(filepath.Join(dir, "memory.db")) == nil }
	switch m.classifyDir(m.legMemDir, m.newMemDir, valid) {
	case "complete":
		art.action = "left-in-place"
		art.symlink = m.isSymlink(m.legMemDir)
		// Diagnostic only, never a move: the handoff is done and new is the authority,
		// but if the new db itself fails integrity_check the user's database may be
		// corrupt. Surface it without touching anything.
		if e := m.env.integrityCheck(filepath.Join(m.newMemDir, "memory.db")); e != nil {
			art.note = "new db fails integrity_check - your database may be corrupt"
			fmt.Fprintf(m.env.out, "pi-stack migrate: WARNING: memory %s\n", art.note)
		}
		return art, nil
	case "replant":
		// Handoff repair: the new memory db is valid, legacy absent — (re)plant the
		// symlink so a legacy-path reader converges. This touches no db content (the
		// authoritative db already lives at new), so it needs no flock. The legacy
		// PARENT may be gone, so recreate it first. Idempotent.
		if e := os.MkdirAll(filepath.Dir(m.legMemDir), 0o755); e != nil {
			return art, fmt.Errorf("memory: mkdir %s: %w", filepath.Dir(m.legMemDir), e)
		}
		if e := m.env.symlink(m.newMemDir, m.legMemDir); e != nil {
			return art, fmt.Errorf("memory: replant symlink %s -> %s: %w", m.legMemDir, m.newMemDir, e)
		}
		art.action, art.symlink, art.changed = "moved", true, true
		return art, nil
	case "conflict":
		art.action, art.note = "refused-conflict", m.conflictNote(m.legMemDir, m.newMemDir)
		return art, nil
	case "broken":
		// A dangling legacy symlink->new (new deleted). Never rename the link.
		art.action, art.note = "refused-broken", brokenHandoffNote(m.legMemDir, m.newMemDir)
		return art, fmt.Errorf("memory: broken handoff: %s points at a missing %s", m.legMemDir, m.newMemDir)
	}
	legInfo, lerr := m.env.lstat(m.legMemDir)
	if lerr != nil {
		art.action = "absent"
		return art, nil
	}
	// Defensive (R6-1): classifyDir already routes a symlink to complete/broken/
	// conflict, but never rename a symlink source as if it were the memory dir.
	if legInfo.Mode()&os.ModeSymlink != 0 {
		art.action, art.note = "refused-broken", brokenHandoffNote(m.legMemDir, m.newMemDir)
		return art, fmt.Errorf("memory: refusing to move a symlink source %s", m.legMemDir)
	}
	// Defensive (R9-1): classifyDir already routes a non-directory legacy source to
	// conflict, but never rename a regular file / fifo / device as if it were the
	// memory dir on any path.
	if !legInfo.IsDir() {
		art.action, art.note = "refused-conflict", m.legacyDirBlockedNote(m.legMemDir)
		return art, nil
	}

	// Acquire the memory flock FIRST — the same lock a running daemon (old or new
	// binary, both fall back to the legacy path pre-migration) holds. If held,
	// refuse memory and let the caller continue with the other artifacts.
	release, ok, err := m.env.acquireLock(m.memoryLockPath())
	if err != nil {
		return art, fmt.Errorf("memory: acquire lock: %w", err)
	}
	if !ok {
		art.action = "refused-serve"
		return art, nil
	}
	defer func() { _ = release() }() // release LAST, after publication

	if err := os.MkdirAll(m.dataDir, 0o755); err != nil {
		return art, fmt.Errorf("memory: mkdir %s: %w", m.dataDir, err)
	}

	// Symmetric integrity gate (R5-1): verify the LEGACY memory db BEFORE any
	// publish. The EXDEV path below re-checks its staging COPY, but the same-fs
	// rename has no copy to check — so without this a corrupt legacy db would be
	// renamed to new + symlinked as if healthy, and a rerun (new corrupt, legacy now
	// a symlink->new) used to misclassify "resumable" and hard-fail. Refuse instead:
	// leave the legacy dir exactly in place (no rename, no symlink, no staging) and
	// surface it as a hard error, exactly like the EXDEV corrupt case. Other
	// artifacts still migrate.
	if e := m.env.integrityCheck(filepath.Join(m.legMemDir, "memory.db")); e != nil {
		art.action, art.note = "refused-corrupt", "memory db failed integrity check; left in place at "+m.legMemDir+" - inspect/repair, then re-run pi-stack migrate"
		return art, fmt.Errorf("memory: legacy db failed integrity_check; left in place at %s: %w", m.legMemDir, e)
	}

	// Same-fs: rename the dir (db+wal+shm stay atomic), then symlink the consumed
	// legacy path → new.
	err = m.env.rename(m.legMemDir, m.newMemDir)
	if err == nil {
		_ = m.env.fsyncDir(m.dataDir)
		if e := m.env.symlink(m.newMemDir, m.legMemDir); e != nil {
			return art, fmt.Errorf("memory: symlink %s -> %s: %w", m.legMemDir, m.newMemDir, e)
		}
		art.action, art.symlink, art.changed = "moved", true, true
		return art, nil
	}
	if !errors.Is(err, syscall.EXDEV) {
		return art, fmt.Errorf("memory: rename %s -> %s: %w", m.legMemDir, m.newMemDir, err)
	}

	// EXDEV: copy db+wal+shm into staging, integrity_check MUST be ok, atomic
	// rename, retain legacy as a .pre-xdg-<ts> safety copy, then symlink.
	staging := m.newMemDir + ".staging-" + m.token()
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return art, fmt.Errorf("memory: mkdir staging: %w", err)
	}
	for _, sc := range []string{"", "-wal", "-shm"} {
		src := filepath.Join(m.legMemDir, "memory.db"+sc)
		if _, e := m.env.stat(src); e != nil {
			continue // an absent -wal/-shm sidecar is fine
		}
		if e := copyFile(src, filepath.Join(staging, "memory.db"+sc)); e != nil {
			_ = os.RemoveAll(staging)
			return art, fmt.Errorf("memory: copy %s: %w", src, e)
		}
	}
	if e := m.env.integrityCheck(filepath.Join(staging, "memory.db")); e != nil {
		// Refuse BEFORE planting any symlink; the source is left completely intact.
		_ = os.RemoveAll(staging)
		art.action, art.note = "refused-corrupt", "copied memory db failed integrity_check; legacy source left intact at "+m.legMemDir
		return art, fmt.Errorf("memory: copied db failed integrity_check: %w", e)
	}
	_ = m.env.fsyncDir(staging)
	if e := m.env.rename(staging, m.newMemDir); e != nil {
		_ = os.RemoveAll(staging)
		return art, fmt.Errorf("memory: rename staging -> %s: %w", m.newMemDir, e)
	}
	_ = m.env.fsyncDir(m.dataDir)
	safety := m.legMemDir + ".pre-xdg-" + m.token()
	if e := m.env.rename(m.legMemDir, safety); e != nil {
		return art, fmt.Errorf("memory: retain safety copy %s: %w", safety, e)
	}
	if e := m.env.symlink(m.newMemDir, m.legMemDir); e != nil {
		return art, fmt.Errorf("memory: symlink %s -> %s: %w", m.legMemDir, m.newMemDir, e)
	}
	art.action, art.symlink, art.safety, art.changed = "moved", true, safety, true
	return art, nil
}

// --- knowledge index (Part B2 step 1): rebuilt, never moved ------------------

func (m *migrator) migrateIndex(bundlePaths []string) (migrateArtifact, error) {
	art := migrateArtifact{name: "index", from: "(rebuilt)", to: m.newIndexDB}
	if m.indexPinned {
		art.action = "pinned"
		return art, nil
	}
	if m.exists(m.newIndexDB) {
		// Already rebuilt at the new location — idempotent, no churn.
		art.action = "left-in-place"
		return art, nil
	}
	if !m.exists(m.legIndexDB) && len(bundlePaths) == 0 {
		// Nothing ever indexed and nothing to index — leave it.
		art.action = "absent"
		return art, nil
	}
	if err := os.MkdirAll(m.newIndexDir, 0o755); err != nil {
		return art, fmt.Errorf("index: mkdir %s: %w", m.newIndexDir, err)
	}
	// O2: only LOCAL, EXISTING bundle roots can be reindexed here. A git URL
	// (scheme://) or a not-yet-present absolute path is resolved/cloned by the
	// running knowledge service later; passing it to reindex fails on the stat
	// ("stat https://...: no such file"). Filter to what exists on disk now.
	local := m.localBundleRoots(bundlePaths)
	if len(local) == 0 && len(bundlePaths) > 0 {
		// Every configured bundle is a URL (or not present yet): nothing LOCAL to
		// index now. If NO legacy index exists there is nothing to override — leave the
		// empty new index dir for `serve` to build. But if a legacy index DOES exist we
		// must still PUBLISH a new index (an empty-but-valid index.db via reindex(nil)),
		// or KnowledgeIndexPath's read-fallback resolves to the legacy name forever and
		// legacy stays authoritative. serve rebuilds from the URL bundles on startup
		// anyway, so an empty published index is safe.
		if !m.exists(m.legIndexDB) {
			art.action, art.note = "left-in-place", "index will be built by serve"
			return art, nil
		}
		if err := m.env.reindex(nil); err != nil {
			art.action, art.note = "rebuild-failed", err.Error()
			return art, nil
		}
		art.action, art.changed, art.note = "rebuilt", true, "empty index published; URL bundles built by serve"
		return art, nil
	}
	if err := m.env.reindex(local); err != nil {
		// The index is a disposable cache a running serve rebuilds anyway, so a
		// rebuild failure is reported but never fatal to the migration.
		art.action, art.note = "rebuild-failed", err.Error()
		return art, nil
	}
	art.action, art.changed = "rebuilt", true
	return art, nil
}

// --- backups (Part B2 step 4 / finding 9): merge-copy, never dir-rename -------

func (m *migrator) migrateBackups() (migrateArtifact, error) {
	art := migrateArtifact{name: "backups", from: m.legBackups, to: m.newBackups}
	info, err := m.env.stat(m.legBackups)
	if err != nil || !info.IsDir() {
		art.action = "absent"
		return art, nil
	}
	if m.converged(m.legBackups, m.newBackups) {
		// A prior run already converged this (unlikely, since we keep the dir), but
		// treat a symlinked-legacy->real-new-dir as complete.
		art.action = "left-in-place"
		return art, nil
	}
	entries, err := os.ReadDir(m.legBackups)
	if err != nil {
		return art, fmt.Errorf("backups: read %s: %w", m.legBackups, err)
	}
	if err := os.MkdirAll(m.newBackups, 0o755); err != nil {
		return art, fmt.Errorf("backups: mkdir %s: %w", m.newBackups, err)
	}
	copied := 0
	for _, e := range entries {
		if e.IsDir() || !backupNameRe.MatchString(e.Name()) {
			continue
		}
		src := filepath.Join(m.legBackups, e.Name())
		dst := filepath.Join(m.newBackups, e.Name())
		if _, derr := os.Stat(dst); derr == nil {
			// Same name already at the new dir. Compare CONTENT (not just size): two
			// DIFFERENT tarballs can share a byte length, so a size-only check would drop
			// a distinct legacy archive. Identical content -> it is the archive we copied
			// on a prior run, skip (idempotent). Different content -> a genuine clash, so
			// copy under a collision-safe -<rand> name (never overwrite).
			if same, cerr := filesIdentical(src, dst); cerr == nil && same {
				continue
			}
			dst = m.collisionSafeBackupName(e.Name())
		}
		if err := copyFile(src, dst); err != nil {
			return art, fmt.Errorf("backups: copy %s: %w", src, err)
		}
		copied++
	}
	_ = m.env.fsyncDir(m.newBackups)
	// The legacy dir + its archives are LEFT readable (a restore given an explicit
	// legacy path still finds the file); future writes converge on the new dir via
	// BackupsDir()'s read-fallback. We deliberately do NOT replace the legacy dir
	// with a symlink — that is incompatible with keeping the archives in place.
	art.action = "copied"
	art.changed = copied > 0
	if !art.changed {
		art.action = "left-in-place"
	}
	art.note = fmt.Sprintf("%d archive(s)", copied)
	return art, nil
}

// filesIdentical reports whether two files have identical CONTENT. It short-
// circuits on a size mismatch, then compares bytes in a streaming loop so a large
// same-size pair is still compared correctly (not assumed equal). A read/stat
// error is returned so the caller falls back to the collision-safe copy rather
// than silently skipping.
func filesIdentical(a, b string) (bool, error) {
	fa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	if fa.Size() != fb.Size() {
		return false, nil
	}
	fha, err := os.Open(a)
	if err != nil {
		return false, err
	}
	defer fha.Close()
	fhb, err := os.Open(b)
	if err != nil {
		return false, err
	}
	defer fhb.Close()
	const chunk = 64 << 10
	ba := make([]byte, chunk)
	bb := make([]byte, chunk)
	for {
		na, ea := io.ReadFull(fha, ba)
		nb, eb := io.ReadFull(fhb, bb)
		if na != nb || !bytes.Equal(ba[:na], bb[:nb]) {
			return false, nil
		}
		aEOF := ea == io.EOF || ea == io.ErrUnexpectedEOF
		bEOF := eb == io.EOF || eb == io.ErrUnexpectedEOF
		if aEOF || bEOF {
			if aEOF != bEOF {
				return false, nil
			}
			return true, nil
		}
		if ea != nil {
			return false, ea
		}
		if eb != nil {
			return false, eb
		}
	}
}

func (m *migrator) collisionSafeBackupName(name string) string {
	base := strings.TrimSuffix(name, ".tar.gz")
	tok := m.token()
	cand := filepath.Join(m.newBackups, base+"-"+tok+".tar.gz")
	for i := 0; ; i++ {
		if _, err := os.Stat(cand); err != nil {
			return cand
		}
		cand = filepath.Join(m.newBackups, fmt.Sprintf("%s-%s-%d.tar.gz", base, tok, i))
	}
}

// --- config rewrite (Part B3): one atomic, locked transaction ----------------

// configuredBundles loads the current config (best-effort) and returns the union
// of base + profile bundle paths, used to seed the index rebuild.
// configParseError returns a non-nil error ONLY when the config file exists but
// cannot be parsed (O1). An absent config, or a pinned config location we never
// rewrite, returns nil — there is nothing to validate or rewrite in those cases.
func (m *migrator) configParseError() error {
	if m.configPinned {
		return nil
	}
	cfgPath := m.configFilePath()
	if _, err := os.Stat(cfgPath); err != nil {
		return nil // absent (LoadFrom treats absence as ok) or unstatable
	}
	if _, err := config.LoadFrom(cfgPath); err != nil {
		return err
	}
	return nil
}

func (m *migrator) configuredBundles() []string {
	cfg, err := config.LoadFrom(m.configFilePath())
	if err != nil || cfg == nil {
		return nil
	}
	return cfg.AllKnowledgeBundles()
}

// localBundleRoots filters a configured bundle set down to LOCAL, EXISTING
// directory roots (O2): git URLs (scheme:// or the scp-like git@host:path form)
// and paths that do not exist yet are dropped, since the running knowledge
// service resolves/clones those later. Passing them to reindex would fail on the
// up-front validateBundleRoot stat.
func (m *migrator) localBundleRoots(bundlePaths []string) []string {
	var out []string
	for _, bp := range bundlePaths {
		e := strings.TrimSpace(bp)
		if e == "" || isGitURL(e) {
			continue
		}
		if _, err := m.env.stat(e); err != nil {
			continue // not present yet — serve will resolve/clone it
		}
		out = append(out, bp)
	}
	return out
}

// rewriteConfig rewrites knowledge_bundles in the base config AND every non-nil
// profile override so a configured path still resolves after the bundle/cache
// moved. Consolidated into ONE atomic transaction under WithConfigLock (B3): the
// config is loaded FRESH under the lock, mutated, and written temp+fsync+rename;
// a no-op writes nothing. Returns the number of entries rewritten.
//
// I1 (note only): the write path re-encodes the WHOLE config, which drops any
// TOML keys the Config struct does not model — this is the pre-existing
// config.Save behavior, unchanged here.
func (m *migrator) rewriteConfig(bundleMigrated, cacheMigrated bool) (int, error) {
	if m.configPinned {
		// The user pins their config location; leave it untouched. Bundle paths keep
		// resolving through the symlink we left at the legacy location.
		return 0, nil
	}
	if !bundleMigrated && !cacheMigrated {
		return 0, nil
	}
	cfgPath := m.configFilePath()

	count := 0
	err := config.WithConfigLock(func() error {
		// LOW-3: the existence early-out lives INSIDE the lock so the check + rewrite
		// are one atomic critical section (no TOCTOU with a concurrent config write).
		if _, serr := os.Stat(cfgPath); serr != nil {
			if os.IsNotExist(serr) {
				return nil // no config file: nothing to rewrite
			}
			return serr
		}
		cfg, err := config.LoadFrom(cfgPath)
		if err != nil {
			return err
		}
		changed := false
		rewriteList := func(list []string) ([]string, bool) {
			listChanged := false
			out := make([]string, len(list))
			for i, e := range list {
				ne, did := m.rewriteBundlePath(e, bundleMigrated, cacheMigrated)
				out[i] = ne
				if did {
					listChanged = true
					count++
				}
			}
			return out, listChanged
		}
		if nl, c := rewriteList(cfg.KnowledgeBundles); c {
			cfg.KnowledgeBundles = nl
			changed = true
		}
		for name, p := range cfg.Profiles {
			if p.KnowledgeBundles == nil {
				continue // preserve an INHERIT override as nil — never materialize it
			}
			if nl, c := rewriteList(*p.KnowledgeBundles); c {
				p.KnowledgeBundles = &nl
				cfg.Profiles[name] = p
				changed = true
			}
		}
		if !changed {
			return nil
		}
		return m.atomicWriteConfig(cfgPath, cfg)
	})
	return count, err
}

// rewriteBundlePath maps ONE configured bundle path across the migration:
// the old default bundle → new default; a descendant of the old cache root →
// new cache root + preserved relative suffix. Git URLs and paths outside those
// roots are returned unchanged. Both sides are canonicalized (canonicalizeBundle,
// the package-main twin of config.canonicalizeBundlePath) so the post-move
// symlink at the legacy path lets an old-spelling entry resolve to — and match —
// the new home.
func (m *migrator) rewriteBundlePath(entry string, bundleMigrated, cacheMigrated bool) (string, bool) {
	e := strings.TrimSpace(entry)
	if e == "" || isGitURL(e) {
		return entry, false
	}
	canon := canonicalizeBundle(e)
	if bundleMigrated && canon == canonicalizeBundle(m.newBundle) {
		if e == m.newBundle {
			return entry, false
		}
		return m.newBundle, true
	}
	if cacheMigrated {
		// Match a descendant of the OLD cache root by CLEANED path — no leaf-existence
		// required. A not-yet-cloned repo under the old cache root (its leaf absent, so
		// EvalSymlinks fails and canon falls back to the old spelling) must still be
		// rewritten to the new cache root, preserving its relative suffix.
		if rel, ok := relUnder(filepath.Clean(m.legCache), cleanAbs(e)); ok {
			nw := filepath.Join(m.newCache, rel)
			if e == nw {
				return entry, false
			}
			return nw, true
		}
		// Also match an entry already spelled under the NEW cache root (resolved via
		// the symlink) so a re-run stays idempotent and a caller that pre-spelled the
		// new path is left untouched.
		cacheCanon := canonicalizeBundle(m.newCache)
		if rel, ok := relUnder(cacheCanon, canon); ok {
			nw := filepath.Join(m.newCache, rel)
			if e == nw {
				return entry, false
			}
			return nw, true
		}
	}
	return entry, false
}

// cleanAbs returns the cleaned ABSOLUTE form of a path WITHOUT resolving symlinks
// or requiring the leaf to exist (unlike canonicalizeBundle, whose EvalSymlinks
// fails on an absent path and falls back to the old spelling). Used to test
// membership under the old cache root so an absent descendant still rewrites.
func cleanAbs(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(p)
}

// atomicWriteConfig encodes cfg to TOML and installs it with temp-in-dir + fsync
// + rename (the atomic commit) + parent fsync, at 0600.
func (m *migrator) atomicWriteConfig(path string, cfg *config.Config) error {
	dir := filepath.Dir(path)
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(cfg); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("temp config: %w", err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		tmp.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := m.env.rename(tmpPath, path); err != nil {
		return fmt.Errorf("commit config: %w", err)
	}
	committed = true
	_ = m.env.fsyncDir(dir)
	return nil
}

// --- the migration itself ----------------------------------------------------

// migrate runs the full explicit relocation. It returns a report the CLI renders
// and a joined error for any HARD failure; a REFUSAL (serve up, conflict) is
// recorded in the report with a nil error (a valid deferred state). Order is
// Part B2: cheap+safe first, precious+guarded (memory) last.
func migrate(env migrateEnv) (report, error) {
	m, err := newMigrator(env)
	if err != nil {
		return report{}, err
	}
	var rep report
	var errs []error
	add := func(a migrateArtifact, e error) {
		rep.artifacts = append(rep.artifacts, a)
		if e != nil {
			errs = append(errs, e)
		}
	}

	// O1: validate that config.toml PARSES before moving any artifact. An
	// unparseable config must not leave a scary half-migration: we still migrate
	// the data (bundle paths keep resolving through the planted symlinks), but skip
	// the config rewrite and do NOT fail the migration on it alone. Reserve a
	// non-zero exit for real I/O / integrity failures.
	configParseable := true
	if err := m.configParseError(); err != nil {
		configParseable = false
		fmt.Fprintln(env.out, "pi-stack migrate: WARNING: config.toml could not be parsed; migrating data but leaving knowledge_bundles paths as-is - they still resolve via the compatibility symlink. Fix the file and re-run to rewrite paths.")
	}

	// 1. knowledge index — REBUILD at the new STATE path (never move). Seeded from
	//    the currently-configured bundles (still at their legacy homes at this
	//    point; a running serve rebuilds from the final paths anyway).
	add(m.migrateIndex(m.configuredBundles()))

	// 2. knowledge bundle — safe dir move (git repo, no live writer). Gate the
	//    config rewrite on the artifact's OUTCOME (moved / already-converged), not on
	//    the symlink target text alone (R7-1): a refused/broken/conflict bundle must
	//    not trigger a rewrite of its paths.
	bundleArt, bundleErr := m.migrateDir("knowledge", m.legBundle, m.newBundle, false,
		m.dirNonEmpty, func(staging string) error {
			if !m.dirNonEmpty(staging) {
				return fmt.Errorf("copied bundle is empty/unreadable")
			}
			return nil
		})
	add(bundleArt, bundleErr)
	bundleMigrated := m.artifactConverged(bundleArt)

	// 3. knowledge cache — safe dir move (re-cloneable). Same outcome-gated rewrite
	//    (R7-1): a dangling/refused cache handoff must NOT rewrite cache paths in
	//    config to a missing new location.
	cacheArt, cacheErr := m.migrateDir("cache", m.legCache, m.newCache, m.cachePinned,
		func(string) bool { return true }, nil)
	add(cacheArt, cacheErr)
	cacheMigrated := m.artifactConverged(cacheArt)

	// Config rewrite — ONE atomic, locked transaction covering both moves (B3).
	// O1: only when the config parses. An unparseable config was warned about up
	// front; the data is migrated and bundle paths resolve through the symlinks.
	if configParseable {
		n, cfgErr := m.rewriteConfig(bundleMigrated, cacheMigrated)
		rep.bundlesRewritten = n
		if cfgErr != nil {
			errs = append(errs, fmt.Errorf("config rewrite: %w", cfgErr))
		}
	}

	// 4. backups — merge-copy, collision-safe, legacy kept readable.
	add(m.migrateBackups())

	// 5. memory — LAST and flock-guarded.
	add(m.migrateMemory())

	return rep, errors.Join(errs...)
}

// --- receipt (Part D) --------------------------------------------------------

// printMigrateReceipt renders the report in the Part D layout: memory,
// knowledge, index, cache, backups, then the config summary + the honest
// "Nothing was deleted" line. An idempotent re-run (nothing changed, nothing
// refused) prints a single "nothing to do" line — no churn.
func printMigrateReceipt(w io.Writer, rep report) {
	byName := map[string]migrateArtifact{}
	anyChanged, anyRefused := false, false
	for _, a := range rep.artifacts {
		byName[a.name] = a
		if a.changed {
			anyChanged = true
		}
		switch a.action {
		case "refused-serve", "refused-conflict", "refused-broken", "refused-corrupt", "rebuild-failed", "pinned":
			anyRefused = true
		}
	}
	if !anyChanged && !anyRefused && rep.bundlesRewritten == 0 {
		fmt.Fprintln(w, "pi-stack: storage is already at the standard XDG layout — nothing to do.")
		return
	}
	fmt.Fprintln(w, "pi-stack: relocating storage to the standard XDG layout.")
	home := receiptHome()
	for _, name := range []string{"memory", "knowledge", "index", "cache", "backups"} {
		if a, ok := byName[name]; ok {
			printArtifact(w, a, home)
		}
	}
	if rep.bundlesRewritten > 0 {
		fmt.Fprintf(w, "  %-11s rewrote %d knowledge_bundles path(s)\n", "config", rep.bundlesRewritten)
	}
	fmt.Fprintln(w, "  Nothing was deleted. Your config file and keys did not move.")
}

func printArtifact(w io.Writer, a migrateArtifact, home string) {
	label := a.name
	switch a.action {
	case "moved":
		extra := "(moved"
		if a.symlink {
			extra += ", symlink left"
		}
		if a.safety != "" {
			extra += ", safety copy kept"
		}
		extra += ")"
		fmt.Fprintf(w, "  %-11s %s  ->  %s   %s\n", label, tildeHome(a.from, home), tildeHome(a.to, home), extra)
	case "rebuilt":
		fmt.Fprintf(w, "  %-11s (rebuilt)  ->  %s\n", label, tildeHome(a.to, home))
	case "copied":
		note := ""
		if a.note != "" {
			note = " " + a.note
		}
		fmt.Fprintf(w, "  %-11s %s  ->  %s   (copied, legacy kept)%s\n", label, tildeHome(a.from, home), tildeHome(a.to, home), note)
	case "pinned":
		fmt.Fprintf(w, "  %-11s pinned (env override set), left in place\n", label)
	case "refused-serve":
		fmt.Fprintf(w, "  %-11s SKIPPED — a running `pi-stack serve` holds the database.\n", label)
		fmt.Fprintf(w, "  %-11s stop it (`pi-stack serve stop`) and re-run `pi-stack migrate`. Nothing is wrong.\n", "")
	case "refused-conflict":
		if a.note != "" {
			fmt.Fprintf(w, "  %-11s SKIPPED — %s\n", label, a.note)
		} else {
			fmt.Fprintf(w, "  %-11s SKIPPED — data exists at BOTH the old and new location. Refusing to merge.\n", label)
			fmt.Fprintf(w, "  %-11s inspect %s and %s, then re-run.\n", "", tildeHome(a.from, home), tildeHome(a.to, home))
		}
	case "refused-broken":
		if a.note != "" {
			fmt.Fprintf(w, "  %-11s SKIPPED — %s\n", label, a.note)
		} else {
			fmt.Fprintf(w, "  %-11s SKIPPED — broken handoff: the legacy symlink points at a missing new dir.\n", label)
		}
	case "refused-corrupt":
		if a.note != "" {
			fmt.Fprintf(w, "  %-11s SKIPPED — %s\n", label, a.note)
		} else {
			fmt.Fprintf(w, "  %-11s SKIPPED — the database failed integrity_check; source left in place.\n", label)
		}
	case "rebuild-failed":
		fmt.Fprintf(w, "  %-11s index rebuild failed (a running serve will rebuild it): %s\n", label, a.note)
	case "left-in-place", "absent":
		// no-op: don't clutter the receipt with unchanged artifacts
	}
}

func receiptHome() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return ""
}

func tildeHome(p, home string) string {
	if home == "" || p == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if strings.HasPrefix(p, home+string(filepath.Separator)) {
		return "~" + p[len(home):]
	}
	return p
}

// --- shared fs helpers (production) ------------------------------------------

// copyFile copies a single regular file, fsyncing the destination. Used for the
// memory db+wal+shm subset and each backup archive.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// copyTreeFS recursively copies src to dst, preserving file modes and reproducing
// symlinks as symlinks. The production migrateEnv.copyTree.
func copyTreeFS(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		tgt, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(tgt, dst)
	}
	if info.IsDir() {
		if err := os.MkdirAll(dst, info.Mode().Perm()|0o700); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyTreeFS(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// fsyncDirFS fsyncs a directory so a rename into it is durable. Best-effort on
// platforms that reject a dir fsync. The production migrateEnv.fsyncDir.
func fsyncDirFS(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// isGitURL reports whether a bundle entry is a git URL (skipped by the config
// rewrite) rather than a local path: an explicit scheme (https://, ssh://, …) or
// the scp-like git@host:path form.
func isGitURL(s string) bool {
	if strings.Contains(s, "://") {
		return true
	}
	if !filepath.IsAbs(s) && strings.Contains(s, "@") && strings.Contains(s, ":") {
		return true
	}
	return false
}

// relUnder returns the relative path of child under base when child is base or a
// descendant of it (never an escaping "..").
func relUnder(base, child string) (string, bool) {
	rel, err := filepath.Rel(base, child)
	if err != nil {
		return "", false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// --- production wiring -------------------------------------------------------

// defaultMigrateEnv fills the seam from the real filesystem, the existing
// integrityCheckDB, and a reindex that rebuilds the index at its new STATE path.
func defaultMigrateEnv() migrateEnv {
	return migrateEnv{
		getenv:   os.Getenv,
		homeDir:  os.UserHomeDir,
		stat:     os.Stat,
		lstat:    os.Lstat,
		rename:   os.Rename,
		symlink:  os.Symlink,
		copyTree: copyTreeFS,
		fsyncDir: fsyncDirFS,
		acquireLock: func(path string) (func() error, bool, error) {
			// Any failure to take the NON-blocking flock (held by a daemon, or an
			// open error) means "unavailable" → refuse memory cleanly; migrate never
			// hard-fails on a lock it cannot get.
			rel, err := acquireLock(path)
			if err != nil {
				return nil, false, nil
			}
			return func() error { rel(); return nil }, true, nil
		},
		integrityCheck: integrityCheckDB,
		reindex:        defaultMigrateReindex,
		now:            time.Now,
		out:            os.Stdout,
	}
}

// defaultMigrateReindex rebuilds the knowledge index at its NEW STATE location
// (STATE/knowledge/index.db) from bundlePaths, reusing the built-in knowledge
// store exactly as `serve` would. It targets the explicit new path (NOT
// KnowledgeIndexPath(), whose read-fallback would resolve to the legacy name
// while the new index is still absent).
func defaultMigrateReindex(bundlePaths []string) error {
	state, err := config.StateDir()
	if err != nil {
		return err
	}
	idx := filepath.Join(state, "knowledge", "index.db")
	if err := os.MkdirAll(filepath.Dir(idx), 0o755); err != nil {
		return err
	}
	var embedder func(string) []float64
	if memEmbedderAvailable() {
		embedder = memEmbed
	}
	store, err := newKnowledgeStore(idx, embedder)
	if err != nil {
		return err
	}
	defer store.db.Close()
	_, _, err = store.reindex(bundlePaths)
	return err
}

// --- CLI --------------------------------------------------------------------

const migrateUsage = `usage: pi-stack-host migrate

  Relocate pi-stack storage to the standard XDG layout, EXPLICITLY and once:
    memory + knowledge bundle + backups -> ~/.local/share/pi-stack
    knowledge index + caches            -> ~/.local/state/pi-stack

  Nothing precious is deleted: each artifact is moved (or copied+verified across
  filesystems) and the legacy path is left as a symlink or a .pre-xdg-<ts> safety
  copy. The knowledge index is REBUILT, not moved. The memory database is moved
  only while its flock is free (a running serve defers it, safely). Re-running is
  safe and idempotent.`

// runMigrateCLI is the `pi-stack-host migrate` entry point: it builds the
// production seam, runs migrate, prints the Part D receipt, and exits non-zero
// only on a HARD error. A refused artifact (serve running, or a both-locations
// conflict) is a valid deferred state and exits 0 — re-running after the user
// resolves it completes the move.
func runMigrateCLI(argv []string) {
	for _, a := range argv {
		if a == "-h" || a == "--help" {
			fmt.Println(migrateUsage)
			return
		}
		fmt.Fprintf(os.Stderr, "pi-stack-host migrate: unknown argument %q\n\n%s\n", a, migrateUsage)
		os.Exit(2)
	}
	env := defaultMigrateEnv()
	rep, err := migrate(env)
	printMigrateReceipt(env.out, rep)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack-host migrate: %v\n", err)
		os.Exit(1)
	}
}
