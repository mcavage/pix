//go:build unix

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"pi-stack/host/config"
)

// --- test harness ------------------------------------------------------------

// migrateTestHome sets HOME to a fresh temp dir (so every config/path resolver +
// os.UserHomeDir converges there) and returns it. No XDG_* / PI_STACK_CONFIG is
// set, so DATA/STATE/CONFIG default under HOME.
func migrateTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Clear any inherited overrides so pins don't leak into the hermetic run.
	t.Setenv("MEMORY_DB", "")
	t.Setenv("KNOWLEDGE_DB", "")
	t.Setenv("KNOWLEDGE_CACHE_DIR", "")
	t.Setenv("PI_STACK_CONFIG", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	return home
}

// baseMigrateEnv is a production-like seam with a captured out buffer and a fake
// reindex that just creates the new index file (so migrateIndex reports rebuilt
// without needing a real OKF bundle).
func baseMigrateEnv(t *testing.T, home string, out *bytes.Buffer) migrateEnv {
	t.Helper()
	env := defaultMigrateEnv()
	env.out = out
	env.now = func() time.Time { return time.Unix(1700000000, 0) }
	env.reindex = func(_ []string) error {
		idx := filepath.Join(home, ".local", "state", "pi-stack", "knowledge", "index.db")
		if err := os.MkdirAll(filepath.Dir(idx), 0o755); err != nil {
			return err
		}
		return os.WriteFile(idx, []byte("fake-index"), 0o600)
	}
	return env
}

// seedMemDBAt builds a REAL sqlite memory.db (n facts) at dir/memory.db and
// closes it, so it passes integrity_check standalone.
func seedMemDBAt(t *testing.T, dir string, n int) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "memory.db")
	st, err := newMemStore(path, nil)
	if err != nil {
		t.Fatalf("newMemStore: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, err := st.remember(rememberInput{content: "fact " + string(rune('a'+i))}); err != nil {
			t.Fatalf("remember: %v", err)
		}
	}
	if err := st.db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return path
}

func mustDir(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func symlinkResolvesTo(t *testing.T, link, want string) {
	t.Helper()
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat %s: %v", link, err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is not a symlink", link)
	}
	tgt, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink %s: %v", link, err)
	}
	if !filepath.IsAbs(tgt) {
		tgt = filepath.Join(filepath.Dir(link), tgt)
	}
	if filepath.Clean(tgt) != filepath.Clean(want) {
		t.Fatalf("symlink %s -> %s, want %s", link, tgt, want)
	}
}

// exdevRename returns EXDEV for a legacy-root -> new-root move, delegating every
// other rename (staging->new, legacy->pre-xdg) to the real os.Rename. This models
// ~/.pi-stack + ~/.config/pi-stack on a different mount than ~/.local.
func exdevRename(home string) func(string, string) error {
	legacyRoots := []string{filepath.Join(home, ".pi-stack"), filepath.Join(home, ".config", "pi-stack")}
	newRoot := filepath.Join(home, ".local")
	under := func(p string, roots ...string) bool {
		for _, r := range roots {
			if p == r || strings.HasPrefix(p, r+string(filepath.Separator)) {
				return true
			}
		}
		return false
	}
	return func(old, new string) error {
		// A staging dir lives under a new root; only the raw legacy->new crossing is
		// EXDEV. Detect the crossing by source-in-legacy AND dest-in-new AND the
		// source is NOT a staging dir.
		if under(old, legacyRoots...) && under(new, newRoot) && !strings.Contains(old, ".staging-") {
			return syscall.EXDEV
		}
		return os.Rename(old, new)
	}
}

// --- tests -------------------------------------------------------------------

func TestMigrateHappySameFsSymlinkHandoff(t *testing.T) {
	home := migrateTestHome(t)
	legMem := filepath.Join(home, ".pi-stack", "memory")
	seedMemDBAt(t, legMem, 3)
	mustDir(t, filepath.Join(home, ".config", "pi-stack", "knowledge"), map[string]string{"concept.md": "x"})
	mustDir(t, filepath.Join(home, ".config", "pi-stack", "knowledge-cache", "repo1"), map[string]string{"f": "y"})
	mustDir(t, filepath.Join(home, ".pi-stack", "knowledge"), map[string]string{"knowledge.db": "legacy-index"}) // triggers a rebuild

	var out bytes.Buffer
	rep, err := migrate(baseMigrateEnv(t, home, &out))
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}

	newMem := filepath.Join(home, ".local", "share", "pi-stack", "memory")
	symlinkResolvesTo(t, legMem, newMem)
	if err := integrityCheckDB(filepath.Join(newMem, "memory.db")); err != nil {
		t.Fatalf("new memory db integrity: %v", err)
	}
	// Exactly one authority: legacy is a symlink, new is the real db.
	if fi, _ := os.Lstat(newMem); fi == nil || !fi.IsDir() {
		t.Fatalf("new memory dir missing")
	}
	newBundle := filepath.Join(home, ".local", "share", "pi-stack", "knowledge")
	symlinkResolvesTo(t, filepath.Join(home, ".config", "pi-stack", "knowledge"), newBundle)
	newCache := filepath.Join(home, ".local", "state", "pi-stack", "knowledge-cache")
	symlinkResolvesTo(t, filepath.Join(home, ".config", "pi-stack", "knowledge-cache"), newCache)
	if _, err := os.Stat(filepath.Join(home, ".local", "state", "pi-stack", "knowledge", "index.db")); err != nil {
		t.Fatalf("index not rebuilt: %v", err)
	}
	action := artifactAction(rep, "memory")
	if action != "moved" {
		t.Fatalf("memory action = %q, want moved", action)
	}
}

func TestMigrateExdevMemoryCopyVerifySafetyCopy(t *testing.T) {
	home := migrateTestHome(t)
	legMem := filepath.Join(home, ".pi-stack", "memory")
	seedMemDBAt(t, legMem, 4)

	var out bytes.Buffer
	env := baseMigrateEnv(t, home, &out)
	env.rename = exdevRename(home)
	rep, err := migrate(env)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}

	newMem := filepath.Join(home, ".local", "share", "pi-stack", "memory")
	if err := integrityCheckDB(filepath.Join(newMem, "memory.db")); err != nil {
		t.Fatalf("new db integrity: %v", err)
	}
	symlinkResolvesTo(t, legMem, newMem)
	// .pre-xdg-<ts> safety copy retained with the original db — nothing deleted.
	matches, _ := filepath.Glob(legMem + ".pre-xdg-*")
	if len(matches) != 1 {
		t.Fatalf("want 1 .pre-xdg safety copy, got %v", matches)
	}
	if err := integrityCheckDB(filepath.Join(matches[0], "memory.db")); err != nil {
		t.Fatalf("safety copy db integrity: %v", err)
	}
	if a := artifactBySafety(rep, "memory"); a == "" {
		t.Fatalf("report missing safety copy for memory")
	}
}

