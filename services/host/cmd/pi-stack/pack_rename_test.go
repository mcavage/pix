package main

import (
	"os"
	"path/filepath"
	"testing"

	"pi-stack/host/config"
)

// TestPackDir_DefaultsToPersonal: the personal-pack rename ("pack" ->
// "personal") — the default dir basename, which the pack's name derives from.
func TestPackDir_DefaultsToPersonal(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	got := config.PackDir()
	want := filepath.Join(dir, "pi-stack", "personal")
	if got != want {
		t.Errorf("PackDir() = %q, want %q", got, want)
	}
	if filepath.Base(got) != "personal" {
		t.Errorf("PackDir() basename = %q, want %q", filepath.Base(got), "personal")
	}
}

// TestMigrateLegacyPackDir_RenamesAndUpdatesConfig: an existing legacy
// ".../pack" (with pack.toml, i.e. a real pack, and a .git dir to prove
// history survives a plain rename) is migrated to ".../personal" on the
// first personalPackRoot() resolution, and a cfg.Pack pointing at the old
// path is updated + saved.
func TestMigrateLegacyPackDir_RenamesAndUpdatesConfig(t *testing.T) {
	data := t.TempDir()
	cfgDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("PI_STACK_CONFIG", filepath.Join(cfgDir, "config.toml"))

	legacy := filepath.Join(data, "pi-stack", "pack")
	if err := os.MkdirAll(filepath.Join(legacy, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "pack.toml"), []byte("name = \"pack\"\nschema = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "marker.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Pack = legacy
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	root := personalPackRoot()
	want := filepath.Join(data, "pi-stack", "personal")
	if root != want {
		t.Fatalf("personalPackRoot() = %q, want %q", root, want)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("legacy dir should be gone after migration, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		t.Errorf(".git not preserved by migration: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "marker.txt")); err != nil {
		t.Errorf("marker.txt not preserved by migration: %v", err)
	}

	reloaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Pack != root {
		t.Errorf("cfg.Pack after migration = %q, want %q", reloaded.Pack, root)
	}
}

// TestMigrateLegacyPackDir_NoLegacy_NoOp: with no legacy dir, migration is a
// clean no-op (personalPackRoot just returns the new default).
func TestMigrateLegacyPackDir_NoLegacy_NoOp(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("PI_STACK_CONFIG", filepath.Join(t.TempDir(), "config.toml"))

	root := personalPackRoot()
	want := filepath.Join(data, "pi-stack", "personal")
	if root != want {
		t.Errorf("personalPackRoot() = %q, want %q", root, want)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("no pack should have been created, stat err = %v", err)
	}
}

// TestMigrateLegacyPackDir_SymlinkedLegacy_Refused: a legacy dir that is
// actually a symlink must NOT be followed into the rename (the migration must
// refuse it entirely, leaving both the symlink and any pre-existing new dir
// untouched).
func TestMigrateLegacyPackDir_SymlinkedLegacy_Refused(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("PI_STACK_CONFIG", filepath.Join(t.TempDir(), "config.toml"))

	base := filepath.Join(data, "pi-stack")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(data, "elsewhere")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "pack.toml"), []byte("name=\"x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(base, "pack")
	if err := os.Symlink(target, legacy); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	root := personalPackRoot()
	want := filepath.Join(data, "pi-stack", "personal")
	if root != want {
		t.Errorf("personalPackRoot() = %q, want %q", root, want)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("symlinked legacy dir must not be migrated, but %q now exists", root)
	}
	if fi, err := os.Lstat(legacy); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("legacy symlink should be left untouched, lstat = %+v, err = %v", fi, err)
	}
}
