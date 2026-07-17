package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// legacyModes builds a fakeEnv modes map (path -> mode) marking the given legacy
// paths as existing dirs, so detectLegacyStorage/fileMode report them.
func legacyModes(paths ...string) map[string]os.FileMode {
	m := map[string]os.FileMode{}
	for _, p := range paths {
		m[p] = os.ModeDir | 0o755
	}
	return m
}

// TestDetectLegacyStorage: only the seeded legacy locations are reported.
func TestDetectLegacyStorage(t *testing.T) {
	home := "/home/fake"
	legMem := filepath.Join(home, ".pi-stack", "memory")
	legBundle := filepath.Join(home, ".config", "pi-stack", "knowledge")
	env := fakeEnv{home: home, modes: legacyModes(legMem, legBundle)}.env()

	got := detectLegacyStorage(env, home)
	if len(got) != 2 {
		t.Fatalf("detectLegacyStorage = %v, want the 2 seeded paths", got)
	}
	// A clean home reports nothing.
	clean := fakeEnv{home: home, modes: map[string]os.FileMode{}}.env()
	if g := detectLegacyStorage(clean, home); len(g) != 0 {
		t.Errorf("clean home legacy = %v, want none", g)
	}
}

// TestDetectLegacyStorage_SymlinkNotPending (R2-3): a legacy memory/bundle/cache
// path migration converted into a symlink→NEW is converged and must NOT be
// reported pending; only a REAL legacy dir is. Convergence now requires the
// symlink to actually RESOLVE to the expected new dir (readlink + compare), not
// merely be a symlink.
func TestDetectLegacyStorage_SymlinkNotPending(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	legMem := filepath.Join(home, ".pi-stack", "memory") // converged -> symlink→new
	newMem := filepath.Join(home, ".local", "share", "pi-stack", "memory")
	legBundle := filepath.Join(home, ".config", "pi-stack", "knowledge") // real dir: pending
	env := fakeEnv{home: home, modes: map[string]os.FileMode{
		legMem:    os.ModeSymlink | 0o777,
		newMem:    os.ModeDir | 0o755, // the symlink target EXISTS -> converged
		legBundle: os.ModeDir | 0o755,
	}, links: map[string]string{legMem: newMem}}.env()

	got := detectLegacyStorage(env, home)
	if len(got) != 1 || got[0] != legBundle {
		t.Fatalf("detectLegacyStorage = %v, want only the real legacy bundle dir %q", got, legBundle)
	}
}

// TestDetectLegacyStorage_DanglingSymlinkPending (R6-1): a legacy path that is a
// symlink pointing at the expected new dir whose target no longer EXISTS (the
// user deleted new) is a broken handoff — NOT converged. It must be surfaced, not
// silently suppressed on the strength of the textual target alone.
func TestDetectLegacyStorage_DanglingSymlinkPending(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	legMem := filepath.Join(home, ".pi-stack", "memory")
	newMem := filepath.Join(home, ".local", "share", "pi-stack", "memory")
	// legMem -> newMem, but newMem is NOT seeded in modes: the target is missing.
	env := fakeEnv{home: home, modes: map[string]os.FileMode{
		legMem: os.ModeSymlink | 0o777,
	}, links: map[string]string{legMem: newMem}}.env()

	if got := detectLegacyStorage(env, home); len(got) != 1 || got[0] != legMem {
		t.Fatalf("detectLegacyStorage = %v, want the dangling legacy memory symlink surfaced", got)
	}
}

// TestDetectLegacyStorage_WrongTargetSymlinkNotConverged (R2-3): a legacy symlink
// that points somewhere OTHER than the expected new dir is NOT converged — it must
// be surfaced (pending), never silently suppressed as "already migrated".
func TestDetectLegacyStorage_WrongTargetSymlinkNotConverged(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	legMem := filepath.Join(home, ".pi-stack", "memory")
	env := fakeEnv{home: home, modes: map[string]os.FileMode{
		legMem: os.ModeSymlink | 0o777,
	}, links: map[string]string{legMem: filepath.Join(home, "somewhere", "else")}}.env()

	if got := detectLegacyStorage(env, home); len(got) != 1 || got[0] != legMem {
		t.Fatalf("detectLegacyStorage = %v, want the wrong-target legacy memory symlink surfaced", got)
	}
}