func TestMigrateExdevCorruptCopyRefused(t *testing.T) {
	home := migrateTestHome(t)
	legMem := filepath.Join(home, ".pi-stack", "memory")
	seedMemDBAt(t, legMem, 2)

	var out bytes.Buffer
	env := baseMigrateEnv(t, home, &out)
	env.rename = exdevRename(home)
	// Force the integrity gate to fail on the STAGING copy only (the new dir).
	env.integrityCheck = func(dbPath string) error {
		if strings.Contains(dbPath, ".staging-") {
			return errors.New("simulated corruption")
		}
		return integrityCheckDB(dbPath)
	}
	rep, err := migrate(env)
	if err == nil {
		t.Fatalf("want hard error on corrupt copy, got nil")
	}
	// Source intact, NO symlink planted, no new authority published.
	if fi, _ := os.Lstat(legMem); fi == nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("legacy memory should remain a real dir (source intact), got %v", fi)
	}
	if err := integrityCheckDB(filepath.Join(legMem, "memory.db")); err != nil {
		t.Fatalf("source db must remain intact: %v", err)
	}
	newMem := filepath.Join(home, ".local", "share", "pi-stack", "memory")
	if _, err := os.Stat(newMem); err == nil {
		t.Fatalf("new memory dir must NOT exist after refusal")
	}
	if got := artifactAction(rep, "memory"); got != "refused-corrupt" {
		t.Fatalf("memory action = %q, want refused-corrupt", got)
	}
	// No staging scratch left behind.
	if m, _ := filepath.Glob(newMem + ".staging-*"); len(m) != 0 {
		t.Fatalf("staging scratch left: %v", m)
	}
}

// corruptDBAt overwrites (or creates) dir/memory.db with garbage bytes so the
// real integrityCheckDB fails its quick_check — a deliberately corrupt file,
// like a truncated/garbled db on disk.
func corruptDBAt(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "memory.db")
	if err := os.WriteFile(path, []byte("this is not a sqlite database, it is garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := integrityCheckDB(path); err == nil {
		t.Fatalf("precondition: corrupt db must fail integrity_check")
	}
	return path
}

// TestMigrateSameFsCorruptLegacyRefused (R5-1a): a same-fs move of a CORRUPT
// legacy memory.db must be REFUSED symmetrically (mirroring the EXDEV corrupt
// gate), not renamed+symlinked as if healthy. The legacy dir is left exactly in
// place (still a real dir, no symlink planted, db bytes untouched), nothing is
// published at the new path, and migrate returns a hard error — while OTHER
// artifacts still migrate.
func TestMigrateSameFsCorruptLegacyRefused(t *testing.T) {
	home := migrateTestHome(t)
	legMem := filepath.Join(home, ".pi-stack", "memory")
	corruptPath := corruptDBAt(t, legMem)
	before, _ := os.ReadFile(corruptPath)
	// A healthy sibling artifact that MUST still migrate.
	mustDir(t, filepath.Join(home, ".config", "pi-stack", "knowledge"), map[string]string{"c.md": "x"})

	var out bytes.Buffer
	rep, err := migrate(baseMigrateEnv(t, home, &out))
	if err == nil {
		t.Fatalf("want hard error on corrupt legacy db, got nil")
	}
	if got := artifactAction(rep, "memory"); got != "refused-corrupt" {
		t.Fatalf("memory action = %q, want refused-corrupt", got)
	}
	// Legacy left EXACTLY in place: still a real dir, never a symlink.
	if fi, _ := os.Lstat(legMem); fi == nil || fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		t.Fatalf("legacy memory must remain a real dir (source left in place), got %v", fi)
	}
	if after, _ := os.ReadFile(corruptPath); !bytes.Equal(before, after) {
		t.Fatalf("legacy db bytes must be untouched")
	}
	// Nothing published at the new path.
	newMem := filepath.Join(home, ".local", "share", "pi-stack", "memory")
	if _, serr := os.Stat(newMem); serr == nil {
		t.Fatalf("new memory dir must NOT exist after refusal")
	}
	if m, _ := filepath.Glob(newMem + ".staging-*"); len(m) != 0 {
		t.Fatalf("no staging scratch may be left: %v", m)
	}
	// Clear guidance in the artifact note.
	if note := artifactNote(rep, "memory"); !strings.Contains(note, "failed integrity check") || !strings.Contains(note, "re-run pi-stack migrate") {
		t.Fatalf("memory note = %q, want a clear integrity-check-failed message", note)
	}
	// The healthy sibling still migrated.
	if got := artifactAction(rep, "knowledge"); got != "moved" {
		t.Fatalf("knowledge action = %q, want moved (other artifacts still migrate)", got)
	}
	symlinkResolvesTo(t, filepath.Join(home, ".config", "pi-stack", "knowledge"),
		filepath.Join(home, ".local", "share", "pi-stack", "knowledge"))
}

