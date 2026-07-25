package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
)

// TestPackDir_DefaultsToDefault: the built-in pack dir basename ("pack" ->
// "personal" -> "default"), which the pack's Name derives from.
func TestPackDir_DefaultsToDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	got := config.PackDir()
	want := filepath.Join(dir, "pix", "default")
	if got != want {
		t.Errorf("PackDir() = %q, want %q", got, want)
	}
	if filepath.Base(got) != "default" {
		t.Errorf("PackDir() basename = %q, want %q", filepath.Base(got), "default")
	}
}

// TestMigrateLegacyPackDir_PersonalToDefault_RenamesAndUpdatesConfig: an
// existing mid-rename ".../personal" (with pack.toml, i.e. a real pack, and a
// .git dir to prove history survives a plain rename) is migrated to
// ".../default" on the first defaultPackRoot() resolution, a cfg.Pack pointing
// at the old path is updated + saved, and the manifest's Name field is
// rewritten to "default" while every other field is preserved.
func TestMigrateLegacyPackDir_PersonalToDefault_RenamesAndUpdatesConfig(t *testing.T) {
	data := t.TempDir()
	cfgDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("PIX_CONFIG", filepath.Join(cfgDir, "config.toml"))

	legacy := filepath.Join(data, "pix", "personal")
	if err := os.MkdirAll(filepath.Join(legacy, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "pack.toml"), []byte("name = \"personal\"\nschema = 1\nollama_bridge_model = \"llama3\"\n"), 0o644); err != nil {
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

	root := defaultPackRoot()
	want := filepath.Join(data, "pix", "default")
	if root != want {
		t.Fatalf("defaultPackRoot() = %q, want %q", root, want)
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

	p, err := loadPack(root)
	if err != nil {
		t.Fatalf("loadPack(%s): %v", root, err)
	}
	if p.Manifest.Name != "default" {
		t.Errorf("migrated manifest Name = %q, want %q", p.Manifest.Name, "default")
	}
	if p.Manifest.OllamaBridgeModel != "llama3" {
		t.Errorf("migrated manifest OllamaBridgeModel = %q, want preserved %q", p.Manifest.OllamaBridgeModel, "llama3")
	}

	reloaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Pack != root {
		t.Errorf("cfg.Pack after migration = %q, want %q", reloaded.Pack, root)
	}
}

// TestMigrateLegacyPackDir_OldPackToDefault: with no ".../personal" dir, an
// even-older ".../pack" dir is migrated straight to ".../default".
func TestMigrateLegacyPackDir_OldPackToDefault(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))

	legacy := filepath.Join(data, "pix", "pack")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "pack.toml"), []byte("name = \"pack\"\nschema = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	root := defaultPackRoot()
	want := filepath.Join(data, "pix", "default")
	if root != want {
		t.Fatalf("defaultPackRoot() = %q, want %q", root, want)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("legacy \"pack\" dir should be gone after migration, stat err = %v", err)
	}
	p, err := loadPack(root)
	if err != nil {
		t.Fatalf("loadPack(%s): %v", root, err)
	}
	if p.Manifest.Name != "default" {
		t.Errorf("migrated manifest Name = %q, want %q", p.Manifest.Name, "default")
	}
}

// TestMigrateLegacyPackDir_PreferPersonalOverPack_LeavesPackUntouched: when
// BOTH a legacy ".../personal" and an older ".../pack" exist, "personal" is
// migrated to "default" and "pack" is left completely untouched (no ambiguous
// merge, no silent deletion of the older one).
func TestMigrateLegacyPackDir_PreferPersonalOverPack_LeavesPackUntouched(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))

	base := filepath.Join(data, "pix")
	personal := filepath.Join(base, "personal")
	oldPack := filepath.Join(base, "pack")
	if err := os.MkdirAll(personal, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(personal, "pack.toml"), []byte("name = \"personal\"\nschema = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(oldPack, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldPack, "pack.toml"), []byte("name = \"pack\"\nschema = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldPack, "sentinel.txt"), []byte("do not touch"), 0o644); err != nil {
		t.Fatal(err)
	}

	root := defaultPackRoot()
	want := filepath.Join(base, "default")
	if root != want {
		t.Fatalf("defaultPackRoot() = %q, want %q", root, want)
	}
	if _, err := os.Stat(personal); !os.IsNotExist(err) {
		t.Errorf("personal dir should be gone after migration, stat err = %v", err)
	}
	// The older "pack" dir must be left exactly as it was.
	if _, err := os.Stat(filepath.Join(oldPack, "sentinel.txt")); err != nil {
		t.Errorf("old \"pack\" dir was touched by migration: %v", err)
	}
	p, err := loadPack(root)
	if err != nil {
		t.Fatalf("loadPack(%s): %v", root, err)
	}
	if p.Manifest.Name != "default" {
		t.Errorf("migrated manifest Name = %q, want %q", p.Manifest.Name, "default")
	}
}