// TestDetectLegacyStorage_RogueSymlinkAtNewNotConverged (R7-2): a legacy path that
// is a symlink->NEW, but where the NEW path is itself a ROGUE symlink (not a real
// directory), is NOT converged. migrate classifies a symlink at the new path as a
// conflict, so detection must agree via the shared converged() semantics (new must
// be a REAL dir) and surface the legacy path rather than false-suppress it on the
// strength of "something exists at new".
func TestDetectLegacyStorage_RogueSymlinkAtNewNotConverged(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	legMem := filepath.Join(home, ".pi-stack", "memory")
	newMem := filepath.Join(home, ".local", "share", "pi-stack", "memory")
	env := fakeEnv{home: home, modes: map[string]os.FileMode{
		legMem: os.ModeSymlink | 0o777,
		newMem: os.ModeSymlink | 0o777, // rogue symlink at the NEW path: NOT a real dir
	}, links: map[string]string{legMem: newMem}}.env()

	if got := detectLegacyStorage(env, home); len(got) != 1 || got[0] != legMem {
		t.Fatalf("detectLegacyStorage = %v, want the rogue-new-symlink legacy path surfaced %q", got, legMem)
	}
}

// TestDetectLegacyStorage_IndexPidBackupsExcluded (R2-2/R2-3): the legacy
// knowledge INDEX dir (~/.pi-stack/knowledge, rebuilt not moved), the legacy
// serve.pid (ephemeral), and the retained legacy backups (merge-copied, legacy
// kept by design) are all left by a successful migrate, so NONE of them counts as
// pending — otherwise `pi-stack migrate` would be advised forever.
func TestDetectLegacyStorage_IndexPidBackupsExcluded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	legIndex := filepath.Join(home, ".pi-stack", "knowledge")
	legBackups := filepath.Join(home, ".pi-stack", "backups")
	legPid := filepath.Join(home, ".config", "pi-stack", "serve.pid")
	env := fakeEnv{home: home, modes: map[string]os.FileMode{
		legIndex:   os.ModeDir | 0o755,
		legBackups: os.ModeDir | 0o755,
		legPid:     0o644,
	}}.env()

	if got := detectLegacyStorage(env, home); len(got) != 0 {
		t.Fatalf("detectLegacyStorage = %v, want none (index/serve.pid/backups are not pending)", got)
	}
}

// TestStorageGroup_PendingMigrationIsInfoNotTodo: the doctor storage group names
// the three bases and emits a pending-migration line as INFO (never a TODO, so it
// can't fail the verdict).
func TestStorageGroup_PendingMigrationIsInfoNotTodo(t *testing.T) {
	home := "/home/fake"
	legMem := filepath.Join(home, ".pi-stack", "memory")
	env := fakeEnv{home: home, modes: legacyModes(legMem)}.env()

	g := storageGroup(env)
	var haveConfig, haveData, haveState, haveMigration bool
	for _, c := range g.checks {
		switch c.label {
		case "config":
			haveConfig = true
		case "data":
			haveData = true
		case "state":
			haveState = true
		case "migration":
			haveMigration = true
			if c.state == stateTODO {
				t.Error("pending-migration must be INFO/WARN, never a TODO (never fails the verdict)")
			}
			if !strings.Contains(c.detail, "pi-stack migrate") {
				t.Errorf("migration detail missing the command: %q", c.detail)
			}
		}
	}
	if !haveConfig || !haveData || !haveState {
		t.Error("storage group must name all three XDG bases")
	}
	if !haveMigration {
		t.Error("a legacy location present must add the migration WARN")
	}

	// No legacy => no migration line, no TODOs added.
	clean := fakeEnv{home: home, modes: map[string]os.FileMode{}}.env()
	for _, c := range storageGroup(clean).checks {
		if c.label == "migration" {
			t.Error("a clean install must not show the migration line")
		}
	}
}

// TestStatusRender_LegacyStorageLine: the status report renders a pending-migration
// advisory line when legacy storage exists, and it is NOT counted as an
// outstanding TODO (the verdict stays "all systems go").
func TestStatusRender_LegacyStorageLine(t *testing.T) {
	st := statusReport{
		Version:       "test",
		ConfigPath:    "/c/config.toml",
		Profile:       "default",
		Providers:     map[string]bool{},
		LegacyStorage: []string{"/h/.pi-stack/memory"},
	}
	var buf bytes.Buffer
	st.render(&buf)
	out := buf.String()
	if !strings.Contains(out, "pi-stack migrate") {
		t.Errorf("status must show the migrate advisory, got:\n%s", out)
	}
	if !strings.Contains(out, "all systems go") {
		t.Errorf("a pending migration is advisory, not an outstanding item; got:\n%s", out)
	}
}