// TestMigrateSymlinkToNewCorruptNewIsComplete (R5-1b): a rerun after a completed
// handoff — legacy is a symlink->new — must be classified COMPLETE/converged even
// when the NEW db is corrupt. It must NEVER be "resumable" (which used to rename
// the symlink over the live new dir -> a permanent hard failure). No move is
// attempted, no error is returned, and the pending-migration state stays cleared;
// a separate diagnostic note flags the possibly-corrupt db.
func TestMigrateSymlinkToNewCorruptNewIsComplete(t *testing.T) {
	home := migrateTestHome(t)
	newMem := filepath.Join(home, ".local", "share", "pi-stack", "memory")
	corruptDBAt(t, newMem) // new is the authority, but its db is corrupt
	legMem := filepath.Join(home, ".pi-stack", "memory")
	if err := os.MkdirAll(filepath.Dir(legMem), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(newMem, legMem); err != nil { // completed handoff
		t.Fatal(err)
	}
	newBefore := dirFingerprint(t, newMem)

	var out bytes.Buffer
	rep, err := migrate(baseMigrateEnv(t, home, &out))
	if err != nil {
		t.Fatalf("converged symlink->new must not error: %v", err)
	}
	if got := artifactAction(rep, "memory"); got != "left-in-place" {
		t.Fatalf("memory action = %q, want left-in-place (complete, not resumable)", got)
	}
	// Legacy symlink untouched and still resolves to new; new dir not renamed over.
	symlinkResolvesTo(t, legMem, newMem)
	if dirFingerprint(t, newMem) != newBefore {
		t.Fatalf("new dir must not be mutated on a converged rerun")
	}
	if m, _ := filepath.Glob(newMem + ".staging-*"); len(m) != 0 {
		t.Fatalf("no staging scratch may be left: %v", m)
	}
	// Separate diagnostic surfaced (the db may be corrupt) without any move.
	if note := artifactNote(rep, "memory"); !strings.Contains(note, "may be corrupt") {
		t.Fatalf("memory note = %q, want a corrupt-db diagnostic", note)
	}
	if !strings.Contains(out.String(), "may be corrupt") {
		t.Fatalf("expected a corrupt-db WARNING on stdout:\n%s", out.String())
	}
}

// TestMigrateDanglingSymlinkToMissingNewRefused (R6-1): the legacy path is a
// symlink -> the expected new dir, but the new dir was DELETED (dangling). This
// must be classified BROKEN (a refusal), NEVER "resumable": the old code renamed
// the dangling link over newDir, planting a self-referential newDir->newDir loop
// and reporting success. Assert: refused-broken action, a clear message, a HARD
// error (exit reflects a real problem), the legacy symlink is left untouched (no
// rename), and NO self-referential loop is created at the new path.
func TestMigrateDanglingSymlinkToMissingNewRefused(t *testing.T) {
	home := migrateTestHome(t)
	newMem := filepath.Join(home, ".local", "share", "pi-stack", "memory")
	legMem := filepath.Join(home, ".pi-stack", "memory")
	if err := os.MkdirAll(filepath.Dir(legMem), 0o755); err != nil {
		t.Fatal(err)
	}
	// A completed handoff whose new dir the user then deleted: legMem -> newMem,
	// but newMem does not exist.
	if err := os.Symlink(newMem, legMem); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	rep, err := migrate(baseMigrateEnv(t, home, &out))
	if err == nil {
		t.Fatalf("a dangling symlink->missing-new must be a HARD error (exit reflects a real problem)")
	}
	if got := artifactAction(rep, "memory"); got != "refused-broken" {
		t.Fatalf("memory action = %q, want refused-broken (NOT resumable)", got)
	}
	if note := artifactNote(rep, "memory"); !strings.Contains(note, "broken handoff") || !strings.Contains(note, "missing") {
		t.Fatalf("memory note = %q, want a clear broken-handoff message", note)
	}
	// The legacy symlink is left untouched, still pointing at newMem — no rename.
	symlinkResolvesTo(t, legMem, newMem)
	// NO self-referential loop planted at the new path: newMem must NOT be a symlink
	// (the bug renamed the dangling link onto newMem, making newMem -> newMem).
	if fi, e := os.Lstat(newMem); e == nil && fi.Mode()&os.ModeSymlink != 0 {
		tgt, _ := os.Readlink(newMem)
		t.Fatalf("new path must not become a symlink loop: %s -> %s", newMem, tgt)
	}
	if m, _ := filepath.Glob(newMem + ".staging-*"); len(m) != 0 {
		t.Fatalf("no staging scratch may be left: %v", m)
	}
}

// TestMigrateDanglingSymlinkBundleRefused (R6-1): the same broken-handoff refusal
// on the generic migrateDir path (knowledge bundle), so both the memory and the
// generic artifact paths refuse a dangling symlink source rather than looping.
func TestMigrateDanglingSymlinkBundleRefused(t *testing.T) {
	home := migrateTestHome(t)
	legBundle := filepath.Join(home, ".config", "pi-stack", "knowledge")
	newBundle := filepath.Join(home, ".local", "share", "pi-stack", "knowledge")
	if err := os.MkdirAll(filepath.Dir(legBundle), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(newBundle, legBundle); err != nil { // dangling: newBundle absent
		t.Fatal(err)
	}

	var out bytes.Buffer
	rep, err := migrate(baseMigrateEnv(t, home, &out))
	if err == nil {
		t.Fatalf("a dangling bundle symlink->missing-new must be a HARD error")
	}
	if got := artifactAction(rep, "knowledge"); got != "refused-broken" {
		t.Fatalf("knowledge action = %q, want refused-broken (NOT resumable)", got)
	}
	symlinkResolvesTo(t, legBundle, newBundle) // untouched, no rename
	if fi, e := os.Lstat(newBundle); e == nil && fi.Mode()&os.ModeSymlink != 0 {
		tgt, _ := os.Readlink(newBundle)
		t.Fatalf("new path must not become a symlink loop: %s -> %s", newBundle, tgt)
	}
}

// TestMigrateDanglingCacheSymlinkNoConfigRewrite (R7-1): a dangling legacy cache
// symlink->missing-new is a broken handoff. migrate must refuse it (refused-broken,
// HARD error -> exit 1) and, crucially, must NOT rewrite the config's cache paths
// to the missing new location. The unified converged() predicate gates the cache
// config rewrite on a REAL success, never on the symlink target text alone.
func TestMigrateDanglingCacheSymlinkNoConfigRewrite(t *testing.T) {
	home := migrateTestHome(t)
	legCache := filepath.Join(home, ".config", "pi-stack", "knowledge-cache")
	newCache := filepath.Join(home, ".local", "state", "pi-stack", "knowledge-cache")
	if err := os.MkdirAll(filepath.Dir(legCache), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(newCache, legCache); err != nil { // dangling: newCache absent
		t.Fatal(err)
	}
	// A configured bundle spelled under the OLD cache root.
	cacheRepo := filepath.Join(legCache, "repo1")
	cfgPath := filepath.Join(home, ".config", "pi-stack", "config.toml")
	if err := os.WriteFile(cfgPath, []byte("knowledge_bundles = [\""+cacheRepo+"\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	rep, err := migrate(baseMigrateEnv(t, home, &out))
	if err == nil {
		t.Fatalf("a dangling cache symlink->missing-new must be a HARD error (exit 1)")
	}
	if got := artifactAction(rep, "cache"); got != "refused-broken" {
		t.Fatalf("cache action = %q, want refused-broken", got)
	}
	if rep.bundlesRewritten != 0 {
		t.Fatalf("bundlesRewritten = %d, want 0 (a broken cache handoff must not rewrite config)", rep.bundlesRewritten)
	}
	cfg, err := config.LoadFrom(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !containsStr(cfg.KnowledgeBundles, cacheRepo) {
		t.Fatalf("cache config entry must be left unchanged, got %v", cfg.KnowledgeBundles)
	}
	// The dangling link is left exactly as it was (no rename onto newCache).
	symlinkResolvesTo(t, legCache, newCache)
}

func TestMigrateResumableStagingDiscarded(t *testing.T) {
	home := migrateTestHome(t)
	legMem := filepath.Join(home, ".pi-stack", "memory")
	seedMemDBAt(t, legMem, 2)
	// Seed a leftover staging dir from a "crashed" prior run.
	newMem := filepath.Join(home, ".local", "share", "pi-stack", "memory")
	staging := newMem + ".staging-deadbeef"
	mustDir(t, staging, map[string]string{"memory.db": "garbage"})

	var out bytes.Buffer
	if _, err := migrate(baseMigrateEnv(t, home, &out)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if m, _ := filepath.Glob(newMem + ".staging-*"); len(m) != 0 {
		t.Fatalf("staging not discarded: %v", m)
	}
	symlinkResolvesTo(t, legMem, newMem)
	if err := integrityCheckDB(filepath.Join(newMem, "memory.db")); err != nil {
		t.Fatalf("new db integrity after resume: %v", err)
	}
}

func TestMigrateConflictRefusedNoMutation(t *testing.T) {
	home := migrateTestHome(t)
	legMem := filepath.Join(home, ".pi-stack", "memory")
	newMem := filepath.Join(home, ".local", "share", "pi-stack", "memory")
	seedMemDBAt(t, legMem, 3)
	seedMemDBAt(t, newMem, 5) // independent content at BOTH locations

	legBefore := dirFingerprint(t, legMem)
	newBefore := dirFingerprint(t, newMem)

	var out bytes.Buffer
	rep, err := migrate(baseMigrateEnv(t, home, &out))
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if got := artifactAction(rep, "memory"); got != "refused-conflict" {
		t.Fatalf("memory action = %q, want refused-conflict", got)
	}
	// Neither side mutated; legacy is still a REAL dir (not a symlink).
	if fi, _ := os.Lstat(legMem); fi == nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("legacy must remain a real dir")
	}
	if dirFingerprint(t, legMem) != legBefore || dirFingerprint(t, newMem) != newBefore {
		t.Fatalf("conflict must not mutate either side")
	}
}

// TestMigrateNonDirAtNewCacheIsConflict (R8-1): a REGULAR FILE sitting at the new
// cache path, with the legacy cache absent, must classify as a CONFLICT (never
// "replant" via the always-true cache validator + stat-existence). No symlink is
// planted at the legacy path, the rogue file is left untouched, config with an
// entry under the old cache root is NOT rewritten, and migrate exits 0 (the
// conflict convention) with a clear "unexpected non-directory" receipt line.
func TestMigrateNonDirAtNewCacheIsConflict(t *testing.T) {
	home := migrateTestHome(t)
	legCache := filepath.Join(home, ".config", "pi-stack", "knowledge-cache")
	newCache := filepath.Join(home, ".local", "state", "pi-stack", "knowledge-cache")
	// A stray regular FILE where the new cache DIRECTORY should be, legacy absent.
	if err := os.MkdirAll(filepath.Dir(newCache), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newCache, []byte("i am not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A config whose bundle entry lives under the OLD cache root must NOT be
	// rewritten while the cache handoff is a conflict.
	cfgPath := filepath.Join(home, ".config", "pi-stack", "config.toml")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	cacheEntry := filepath.Join(legCache, "repo1")
	cfgBody := "knowledge_bundles = [\"" + cacheEntry + "\"]\n"
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgBefore, _ := os.ReadFile(cfgPath)

	var out bytes.Buffer
	rep, err := migrate(baseMigrateEnv(t, home, &out))
	if err != nil {
		t.Fatalf("migrate must not hard-fail on a conflict (exit-0 convention): %v", err)
	}
	if got := artifactAction(rep, "cache"); got != "refused-conflict" {
		t.Fatalf("cache action = %q, want refused-conflict", got)
	}
	if rep.bundlesRewritten != 0 {
		t.Fatalf("config must NOT be rewritten on a conflict; rewrote %d", rep.bundlesRewritten)
	}
	// No symlink planted at the legacy cache path (it stays absent).
	if fi, lerr := os.Lstat(legCache); lerr == nil {
		t.Fatalf("legacy cache must stay absent, found mode %v", fi.Mode())
	}
	// The rogue file is left exactly as-is (a regular file, same bytes).
	fi, serr := os.Lstat(newCache)
	if serr != nil || !fi.Mode().IsRegular() {
		t.Fatalf("rogue file at new cache must be left untouched as a regular file: %v %v", fi, serr)
	}
	if b, _ := os.ReadFile(newCache); string(b) != "i am not a directory" {
		t.Fatalf("rogue file content changed: %q", string(b))
	}
	// Config file byte-identical (untouched).
	if cfgAfter, _ := os.ReadFile(cfgPath); !bytes.Equal(cfgBefore, cfgAfter) {
		t.Fatalf("config must be byte-identical after a conflict")
	}
	// The receipt names the specific reason (rendered from the artifact note).
	var receipt bytes.Buffer
	printMigrateReceipt(&receipt, rep)
	if !strings.Contains(receipt.String(), "unexpected non-directory") {
		t.Fatalf("receipt missing the non-directory conflict reason:\n%s", receipt.String())
	}
}

// TestMigrateNonDirAtNewMemoryIsConflict (R8-1): a REGULAR FILE at the new MEMORY
// path must classify as a conflict, never plant a symlink at the legacy memory
// path, and leave the rogue file untouched. migrate exits 0.
func TestMigrateNonDirAtNewMemoryIsConflict(t *testing.T) {
	home := migrateTestHome(t)
	legMem := filepath.Join(home, ".pi-stack", "memory")
	newMem := filepath.Join(home, ".local", "share", "pi-stack", "memory")
	if err := os.MkdirAll(filepath.Dir(newMem), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newMem, []byte("not a memory dir"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	rep, err := migrate(baseMigrateEnv(t, home, &out))
	if err != nil {
		t.Fatalf("migrate must not hard-fail on a conflict: %v", err)
	}
	if got := artifactAction(rep, "memory"); got != "refused-conflict" {
		t.Fatalf("memory action = %q, want refused-conflict", got)
	}
	// No symlink planted at the legacy memory path (stays absent).
	if fi, lerr := os.Lstat(legMem); lerr == nil {
		t.Fatalf("legacy memory must stay absent, found mode %v", fi.Mode())
	}
	// Rogue file untouched.
	fi, serr := os.Lstat(newMem)
	if serr != nil || !fi.Mode().IsRegular() {
		t.Fatalf("rogue file at new memory must be left untouched: %v %v", fi, serr)
	}
	if b, _ := os.ReadFile(newMem); string(b) != "not a memory dir" {
		t.Fatalf("rogue memory file content changed: %q", string(b))
	}
}

// TestMigrateNonDirAtLegacyCacheIsConflict (R9-1): a REGULAR FILE sitting at the
// LEGACY cache path, with the new cache absent, must classify as a CONFLICT (the
// symmetric twin of the non-dir-at-new case). The old code treated any present
// non-symlink legacy path as a movable/resumable source: it would rename the file
// onto newCache, plant a legacy symlink, mark changed, and rewrite config to an
// unusable path. Assert: refused-conflict, no rename, no symlink planted, the
// rogue file left untouched, a config entry under the old cache root NOT
// rewritten, and migrate exits 0 with a clear "unexpected non-directory" receipt.
func TestMigrateNonDirAtLegacyCacheIsConflict(t *testing.T) {
	home := migrateTestHome(t)
	legCache := filepath.Join(home, ".config", "pi-stack", "knowledge-cache")
	newCache := filepath.Join(home, ".local", "state", "pi-stack", "knowledge-cache")
	// A stray regular FILE where the legacy cache DIRECTORY should be, new absent.
	if err := os.MkdirAll(filepath.Dir(legCache), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legCache, []byte("i am not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	legBefore, _ := os.ReadFile(legCache)

	// A config whose bundle entry lives under the OLD cache root must NOT be
	// rewritten while the cache handoff is a conflict.
	cfgPath := filepath.Join(home, ".config", "pi-stack", "config.toml")
	cacheEntry := filepath.Join(legCache, "repo1")
	cfgBody := "knowledge_bundles = [\"" + cacheEntry + "\"]\n"
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgBefore, _ := os.ReadFile(cfgPath)

	var out bytes.Buffer
	rep, err := migrate(baseMigrateEnv(t, home, &out))
	if err != nil {
		t.Fatalf("migrate must not hard-fail on a conflict (exit-0 convention): %v", err)
	}
	if got := artifactAction(rep, "cache"); got != "refused-conflict" {
		t.Fatalf("cache action = %q, want refused-conflict", got)
	}
	if rep.bundlesRewritten != 0 {
		t.Fatalf("config must NOT be rewritten on a conflict; rewrote %d", rep.bundlesRewritten)
	}
	// No symlink planted at the new cache path (it stays absent).
	if fi, lerr := os.Lstat(newCache); lerr == nil {
		t.Fatalf("new cache must stay absent, found mode %v", fi.Mode())
	}
	// The rogue file is left exactly as-is (a regular file, same bytes, not a symlink).
	fi, serr := os.Lstat(legCache)
	if serr != nil || !fi.Mode().IsRegular() {
		t.Fatalf("rogue file at legacy cache must be left untouched as a regular file: %v %v", fi, serr)
	}
	if after, _ := os.ReadFile(legCache); !bytes.Equal(legBefore, after) {
		t.Fatalf("rogue legacy file content changed: %q", string(after))
	}
	// Config file byte-identical (untouched).
	if cfgAfter, _ := os.ReadFile(cfgPath); !bytes.Equal(cfgBefore, cfgAfter) {
		t.Fatalf("config must be byte-identical after a conflict")
	}
	// The receipt names the specific reason (rendered from the artifact note).
	var receipt bytes.Buffer
	printMigrateReceipt(&receipt, rep)
	if !strings.Contains(receipt.String(), "unexpected non-directory at legacy path") {
		t.Fatalf("receipt missing the legacy non-directory conflict reason:\n%s", receipt.String())
	}
}

// TestMigrateNonDirAtLegacyBundleIsConflict (R9-1): a REGULAR FILE at the LEGACY
// knowledge BUNDLE path (the generic migrateDir path with a real validator) must
// classify as a conflict, never rename the file onto the new bundle, never plant
// a symlink, leave the rogue file untouched, and not rewrite config. migrate
// exits 0.
func TestMigrateNonDirAtLegacyBundleIsConflict(t *testing.T) {
	home := migrateTestHome(t)
	legBundle := filepath.Join(home, ".config", "pi-stack", "knowledge")
	newBundle := filepath.Join(home, ".local", "share", "pi-stack", "knowledge")
	if err := os.MkdirAll(filepath.Dir(legBundle), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legBundle, []byte("not a bundle dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	legBefore, _ := os.ReadFile(legBundle)

	// A config pointing at the legacy default bundle must NOT be rewritten.
	cfgPath := filepath.Join(home, ".config", "pi-stack", "config.toml")
	cfgBody := "knowledge_bundles = [\"" + legBundle + "\"]\n"
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgBefore, _ := os.ReadFile(cfgPath)

	var out bytes.Buffer
	rep, err := migrate(baseMigrateEnv(t, home, &out))
	if err != nil {
		t.Fatalf("migrate must not hard-fail on a conflict: %v", err)
	}
	if got := artifactAction(rep, "knowledge"); got != "refused-conflict" {
		t.Fatalf("knowledge action = %q, want refused-conflict", got)
	}
	if rep.bundlesRewritten != 0 {
		t.Fatalf("config must NOT be rewritten on a conflict; rewrote %d", rep.bundlesRewritten)
	}
	// No symlink/dir planted at the new bundle path (stays absent).
	if fi, lerr := os.Lstat(newBundle); lerr == nil {
		t.Fatalf("new bundle must stay absent, found mode %v", fi.Mode())
	}
	// Rogue file untouched (still a regular file, same bytes).
	fi, serr := os.Lstat(legBundle)
	if serr != nil || !fi.Mode().IsRegular() {
		t.Fatalf("rogue file at legacy bundle must be left untouched: %v %v", fi, serr)
	}
	if after, _ := os.ReadFile(legBundle); !bytes.Equal(legBefore, after) {
		t.Fatalf("rogue legacy bundle file content changed: %q", string(after))
	}
	// Config byte-identical.
	if cfgAfter, _ := os.ReadFile(cfgPath); !bytes.Equal(cfgBefore, cfgAfter) {
		t.Fatalf("config must be byte-identical after a conflict")
	}
	// The note names the specific reason.
	if note := artifactNote(rep, "knowledge"); !strings.Contains(note, "unexpected non-directory at legacy path") {
		t.Fatalf("knowledge note = %q, want the legacy non-directory reason", note)
	}
}

func TestMigrateFlockHeldMemoryRefusedOthersMigrate(t *testing.T) {
	home := migrateTestHome(t)
	seedMemDBAt(t, filepath.Join(home, ".pi-stack", "memory"), 2)
	mustDir(t, filepath.Join(home, ".config", "pi-stack", "knowledge"), map[string]string{"c.md": "x"})

	var out bytes.Buffer
	env := baseMigrateEnv(t, home, &out)
	env.acquireLock = func(string) (func() error, bool, error) { return nil, false, nil } // held
	rep, err := migrate(env)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if got := artifactAction(rep, "memory"); got != "refused-serve" {
		t.Fatalf("memory action = %q, want refused-serve", got)
	}
	// Legacy memory untouched (still a real dir).
	if fi, _ := os.Lstat(filepath.Join(home, ".pi-stack", "memory")); fi == nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("legacy memory must be untouched")
	}
	// Other artifacts still migrated.
	if got := artifactAction(rep, "knowledge"); got != "moved" {
		t.Fatalf("knowledge action = %q, want moved", got)
	}
	symlinkResolvesTo(t, filepath.Join(home, ".config", "pi-stack", "knowledge"),
		filepath.Join(home, ".local", "share", "pi-stack", "knowledge"))
}

func TestMigrateConfigRewriteBaseAndProfileIdempotent(t *testing.T) {
	home := migrateTestHome(t)
	bundleDefault := filepath.Join(home, ".config", "pi-stack", "knowledge")
	cacheRepo := filepath.Join(home, ".config", "pi-stack", "knowledge-cache", "repo1")
	mustDir(t, bundleDefault, map[string]string{"c.md": "x"})
	mustDir(t, cacheRepo, map[string]string{"f": "y"})

	gitURL := "https://github.com/acme/kb.git"
	cfgPath := filepath.Join(home, ".config", "pi-stack", "config.toml")
	cfgBody := "" +
		"knowledge_bundles = [\"" + bundleDefault + "\", \"" + cacheRepo + "\", \"" + gitURL + "\"]\n\n" +
		"[profiles.work]\n" +
		"knowledge_bundles = [\"" + bundleDefault + "\"]\n"
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	rep, err := migrate(baseMigrateEnv(t, home, &out))
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if rep.bundlesRewritten == 0 {
		t.Fatalf("expected bundles rewritten > 0")
	}

	newBundle := filepath.Join(home, ".local", "share", "pi-stack", "knowledge")
	newCacheRepo := filepath.Join(home, ".local", "state", "pi-stack", "knowledge-cache", "repo1")
	cfg, err := config.LoadFrom(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !containsStr(cfg.KnowledgeBundles, newBundle) {
		t.Fatalf("base bundle not rewritten: %v", cfg.KnowledgeBundles)
	}
	if !containsStr(cfg.KnowledgeBundles, newCacheRepo) {
		t.Fatalf("base cache path not rewritten: %v", cfg.KnowledgeBundles)
	}
	if !containsStr(cfg.KnowledgeBundles, gitURL) {
		t.Fatalf("git URL must be untouched: %v", cfg.KnowledgeBundles)
	}
	wp := cfg.Profiles["work"]
	if wp.KnowledgeBundles == nil || !containsStr(*wp.KnowledgeBundles, newBundle) {
		t.Fatalf("profile override not rewritten: %+v", wp.KnowledgeBundles)
	}

	// Idempotent: a second run rewrites nothing and leaves the file byte-identical.
	after1, _ := os.ReadFile(cfgPath)
	var out2 bytes.Buffer
	rep2, err := migrate(baseMigrateEnv(t, home, &out2))
	if err != nil {
		t.Fatalf("migrate 2: %v", err)
	}
	if rep2.bundlesRewritten != 0 {
		t.Fatalf("second run rewrote %d bundles, want 0", rep2.bundlesRewritten)
	}
	after2, _ := os.ReadFile(cfgPath)
	if !bytes.Equal(after1, after2) {
		t.Fatalf("config not byte-stable across idempotent runs")
	}
}

func TestMigrateBackupsMergeCopyCollisionSafe(t *testing.T) {
	home := migrateTestHome(t)
	legBackups := filepath.Join(home, ".pi-stack", "backups")
	mustDir(t, legBackups, map[string]string{
		"pi-stack-backup-20200101-000000.tar.gz": "AAAA",
		"pi-stack-backup-20200101-000001.tar.gz": "BBBB",
		"keepme.txt":                             "not a backup",
	})
	// Pre-seed the NEW dir with a same-named-but-different-content archive -> forces
	// a collision-safe -<rand> copy rather than a clobber.
	newBackups := filepath.Join(home, ".local", "share", "pi-stack", "backups")
	mustDir(t, newBackups, map[string]string{
		"pi-stack-backup-20200101-000000.tar.gz": "DIFFERENT-CONTENT",
	})

	var out bytes.Buffer
	rep, err := migrate(baseMigrateEnv(t, home, &out))
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if got := artifactAction(rep, "backups"); got != "copied" {
		t.Fatalf("backups action = %q, want copied", got)
	}
	// Legacy archives still present (restore-by-legacy-path still works).
	for _, n := range []string{"pi-stack-backup-20200101-000000.tar.gz", "pi-stack-backup-20200101-000001.tar.gz"} {
		if _, err := os.Stat(filepath.Join(legBackups, n)); err != nil {
			t.Fatalf("legacy archive %s missing: %v", n, err)
		}
	}
	entries, _ := os.ReadDir(newBackups)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	// The pre-existing different archive is preserved, the second archive copied,
	// and the colliding one landed under a collision-safe name (3 total).
	if len(names) != 3 {
		t.Fatalf("new backups dir has %v, want 3 archives", names)
	}
	if containsStr(names, "keepme.txt") {
		t.Fatalf("non-backup file must NOT be copied: %v", names)
	}
}

func TestMigrateIdempotentFullReRunNoChurn(t *testing.T) {
	home := migrateTestHome(t)
	seedMemDBAt(t, filepath.Join(home, ".pi-stack", "memory"), 3)
	mustDir(t, filepath.Join(home, ".config", "pi-stack", "knowledge"), map[string]string{"c.md": "x"})

	var out1 bytes.Buffer
	if _, err := migrate(baseMigrateEnv(t, home, &out1)); err != nil {
		t.Fatalf("migrate 1: %v", err)
	}

	var out2 bytes.Buffer
	rep2, err := migrate(baseMigrateEnv(t, home, &out2))
	if err != nil {
		t.Fatalf("migrate 2: %v", err)
	}
	for _, a := range rep2.artifacts {
		if a.changed {
			t.Fatalf("second run changed artifact %q (action %q)", a.name, a.action)
		}
	}
	var rc bytes.Buffer
	printMigrateReceipt(&rc, rep2)
	if !strings.Contains(rc.String(), "nothing to do") {
		t.Fatalf("idempotent re-run receipt = %q, want 'nothing to do'", rc.String())
	}
}

func TestMigrateEnvOverridePins(t *testing.T) {
	home := migrateTestHome(t)
	seedMemDBAt(t, filepath.Join(home, ".pi-stack", "memory"), 1)
	t.Setenv("MEMORY_DB", filepath.Join(home, "custom", "memory.db"))
	t.Setenv("KNOWLEDGE_DB", filepath.Join(home, "custom", "index.db"))

	var out bytes.Buffer
	rep, err := migrate(baseMigrateEnv(t, home, &out))
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if got := artifactAction(rep, "memory"); got != "pinned" {
		t.Fatalf("memory action = %q, want pinned", got)
	}
	if got := artifactAction(rep, "index"); got != "pinned" {
		t.Fatalf("index action = %q, want pinned", got)
	}
	// The pinned legacy memory must be left completely alone.
	if fi, _ := os.Lstat(filepath.Join(home, ".pi-stack", "memory")); fi == nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("pinned memory must be untouched")
	}
}

// TestMigrateReceiptText renders the three headline receipts for the record.
func TestMigrateReceiptText(t *testing.T) {
	home := "/home/u"
	t.Setenv("HOME", home)
	happy := report{
		artifacts: []migrateArtifact{
			{name: "memory", action: "moved", from: home + "/.pi-stack/memory", to: home + "/.local/share/pi-stack/memory", symlink: true, changed: true},
			{name: "knowledge", action: "moved", from: home + "/.config/pi-stack/knowledge", to: home + "/.local/share/pi-stack/knowledge", symlink: true, changed: true},
			{name: "index", action: "rebuilt", to: home + "/.local/state/pi-stack/knowledge/index.db", changed: true},
			{name: "cache", action: "moved", from: home + "/.config/pi-stack/knowledge-cache", to: home + "/.local/state/pi-stack/knowledge-cache", symlink: true, changed: true},
			{name: "backups", action: "copied", from: home + "/.pi-stack/backups", to: home + "/.local/share/pi-stack/backups", note: "2 archive(s)", changed: true},
		},
		bundlesRewritten: 2,
	}
	var b bytes.Buffer
	printMigrateReceipt(&b, happy)
	if !strings.Contains(b.String(), "Nothing was deleted") {
		t.Fatalf("happy receipt missing 'Nothing was deleted':\n%s", b.String())
	}
	t.Logf("HAPPY RECEIPT:\n%s", b.String())

	serveUp := report{artifacts: []migrateArtifact{{name: "memory", action: "refused-serve"}}}
	b.Reset()
	printMigrateReceipt(&b, serveUp)
	t.Logf("SERVE-UP RECEIPT:\n%s", b.String())

	conflict := report{artifacts: []migrateArtifact{{name: "memory", action: "refused-conflict", from: home + "/.pi-stack/memory", to: home + "/.local/share/pi-stack/memory"}}}
	b.Reset()
	printMigrateReceipt(&b, conflict)
	t.Logf("CONFLICT RECEIPT:\n%s", b.String())

	// A broken-handoff refusal renders its clear note (R6-1).
	broken := report{artifacts: []migrateArtifact{{name: "memory", action: "refused-broken", note: brokenHandoffNote(home+"/.pi-stack/memory", home+"/.local/share/pi-stack/memory")}}}
	b.Reset()
	printMigrateReceipt(&b, broken)
	if s := b.String(); !strings.Contains(s, "SKIPPED") || !strings.Contains(s, "broken handoff") {
		t.Fatalf("broken-handoff receipt missing the clear note:\n%s", s)
	}
	t.Logf("BROKEN RECEIPT:\n%s", b.String())
}

// TestMigrateUnparseableConfigSkipsRewriteExitsZero (O1): an unparseable
// config.toml must NOT leave a scary half-migration. The data migrates, the
// config rewrite is skipped with a clear warning, migrate exits 0, and the
// bundle path still resolves through the planted symlink.
func TestMigrateUnparseableConfigSkipsRewriteExitsZero(t *testing.T) {
	home := migrateTestHome(t)
	legMem := filepath.Join(home, ".pi-stack", "memory")
	seedMemDBAt(t, legMem, 2)
	legBundle := filepath.Join(home, ".config", "pi-stack", "knowledge")
	mustDir(t, legBundle, map[string]string{"c.md": "x"})

	// A basic string with an embedded newline is illegal TOML -> a hard parse error.
	cfgPath := filepath.Join(home, ".config", "pi-stack", "config.toml")
	if err := os.WriteFile(cfgPath, []byte("knowledge_bundles = \"unclosed\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	rep, err := migrate(baseMigrateEnv(t, home, &out))
	if err != nil {
		t.Fatalf("unparseable config must not fail migrate: %v", err)
	}
	// Data migrated: legacy now symlinks -> new for both memory and the bundle.
	newMem := filepath.Join(home, ".local", "share", "pi-stack", "memory")
	symlinkResolvesTo(t, legMem, newMem)
	if err := integrityCheckDB(filepath.Join(newMem, "memory.db")); err != nil {
		t.Fatalf("new memory db integrity: %v", err)
	}
	newBundle := filepath.Join(home, ".local", "share", "pi-stack", "knowledge")
	symlinkResolvesTo(t, legBundle, newBundle) // bundle path still resolves via symlink
	// Rewrite skipped.
	if rep.bundlesRewritten != 0 {
		t.Fatalf("bundlesRewritten = %d, want 0 (rewrite skipped)", rep.bundlesRewritten)
	}
	// Clear warning printed.
	if !strings.Contains(out.String(), "config.toml could not be parsed") {
		t.Fatalf("missing unparseable-config warning:\n%s", out.String())
	}
	// The config file was left untouched (still unparseable).
	if _, lerr := config.LoadFrom(cfgPath); lerr == nil {
		t.Fatalf("config should remain unparseable (rewrite must have been skipped)")
	}
}

// TestMigrateIndexSkipsGitURLBundles (O2): reindex must receive only LOCAL,
// EXISTING bundle roots. A git URL entry alongside a real local bundle is
// filtered out (it would otherwise fail with "stat https://...: no such file").
func TestMigrateIndexSkipsGitURLBundles(t *testing.T) {
	home := migrateTestHome(t)
	localBundle := filepath.Join(home, ".config", "pi-stack", "knowledge")
	mustDir(t, localBundle, map[string]string{"c.md": "x"})
	gitURL := "https://github.com/acme/kb.git"
	cfgPath := filepath.Join(home, ".config", "pi-stack", "config.toml")
	cfgBody := "knowledge_bundles = [\"" + gitURL + "\", \"" + localBundle + "\"]\n"
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o600); err != nil {
		t.Fatal(err)
	}

	var got []string
	var out bytes.Buffer
	env := baseMigrateEnv(t, home, &out)
	env.reindex = func(paths []string) error {
		got = append([]string(nil), paths...)
		idx := filepath.Join(home, ".local", "state", "pi-stack", "knowledge", "index.db")
		if err := os.MkdirAll(filepath.Dir(idx), 0o755); err != nil {
			return err
		}
		return os.WriteFile(idx, []byte("x"), 0o600)
	}
	if _, err := migrate(env); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if len(got) != 1 || got[0] != localBundle {
		t.Fatalf("reindex got %v, want [%s] (git URL must be filtered)", got, localBundle)
	}
}

// TestMigrateIndexAllURLBundlesNoError (O2): a bundle set that is ENTIRELY git
// URLs never calls reindex and never errors — it just leaves the empty new index
// dir for `serve` to build.
func TestMigrateIndexAllURLBundlesNoError(t *testing.T) {
	home := migrateTestHome(t)
	mustDir(t, filepath.Join(home, ".config", "pi-stack"), nil)
	cfgPath := filepath.Join(home, ".config", "pi-stack", "config.toml")
	body := "knowledge_bundles = [\"https://github.com/acme/kb.git\", \"git@host:owner/repo.git\"]\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	reindexCalled := false
	var out bytes.Buffer
	env := baseMigrateEnv(t, home, &out)
	env.reindex = func(paths []string) error { reindexCalled = true; return nil }
	rep, err := migrate(env)
	if err != nil {
		t.Fatalf("all-URL bundles must not error: %v", err)
	}
	if reindexCalled {
		t.Fatal("reindex must NOT be called when every bundle is a URL")
	}
	newIndexDir := filepath.Join(home, ".local", "state", "pi-stack", "knowledge")
	if fi, serr := os.Stat(newIndexDir); serr != nil || !fi.IsDir() {
		t.Fatalf("new index dir must exist for serve to build: %v", serr)
	}
	if a := artifactAction(rep, "index"); a != "left-in-place" {
		t.Fatalf("index action = %q, want left-in-place", a)
	}
}

// TestMigrateRogueSymlinkAtNewMemoryRefused (LOW-1): a pre-existing rogue symlink
// planted at the NEW memory path is REFUSED (not followed), so migration never
// writes through an attacker link.
func TestMigrateRogueSymlinkAtNewMemoryRefused(t *testing.T) {
	home := migrateTestHome(t)
	legMem := filepath.Join(home, ".pi-stack", "memory")
	seedMemDBAt(t, legMem, 2)
	newMem := filepath.Join(home, ".local", "share", "pi-stack", "memory")
	if err := os.MkdirAll(filepath.Dir(newMem), 0o755); err != nil {
		t.Fatal(err)
	}
	rogueTarget := t.TempDir()
	if err := os.Symlink(rogueTarget, newMem); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	rep, err := migrate(baseMigrateEnv(t, home, &out))
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if got := artifactAction(rep, "memory"); got != "refused-conflict" {
		t.Fatalf("memory action = %q, want refused-conflict", got)
	}
	// Legacy memory left intact as a REAL dir (never converted / followed).
	if fi, _ := os.Lstat(legMem); fi == nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("legacy memory must remain a real dir")
	}
	// The rogue symlink is left exactly as it was (not followed / overwritten),
	// and nothing was written into its target.
	if fi, _ := os.Lstat(newMem); fi == nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("rogue symlink at new memory must be left as-is")
	}
	if entries, _ := os.ReadDir(rogueTarget); len(entries) != 0 {
		t.Fatalf("nothing may be written through the rogue symlink, found %d entries", len(entries))
	}
}

// TestMigrateResumableReplantsSymlink (R1-4): a prior run moved the bundle to the
// NEW path but crashed before (or failed at) planting the legacy compatibility
// symlink — new exists + valid, legacy absent. A rerun must (re)plant the symlink
// (idempotent handoff repair) rather than read the state as "complete" and skip.
func TestMigrateResumableReplantsSymlink(t *testing.T) {
	home := migrateTestHome(t)
	newBundle := filepath.Join(home, ".local", "share", "pi-stack", "knowledge")
	mustDir(t, newBundle, map[string]string{"c.md": "x"})
	legBundle := filepath.Join(home, ".config", "pi-stack", "knowledge")
	if _, err := os.Lstat(legBundle); err == nil {
		t.Fatal("precondition: legacy bundle must be absent (symlink step 'failed')")
	}

	var out bytes.Buffer
	rep, err := migrate(baseMigrateEnv(t, home, &out))
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	symlinkResolvesTo(t, legBundle, newBundle) // symlink re-planted
	if got := artifactAction(rep, "knowledge"); got != "moved" {
		t.Fatalf("knowledge action = %q, want moved (replanted)", got)
	}
	// Idempotent: a second run changes nothing.
	var out2 bytes.Buffer
	rep2, err := migrate(baseMigrateEnv(t, home, &out2))
	if err != nil {
		t.Fatalf("migrate 2: %v", err)
	}
	if got := artifactAction(rep2, "knowledge"); got != "left-in-place" {
		t.Fatalf("knowledge action after replant = %q, want left-in-place", got)
	}
}

// TestMigrateIndexAllURLWithLegacyPublishesNew (R1-6): when every bundle is a URL
// AND a legacy index exists, migrate must PUBLISH a new (empty-but-valid) index at
// the new path so new wins — else KnowledgeIndexPath falls back to legacy forever.
func TestMigrateIndexAllURLWithLegacyPublishesNew(t *testing.T) {
	home := migrateTestHome(t)
	// A pre-existing legacy index.
	mustDir(t, filepath.Join(home, ".pi-stack", "knowledge"), map[string]string{"knowledge.db": "legacy-index"})
	mustDir(t, filepath.Join(home, ".config", "pi-stack"), nil)
	cfgPath := filepath.Join(home, ".config", "pi-stack", "config.toml")
	body := "knowledge_bundles = [\"https://github.com/acme/kb.git\", \"git@host:owner/repo.git\"]\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	called := false
	var reindexArgs []string
	var out bytes.Buffer
	env := baseMigrateEnv(t, home, &out)
	env.reindex = func(paths []string) error {
		called = true
		reindexArgs = append([]string(nil), paths...)
		idx := filepath.Join(home, ".local", "state", "pi-stack", "knowledge", "index.db")
		if err := os.MkdirAll(filepath.Dir(idx), 0o755); err != nil {
			return err
		}
		return os.WriteFile(idx, []byte("empty-valid"), 0o600)
	}
	rep, err := migrate(env)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if !called {
		t.Fatal("reindex must be called to PUBLISH a new index when a legacy index exists")
	}
	if len(reindexArgs) != 0 {
		t.Fatalf("reindex should get an EMPTY local set (URL bundles built by serve), got %v", reindexArgs)
	}
	newIdx := filepath.Join(home, ".local", "state", "pi-stack", "knowledge", "index.db")
	if _, serr := os.Stat(newIdx); serr != nil {
		t.Fatalf("new index.db must exist: %v", serr)
	}
	if got := config.KnowledgeIndexPath(); got != newIdx {
		t.Fatalf("KnowledgeIndexPath = %q, want the NEW index %q (never legacy)", got, newIdx)
	}
	if a := artifactAction(rep, "index"); a != "rebuilt" {
		t.Fatalf("index action = %q, want rebuilt", a)
	}
}

// TestMigrateRewritesAbsentCacheDescendant (R1-7): a knowledge_bundles entry under
// the OLD cache root whose leaf does NOT exist (repo not cloned) must still be
// rewritten to the new cache root, preserving its relative suffix.
func TestMigrateRewritesAbsentCacheDescendant(t *testing.T) {
	home := migrateTestHome(t)
	// A real cache repo so the cache dir actually migrates (cacheMigrated = true).
	mustDir(t, filepath.Join(home, ".config", "pi-stack", "knowledge-cache", "repo1"), map[string]string{"f": "y"})
	// The config references an ABSENT descendant (repo2 was never cloned).
	absentRepo2 := filepath.Join(home, ".config", "pi-stack", "knowledge-cache", "repo2")
	cfgPath := filepath.Join(home, ".config", "pi-stack", "config.toml")
	if err := os.WriteFile(cfgPath, []byte("knowledge_bundles = [\""+absentRepo2+"\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	rep, err := migrate(baseMigrateEnv(t, home, &out))
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if rep.bundlesRewritten != 1 {
		t.Fatalf("bundlesRewritten = %d, want 1 (absent descendant rewritten)", rep.bundlesRewritten)
	}
	newRepo2 := filepath.Join(home, ".local", "state", "pi-stack", "knowledge-cache", "repo2")
	cfg, err := config.LoadFrom(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !containsStr(cfg.KnowledgeBundles, newRepo2) {
		t.Fatalf("absent cache descendant not rewritten: %v (want %s)", cfg.KnowledgeBundles, newRepo2)
	}
}

// TestMigrateBackupsSameNameDifferentContentCopied (R1-8): two same-NAME archives
// with the SAME size but DIFFERENT content must not collapse — the legacy archive
// is copied under a collision-safe name, never dropped by a size-only check.
func TestMigrateBackupsSameNameDifferentContentCopied(t *testing.T) {
	home := migrateTestHome(t)
	legBackups := filepath.Join(home, ".pi-stack", "backups")
	mustDir(t, legBackups, map[string]string{
		"pi-stack-backup-20200101-000000.tar.gz": "AAAA", // 4 bytes
	})
	newBackups := filepath.Join(home, ".local", "share", "pi-stack", "backups")
	mustDir(t, newBackups, map[string]string{
		"pi-stack-backup-20200101-000000.tar.gz": "BBBB", // 4 bytes: SAME size, DIFFERENT content
	})

	var out bytes.Buffer
	rep, err := migrate(baseMigrateEnv(t, home, &out))
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if got := artifactAction(rep, "backups"); got != "copied" {
		t.Fatalf("backups action = %q, want copied", got)
	}
	entries, _ := os.ReadDir(newBackups)
	if len(entries) != 2 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("new backups has %v, want 2 (pre-existing + collision-safe copy)", names)
	}
	// The pre-existing new-dir archive is untouched.
	if b, _ := os.ReadFile(filepath.Join(newBackups, "pi-stack-backup-20200101-000000.tar.gz")); string(b) != "BBBB" {
		t.Fatal("the pre-existing new archive must NOT be clobbered")
	}
	// The legacy content landed under a different name.
	foundAAAA := false
	for _, e := range entries {
		if b, _ := os.ReadFile(filepath.Join(newBackups, e.Name())); string(b) == "AAAA" {
			foundAAAA = true
		}
	}
	if !foundAAAA {
		t.Fatal("the legacy archive content must be copied under a collision-safe name")
	}
}

// --- helpers -----------------------------------------------------------------

func artifactAction(rep report, name string) string {
	for _, a := range rep.artifacts {
		if a.name == name {
			return a.action
		}
	}
	return ""
}

func artifactNote(rep report, name string) string {
	for _, a := range rep.artifacts {
		if a.name == name {
			return a.note
		}
	}
	return ""
}

func artifactBySafety(rep report, name string) string {
	for _, a := range rep.artifacts {
		if a.name == name {
			return a.safety
		}
	}
	return ""
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// dirFingerprint is a cheap content signature (names + sizes) to assert a dir was
// not mutated.
func dirFingerprint(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	var sb strings.Builder
	for _, e := range entries {
		fi, _ := e.Info()
		sb.WriteString(e.Name())
		if fi != nil {
			sb.WriteString(":")
			sb.WriteString(time.Duration(fi.Size()).String())
		}
		sb.WriteString(";")
	}
	return sb.String()
}