// TestMigrateLegacyPackDir_NoLegacy_NoOp: with no legacy dir, migration is a
// clean no-op (defaultPackRoot just returns the new default).
func TestMigrateLegacyPackDir_NoLegacy_NoOp(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))

	root := defaultPackRoot()
	want := filepath.Join(data, "pix", "default")
	if root != want {
		t.Errorf("defaultPackRoot() = %q, want %q", root, want)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("no pack should have been created, stat err = %v", err)
	}
}

// TestMigrateLegacyPackDir_AlreadyDefault_Idempotent: calling defaultPackRoot
// twice (or when ".../default" already exists) is a no-op the second time —
// migration never re-fires once the new dir is in place.
func TestMigrateLegacyPackDir_AlreadyDefault_Idempotent(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))

	newDir := filepath.Join(data, "pix", "default")
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newDir, "pack.toml"), []byte("name = \"default\"\nschema = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A stray legacy dir sitting alongside it must NOT be touched once
	// ".../default" already exists.
	legacy := filepath.Join(data, "pix", "personal")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "pack.toml"), []byte("name = \"personal\"\nschema = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		root := defaultPackRoot()
		if root != newDir {
			t.Fatalf("call %d: defaultPackRoot() = %q, want %q", i, root, newDir)
		}
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Errorf("stray legacy dir should be untouched once default exists: %v", err)
	}
}

// TestMigrateLegacyPackDir_SymlinkedPersonal_Refused: a legacy ".../personal"
// that is actually a symlink must NOT be followed into the rename (the
// migration must refuse it entirely, leaving both the symlink and any
// pre-existing new dir untouched).
func TestMigrateLegacyPackDir_SymlinkedPersonal_Refused(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))

	base := filepath.Join(data, "pix")
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
	legacy := filepath.Join(base, "personal")
	if err := os.Symlink(target, legacy); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	root := defaultPackRoot()
	want := filepath.Join(data, "pix", "default")
	if root != want {
		t.Errorf("defaultPackRoot() = %q, want %q", root, want)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("symlinked legacy dir must not be migrated, but %q now exists", root)
	}
	if fi, err := os.Lstat(legacy); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("legacy symlink should be left untouched, lstat = %+v, err = %v", fi, err)
	}
}

// TestMigrateLegacyPackDir_SymlinkedOldPack_Refused: same refusal for a
// symlinked ".../pack" when there is no ".../personal" candidate at all.
func TestMigrateLegacyPackDir_SymlinkedOldPack_Refused(t *testing.T) {
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))

	base := filepath.Join(data, "pix")
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

	root := defaultPackRoot()
	want := filepath.Join(data, "pix", "default")
	if root != want {
		t.Errorf("defaultPackRoot() = %q, want %q", root, want)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("symlinked legacy dir must not be migrated, but %q now exists", root)
	}
	if fi, err := os.Lstat(legacy); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("legacy symlink should be left untouched, lstat = %+v, err = %v", fi, err)
	}
}

