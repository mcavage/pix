package pack

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

// TestWritePackManifest_RefusesSymlinkedManifest: WriteManifest must
// Lstat-refuse a symlinked pack.toml rather than writing through it (the pack
// root is untrusted input — an adopted/migrated pack could ship a symlinked
// manifest pointing outside the pack root).
func TestWritePackManifest_RefusesSymlinkedManifest(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere.toml")
	if err := os.WriteFile(target, []byte("do not touch"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(root, PackManifestName)
	if err := os.Symlink(target, manifestPath); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	err := WriteManifest(root, Manifest{Name: "default", Schema: 1})
	if err == nil {
		t.Fatal("WriteManifest through a symlinked pack.toml should have failed")
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
// write round-trips every field via LoadPack.
func TestWritePackManifest_AtomicRoundTrip(t *testing.T) {
	root := t.TempDir()
	m := Manifest{Name: "default", Schema: 1, OllamaBridgeModel: "llama3", GogAccount: "me@example.com"}
	if err := WriteManifest(root, m); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPack(root)
	if err != nil {
		t.Fatal(err)
	}
	if p.Manifest.Name != m.Name || p.Manifest.Schema != m.Schema ||
		p.Manifest.OllamaBridgeModel != m.OllamaBridgeModel || p.Manifest.GogAccount != m.GogAccount {
		t.Errorf("round-tripped manifest = %+v, want %+v", p.Manifest, m)
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

	root := DefaultPackRoot() // creates+migrates nothing; just resolves the path and ensures parent exists
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, PackManifestName), []byte("name = \"default\"\nschema = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	RunPackUse(fakeGitEnv(nil), &buf, []string{"default"}, registerOK)

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

	root := DefaultPackRoot()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, PackManifestName), []byte("name = \"default\"\nschema = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	RunPackUse(fakeGitEnv(nil), &buf, []string{"personal"}, registerOK)

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

// --- no legacy-path discovery: the pre-public "pack"/"personal" dir names are
// not probed, renamed, or repaired ----------------------------------------

// TestDefaultPackRoot_LeavesLegacyDirsAlone: the 0.1.0 rename was a pre-launch
// cutover with no legacy-path discovery, so a directory named "personal" or
// "pack" sitting in the pix data dir is just a directory: DefaultPackRoot
// resolves the "default" root, renames nothing, and never rewrites cfg.Pack.
// (Only the BARE `pack use personal` token remains a deprecated alias — see
// TestPackUse_PersonalAlias_DeprecatedButResolvesToDefault.)
func TestDefaultPackRoot_LeavesLegacyDirsAlone(t *testing.T) {
	data := t.TempDir()
	cfgDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("PIX_CONFIG", filepath.Join(cfgDir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(cfgDir, "state"))

	for _, name := range []string{"personal", "pack"} {
		dir := filepath.Join(data, "pix", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf("name = %q\nschema = 1\n", name)
		if err := os.WriteFile(filepath.Join(dir, PackManifestName), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Pack = filepath.Join(data, "pix", "personal")
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	got := DefaultPackRoot()
	if want := filepath.Join(data, "pix", "default"); got != want {
		t.Errorf("DefaultPackRoot() = %q, want %q", got, want)
	}
	if _, err := os.Stat(got); !os.IsNotExist(err) {
		t.Errorf("DefaultPackRoot() must not create or migrate anything at %s (stat err = %v)", got, err)
	}
	for _, name := range []string{"personal", "pack"} {
		dir := filepath.Join(data, "pix", name)
		b, rerr := os.ReadFile(filepath.Join(dir, PackManifestName))
		if rerr != nil {
			t.Fatalf("legacy-named dir %s was moved or rewritten: %v", dir, rerr)
		}
		if !strings.Contains(string(b), fmt.Sprintf("name = %q", name)) {
			t.Errorf("manifest of %s was rewritten: %s", dir, string(b))
		}
	}
	reloaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Pack != filepath.Join(data, "pix", "personal") {
		t.Errorf("cfg.Pack was rewritten to %q; resolution must never repoint it", reloaded.Pack)
	}
}