// TestWritePackManifest_RefusesSymlinkedManifest: writePackManifest must
// Lstat-refuse a symlinked pack.toml rather than writing through it (the pack
// root is untrusted input — an adopted/migrated pack could ship a symlinked
// manifest pointing outside the pack root).
func TestWritePackManifest_RefusesSymlinkedManifest(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere.toml")
	if err := os.WriteFile(target, []byte("do not touch"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(root, packManifestName)
	if err := os.Symlink(target, manifestPath); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	err := writePackManifest(root, packManifest{Name: "default", Schema: 1})
	if err == nil {
		t.Fatal("writePackManifest through a symlinked pack.toml should have failed")
	}
	b, rerr := os.ReadFile(target)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(b) != "do not touch" {
		t.Errorf("symlink target was written through: %q", string(b))
	}
	if fi, lerr := os.Lstat(manifestPath); lerr != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("manifest symlink should be left untouched, lstat = %+v, err = %v", fi, lerr)
	}
}

// TestWritePackManifest_AtomicRoundTrip: a normal (non-symlinked) manifest
// write round-trips every field via loadPack.
func TestWritePackManifest_AtomicRoundTrip(t *testing.T) {
	root := t.TempDir()
	m := packManifest{Name: "default", Schema: 1, OllamaBridgeModel: "llama3", GogAccount: "me@example.com"}
	if err := writePackManifest(root, m); err != nil {
		t.Fatal(err)
	}
	p, err := loadPack(root)
	if err != nil {
		t.Fatal(err)
	}
	if p.Manifest.Name != m.Name || p.Manifest.Schema != m.Schema ||
		p.Manifest.OllamaBridgeModel != m.OllamaBridgeModel || p.Manifest.GogAccount != m.GogAccount {
		t.Errorf("round-tripped manifest = %+v, want %+v", p.Manifest, m)
	}
}

// TestPackNew_DefaultRoot_ActivatesAndPrintsDefaultLabel: `pack new` on the
// resolved default root (no PATH arg) creates + auto-activates it, and the
// output/config say "default", never "personal".
func TestPackNew_DefaultRoot_ActivatesAndPrintsDefaultLabel(t *testing.T) {
	data := t.TempDir()
	cfgDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("PIX_CONFIG", filepath.Join(cfgDir, "config.toml"))

	var buf bytes.Buffer
	runPackNew(fakeGitEnv(nil), &buf, nil)

	root := filepath.Join(data, "pix", "default")
	out := buf.String()
	if !strings.Contains(out, `created pack "default"`) {
		t.Errorf("expected output to name the pack \"default\", got: %s", out)
	}
	if !strings.Contains(out, "active pack -> this (default) pack") {
		t.Errorf("expected the default-pack activation line, got: %s", out)
	}
	if strings.Contains(out, "personal") {
		t.Errorf("output should never say \"personal\", got: %s", out)
	}
	p, err := loadPack(root)
	if err != nil {
		t.Fatalf("loadPack(%s): %v", root, err)
	}
	if p.Manifest.Name != "default" {
		t.Errorf("new pack Name = %q, want %q", p.Manifest.Name, "default")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pack != root {
		t.Errorf("cfg.Pack after `pack new` = %q, want %q (auto-activated)", cfg.Pack, root)
	}
}

// TestPackUse_DefaultAlias_ResolvesToPackDir: `pack use default` resolves to
// config.PackDir(), NOT $PWD/default.
func TestPackUse_DefaultAlias_ResolvesToPackDir(t *testing.T) {
	data := t.TempDir()
	cfgDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("PIX_CONFIG", filepath.Join(cfgDir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(cfgDir, "state"))

	// A decoy "default" dir under the CWD must never be picked instead.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	decoy := filepath.Join(wd, "default")
	if _, err := os.Stat(decoy); err == nil {
		t.Fatalf("refusing to run: %s already exists on disk", decoy)
	}

	root := defaultPackRoot() // creates+migrates nothing; just resolves the path and ensures parent exists
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, packManifestName), []byte("name = \"default\"\nschema = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	runPackUse(fakeGitEnv(nil), &buf, []string{"default"})

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pack != root {
		t.Errorf("cfg.Pack after `pack use default` = %q, want %q", cfg.Pack, root)
	}
	if _, err := os.Stat(decoy); err == nil {
		t.Errorf("`pack use default` created a decoy at %s (should resolve to PackDir, not $PWD/default)", decoy)
	}
}

// TestPackUse_PersonalAlias_DeprecatedButResolvesToDefault: the bare token
// "personal" is a deprecated alias that still resolves to the default pack
// root, with a deprecation warning printed.
func TestPackUse_PersonalAlias_DeprecatedButResolvesToDefault(t *testing.T) {
	data := t.TempDir()
	cfgDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("PIX_CONFIG", filepath.Join(cfgDir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(cfgDir, "state"))

	root := defaultPackRoot()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, packManifestName), []byte("name = \"default\"\nschema = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	runPackUse(fakeGitEnv(nil), &buf, []string{"personal"})

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pack != root {
		t.Errorf("cfg.Pack after `pack use personal` = %q, want %q", cfg.Pack, root)
	}
	if !strings.Contains(buf.String(), "deprecated") {
		t.Errorf("expected a deprecation warning for the \"personal\" alias, got: %s", buf.String())
	}
}

// --- transactional migration: trust state, checked saves, rollback, repair ---

// migrationTestEnv isolates data/config/state dirs and returns the legacy
// "personal" path + the expected default root.
func migrationTestEnv(t *testing.T) (legacy, def string) {
	t.Helper()
	data := t.TempDir()
	cfgDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("PIX_CONFIG", filepath.Join(cfgDir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(cfgDir, "state"))
	return filepath.Join(data, "pix", "personal"), filepath.Join(data, "pix", "default")
}

func writeLegacyPack(t *testing.T, legacy string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(legacy, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "pack.toml"), []byte("name = \"personal\"\nschema = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestMigrateLegacyPackDir_MigratesTrustPathState: every PATH-KEYED piece of
// the trust store follows the rename (accepted key + record path, adopted key,
// installed owner, activation path/owner), remote-keyed identity stays, and
// fingerprints/provenance/wrappers/contribution sets are preserved untouched.
func TestMigrateLegacyPackDir_MigratesTrustPathState(t *testing.T) {
	legacy, def := migrationTestEnv(t)
	writeLegacyPack(t, legacy)
	oldCanon := canonicalizePackRoot(legacy)

	store := &packTrustStore{
		Version: 1,
		Accepted: map[string]packTrustRecord{
			"path:" + oldCanon:       {Path: oldCanon, Fingerprint: "fp-local"},
			"remote:https://x/p.git": {Remote: "https://x/p.git", Path: oldCanon, Commit: "abc", Fingerprint: "fp-remote"},
		},
		Adopted:   map[string]packProvenance{oldCanon: {Remote: "https://x/p.git", Commit: "abc"}},
		Installed: &packInstalledSet{Owner: "path:" + oldCanon, Wrappers: []string{"w1", "w2"}},
		Activation: &packActivationRecord{
			Owner: "path:" + oldCanon, Path: oldCanon,
			MCP: []string{gwServerName}, Knowledge: []string{"kb"}, GogAccount: "a@b.c",
		},
	}
	if err := store.save(); err != nil {
		t.Fatal(err)
	}

	root := defaultPackRoot()
	if root != def {
		t.Fatalf("defaultPackRoot() = %q, want %q", root, def)
	}
	newCanon := canonicalizePackRoot(def)

	s, err := loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Accepted["path:"+oldCanon]; ok {
		t.Error("old path-keyed acceptance must be gone")
	}
	rec, ok := s.Accepted["path:"+newCanon]
	if !ok || rec.Fingerprint != "fp-local" || rec.Path != newCanon {
		t.Errorf("migrated path acceptance = %+v (ok=%v), want Path=%q Fingerprint=fp-local", rec, ok, newCanon)
	}
	rrec, ok := s.Accepted["remote:https://x/p.git"]
	if !ok || rrec.Fingerprint != "fp-remote" || rrec.Commit != "abc" {
		t.Errorf("remote-keyed acceptance must keep its key + fingerprint + commit, got %+v (ok=%v)", rrec, ok)
	}
	if rrec.Path != newCanon {
		t.Errorf("remote-keyed record Path = %q, want refreshed %q", rrec.Path, newCanon)
	}
	if _, ok := s.Adopted[oldCanon]; ok {
		t.Error("old adopted key must be gone")
	}
	if prov, ok := s.Adopted[newCanon]; !ok || prov.Remote != "https://x/p.git" || prov.Commit != "abc" {
		t.Errorf("adopted provenance = %+v (ok=%v), want preserved under new key", prov, ok)
	}
	if s.Installed == nil || s.Installed.Owner != "path:"+newCanon {
		t.Errorf("installed owner = %+v, want path:%s", s.Installed, newCanon)
	}
	if s.Installed != nil && (len(s.Installed.Wrappers) != 2 || s.Installed.Wrappers[0] != "w1") {
		t.Errorf("wrappers must be preserved, got %+v", s.Installed)
	}
	a := s.Activation
	if a == nil || a.Owner != "path:"+newCanon || a.Path != newCanon {
		t.Fatalf("activation = %+v, want owner/path migrated", a)
	}
	if len(a.MCP) != 1 || a.MCP[0] != gwServerName || len(a.Knowledge) != 1 || a.GogAccount != "a@b.c" {
		t.Errorf("activation contribution set must be preserved, got %+v", a)
	}
}

// TestMigrateLegacyPackDir_ConfigSaveFailure_RollsBackEverything: a cfg.Save
// failure rolls back the trust store, the manifest name, and the directory
// rename; defaultPackRoot fails CLOSED to the legacy path so no caller can
// create a second empty pack at the default root.
func TestMigrateLegacyPackDir_ConfigSaveFailure_RollsBackEverything(t *testing.T) {
	legacy, def := migrationTestEnv(t)
	writeLegacyPack(t, legacy)
	oldCanon := canonicalizePackRoot(legacy)

	store := &packTrustStore{
		Version:  1,
		Accepted: map[string]packTrustRecord{"path:" + oldCanon: {Path: oldCanon, Fingerprint: "fp"}},
	}
	if err := store.save(); err != nil {
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

	origSave := savePackMigrationConfig
	savePackMigrationConfig = func(*config.Config) error { return fmt.Errorf("disk full") }
	t.Cleanup(func() { savePackMigrationConfig = origSave })

	root := defaultPackRoot()
	if root != legacy {
		t.Errorf("defaultPackRoot() after failed migration = %q, want fail-closed legacy %q", root, legacy)
	}
	if _, err := os.Stat(def); !os.IsNotExist(err) {
		t.Errorf("default dir must not exist after rollback, stat err = %v", err)
	}
	p, err := loadPack(legacy)
	if err != nil {
		t.Fatalf("legacy pack must still load: %v", err)
	}
	if p.Manifest.Name != "personal" {
		t.Errorf("manifest name after rollback = %q, want %q", p.Manifest.Name, "personal")
	}
	s, err := loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	if rec, ok := s.Accepted["path:"+oldCanon]; !ok || rec.Fingerprint != "fp" {
		t.Errorf("trust store must be rolled back to the old key, got %+v", s.Accepted)
	}
	reloaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Pack != legacy {
		t.Errorf("cfg.Pack after rollback = %q, want unchanged %q", reloaded.Pack, legacy)
	}
}

// TestRepairStaleLegacyPackState: the default dir already exists (an earlier,
// non-transactional migration renamed it) but cfg.Pack and the trust store
// still reference the GONE legacy path — a later defaultPackRoot() resolution
// repairs both, idempotently.
func TestRepairStaleLegacyPackState(t *testing.T) {
	legacy, def := migrationTestEnv(t)
	if err := os.MkdirAll(def, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(def, "pack.toml"), []byte("name = \"default\"\nschema = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldCanon := canonicalizePackRoot(legacy)

	store := &packTrustStore{
		Version:    1,
		Accepted:   map[string]packTrustRecord{"path:" + oldCanon: {Path: oldCanon, Fingerprint: "fp"}},
		Activation: &packActivationRecord{Owner: "path:" + oldCanon, Path: oldCanon, MCP: []string{gwServerName}},
	}
	if err := store.save(); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Pack = legacy // stale: the dir no longer exists
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ { // idempotent
		if root := defaultPackRoot(); root != def {
			t.Fatalf("call %d: defaultPackRoot() = %q, want %q", i, root, def)
		}
	}
	newCanon := canonicalizePackRoot(def)
	reloaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Pack != def {
		t.Errorf("stale cfg.Pack must be repaired to %q, got %q", def, reloaded.Pack)
	}
	s, err := loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	if rec, ok := s.Accepted["path:"+newCanon]; !ok || rec.Fingerprint != "fp" || rec.Path != newCanon {
		t.Errorf("stale trust acceptance must be migrated, got %+v", s.Accepted)
	}
	if s.Activation == nil || s.Activation.Path != newCanon || s.Activation.Owner != "path:"+newCanon {
		t.Errorf("stale activation must be migrated, got %+v", s.Activation)
	}
	if s.Activation != nil && (len(s.Activation.MCP) != 1 || s.Activation.MCP[0] != gwServerName) {
		t.Errorf("activation contribution must be preserved, got %+v", s.Activation)
	}
}

// TestRepairStaleLegacyPackState_LiveLegacyNeverHijacked: when the legacy dir
// STILL EXISTS beside an existing default, a cfg.Pack pointing at it is a
// real, live pack — repair must not touch it.
func TestRepairStaleLegacyPackState_LiveLegacyNeverHijacked(t *testing.T) {
	legacy, def := migrationTestEnv(t)
	writeLegacyPack(t, legacy)
	if err := os.MkdirAll(def, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(def, "pack.toml"), []byte("name = \"default\"\nschema = 1\n"), 0o644); err != nil {
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

	if root := defaultPackRoot(); root != def {
		t.Fatalf("defaultPackRoot() = %q, want %q", root, def)
	}
	reloaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Pack != legacy {
		t.Errorf("cfg.Pack pointing at a LIVE legacy pack must be left alone, got %q", reloaded.Pack)
	}
	if _, err := os.Stat(filepath.Join(legacy, "pack.toml")); err != nil {
		t.Errorf("live legacy pack must be untouched: %v", err)
	}
}

// --- item 6: activateDefaultPack returns errors; never claims active on a
// cfg.Save failure; never overrides an explicit alternate; setup propagates ---

// activateDefaultPack must return a real error (not swallow it) when cfg.Save
// fails, and cfg.Pack must remain untouched on disk — never claim activation
// happened when it didn't.
func TestActivateDefaultPack_ConfigSaveFailure_NeverClaimsActive(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory write permissions")
	}
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "cfg")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIX_CONFIG", filepath.Join(cfgDir, "config.toml"))
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)

	root := filepath.Join(data, "pix", "default")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writePackManifest(root, packManifest{Name: "default", Schema: 1}); err != nil {
		t.Fatal(err)
	}

	// Config must already exist on disk (so the atomic temp-file create inside
	// cfg.Save, not the MkdirAll, is what fails).
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(cfgDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cfgDir, 0o755) })

	if err := activateDefaultPack(root); err == nil {
		t.Fatal("expected activateDefaultPack to fail when cfg.Save fails")
	}

	if err := os.Chmod(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	reloaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Pack != "" {
		t.Errorf("cfg.Pack must remain empty after a failed activation, got %q", reloaded.Pack)
	}
}

// activateDefaultPack must never override an explicitly active ALTERNATE
// pack — a no-op (nil error), leaving cfg.Pack exactly as it was.
func TestActivateDefaultPack_DoesNotOverrideExplicitAlternate(t *testing.T) {
	data := t.TempDir()
	cfgDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("PIX_CONFIG", filepath.Join(cfgDir, "config.toml"))

	root := filepath.Join(data, "pix", "default")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writePackManifest(root, packManifest{Name: "default", Schema: 1}); err != nil {
		t.Fatal(err)
	}
	alternate := filepath.Join(data, "alt-pack")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Pack = alternate
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	if err := activateDefaultPack(root); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	reloaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Pack != alternate {
		t.Errorf("an explicitly active alternate pack must not be overridden, got %q, want %q", reloaded.Pack, alternate)
	}
}

// setupHostPhase must activate an ALREADY-EXISTING default pack (e.g. one
// landed by the legacy migration, or discovered from a prior run) when
// cfg.Pack is empty — not only a brand-new one created via runPackNew.
func TestSetupHostPhase_ActivatesExistingMigratedDefaultPack_WhenCfgPackEmpty(t *testing.T) {
	data := t.TempDir()
	cfgDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("PIX_CONFIG", filepath.Join(cfgDir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(cfgDir, "state"))

	// Simulate a default pack that already exists (as if migrated or created by
	// an earlier run) but whose activation never landed: cfg.Pack is empty.
	root := filepath.Join(data, "pix", "default")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writePackManifest(root, packManifest{Name: "default", Schema: 1}); err != nil {
		t.Fatal(err)
	}

	refs := "ANTHROPIC_API_KEY=op://v/anthropic/key\nOPENAI_API_KEY=op://v/openai/key\nGEMINI_API_KEY=op://v/gemini/key\n"
	env, _ := stepEnv(t, refs, "anthropic openai google", "sk-val")
	// stepEnv points PIX_CONFIG/XDG_STATE_HOME/XDG_DATA_HOME at ITS OWN
	// temp dirs (overriding what we set above); redirect them back to cfgDir /
	// data so the real config.Load/Save AND the pre-created default pack this
	// test asserts on both land where we expect.
	t.Setenv("PIX_CONFIG", filepath.Join(cfgDir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(cfgDir, "state"))
	t.Setenv("XDG_DATA_HOME", data)
	for envVar, ref := range map[string]string{
		"ANTHROPIC_API_KEY": "op://v/anthropic/key",
		"OPENAI_API_KEY":    "op://v/openai/key",
		"GEMINI_API_KEY":    "op://v/gemini/key",
	} {
		if err := recordSyncedRefWithDigest(envVar, ref, secretDigestHex("sk-val")); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := setupHostPhase(env, []string{"--yes"}, strings.NewReader(""), &out, false); err != nil {
		t.Fatalf("unexpected error: %v\n%s", err, out.String())
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pack != root {
		t.Errorf("cfg.Pack after setup = %q, want the existing default pack %q activated", cfg.Pack, root)
	}
}

// setupHostPhase must FAIL (propagate the error) when activating the default
// pack fails (cfg.Save error) — it must never report success while cfg.Pack
// still points nowhere.
func TestSetupHostPhase_PackActivationFailure_FailsSetup(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory write permissions")
	}
	data := t.TempDir()
	cfgDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("PIX_CONFIG", filepath.Join(cfgDir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(cfgDir, "state"))

	root := filepath.Join(data, "pix", "default")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writePackManifest(root, packManifest{Name: "default", Schema: 1}); err != nil {
		t.Fatal(err)
	}

	refs := "ANTHROPIC_API_KEY=op://v/anthropic/key\nOPENAI_API_KEY=op://v/openai/key\nGEMINI_API_KEY=op://v/gemini/key\n"
	env, _ := stepEnv(t, refs, "anthropic openai google", "sk-val")
	// stepEnv points PIX_CONFIG/XDG_STATE_HOME/XDG_DATA_HOME at ITS OWN
	// temp dirs (overriding what we set above); redirect them back to cfgDir /
	// data so we chmod the SAME directory config.Save() actually writes into
	// and the pre-created pack above is the one setupHostPhase resolves.
	t.Setenv("PIX_CONFIG", filepath.Join(cfgDir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(cfgDir, "state"))
	t.Setenv("XDG_DATA_HOME", data)
	for envVar, ref := range map[string]string{
		"ANTHROPIC_API_KEY": "op://v/anthropic/key",
		"OPENAI_API_KEY":    "op://v/openai/key",
		"GEMINI_API_KEY":    "op://v/gemini/key",
	} {
		if err := recordSyncedRefWithDigest(envVar, ref, secretDigestHex("sk-val")); err != nil {
			t.Fatal(err)
		}
	}
	// Config must exist on disk before it becomes unwritable, so setupHostPhase's
	// own earlier config.Load()/writes have already succeeded and only the pack
	// activation's cfg.Save is what fails.
	if cfg, err := config.Load(); err != nil {
		t.Fatal(err)
	} else if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfgDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cfgDir, 0o755) })

	var out bytes.Buffer
	err := setupHostPhase(env, []string{"--yes"}, strings.NewReader(""), &out, false)
	_ = os.Chmod(cfgDir, 0o755) // restore before any later cleanup/log reads
	if err == nil {
		t.Fatal("setup must fail when default-pack activation fails")
	}
}

// --- item 7: a cfg.Pack SYMLINK ALIAS resolving onto the legacy dir must
// refuse migration outright, before touching anything ---------------------

// TestMigrateLegacyPackDir_CfgPackSymlinkAlias_RefusesBeforeAnyMutation:
// cfg.Pack points at a DIFFERENT path (an alias) that is itself a symlink
// resolving onto the legacy pack dir being migrated. canonicalizePackRoot
// (Abs+Clean only, never EvalSymlinks) would never string-match the alias
// against the legacy dir's own canonical path, so the existing step-4
// cfg.Pack==oldDir check would miss it — proceeding would rename the legacy
// dir out from under the alias (leaving it dangling) with no trust-store
// migration for the alias-keyed state either. The fix must catch this BEFORE
// the directory rename, the manifest edit, the trust-store save, or the
// config save — i.e. NO mutation at all — and defaultPackRoot must fall back
// to the (untouched) legacy path.
func TestMigrateLegacyPackDir_CfgPackSymlinkAlias_RefusesBeforeAnyMutation(t *testing.T) {
	legacy, def := migrationTestEnv(t)
	writeLegacyPack(t, legacy)

	// alias is a SEPARATE path (not `legacy` itself) that symlinks onto it —
	// e.g. a user-created shortcut, or another config pointing at the same
	// pack by a different name.
	alias := legacy + "-alias"
	if err := os.Symlink(legacy, alias); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Pack = alias
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	trustKey := "path:" + canonicalizePackRoot(alias)
	store := &packTrustStore{
		Version:  1,
		Accepted: map[string]packTrustRecord{trustKey: {Path: canonicalizePackRoot(alias), Fingerprint: "fp-alias"}},
	}
	if err := store.save(); err != nil {
		t.Fatal(err)
	}

	root := defaultPackRoot()
	if root != legacy {
		t.Errorf("defaultPackRoot() with an aliased cfg.Pack must fall back to the untouched legacy dir %q, got %q", legacy, root)
	}
	// NO mutation: new dir must not exist, legacy dir/manifest untouched.
	if _, err := os.Stat(def); !os.IsNotExist(err) {
		t.Errorf("the default dir must NOT be created when migration is refused, stat err = %v", err)
	}
	p, err := loadPack(legacy)
	if err != nil {
		t.Fatalf("legacy pack must still load untouched: %v", err)
	}
	if p.Manifest.Name != "personal" {
		t.Errorf("legacy manifest must be untouched, got name = %q", p.Manifest.Name)
	}
	// cfg.Pack (the alias) must be untouched.
	reloaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Pack != alias {
		t.Errorf("cfg.Pack (the alias) must be untouched, got %q, want %q", reloaded.Pack, alias)
	}
	// The alias's own trust record must be untouched (never migrated to a
	// oldCanon/newCanon key it never had in the first place).
	s, err := loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	if rec, ok := s.Accepted[trustKey]; !ok || rec.Fingerprint != "fp-alias" {
		t.Errorf("alias trust record must be untouched, got %+v", s.Accepted)
	}
	if _, ok := s.Accepted["path:"+canonicalizePackRoot(legacy)]; ok {
		t.Error("no NEW trust record should be created for the legacy path")
	}
}

// --- item 8: repairStaleLegacyPackState is transactional (trust+config) ---

// TestRepairStaleLegacyPackState_ConfigSaveFailure_RollsBackTrust: when the
// trust-store repair succeeds but the PAIRED cfg.Pack repair then fails to
// save, the already-persisted trust-store change must be ROLLED BACK — never
// left half-migrated (trust repaired to the new path while cfg.Pack still
// points at the stale one, or vice versa). A later retry (once saving works
// again) must repair BOTH.
func TestRepairStaleLegacyPackState_ConfigSaveFailure_RollsBackTrust(t *testing.T) {
	legacy, def := migrationTestEnv(t)
	if err := os.MkdirAll(def, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(def, "pack.toml"), []byte("name = \"default\"\nschema = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldCanon := canonicalizePackRoot(legacy)

	store := &packTrustStore{
		Version:  1,
		Accepted: map[string]packTrustRecord{"path:" + oldCanon: {Path: oldCanon, Fingerprint: "fp"}},
	}
	if err := store.save(); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Pack = legacy // stale: the dir no longer exists
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	origSave := savePackMigrationConfig
	savePackMigrationConfig = func(*config.Config) error { return fmt.Errorf("disk full") }
	t.Cleanup(func() { savePackMigrationConfig = origSave })

	if root := defaultPackRoot(); root != def {
		t.Fatalf("defaultPackRoot() = %q, want %q even when the repair partially fails", root, def)
	}

	newCanon := canonicalizePackRoot(def)
	s, err := loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Accepted["path:"+newCanon]; ok {
		t.Errorf("trust-store repair must be ROLLED BACK when the paired config save fails, got migrated key present: %+v", s.Accepted)
	}
	if rec, ok := s.Accepted["path:"+oldCanon]; !ok || rec.Fingerprint != "fp" {
		t.Errorf("trust store must be restored to its ORIGINAL (stale) state, got %+v", s.Accepted)
	}
	reloaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Pack != legacy {
		t.Errorf("cfg.Pack must remain untouched (still stale) when the repair fails, got %q, want %q", reloaded.Pack, legacy)
	}

	// Retry once saving works again: BOTH must now repair.
	savePackMigrationConfig = origSave
	if root := defaultPackRoot(); root != def {
		t.Fatalf("retry: defaultPackRoot() = %q, want %q", root, def)
	}
	s2, err := loadPackTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	if rec, ok := s2.Accepted["path:"+newCanon]; !ok || rec.Fingerprint != "fp" {
		t.Errorf("retry: trust store must be repaired to the new key, got %+v", s2.Accepted)
	}
	reloaded2, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded2.Pack != def {
		t.Errorf("retry: cfg.Pack must be repaired to %q, got %q", def, reloaded2.Pack)
	}
}
