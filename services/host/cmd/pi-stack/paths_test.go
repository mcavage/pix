package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// statSet returns an os.Lstat-shaped func that reports a REAL DIR for each seeded
// path, so buildPathsReport's legacy probe is hermetic. buildPathsReport now
// inspects fi.Mode(), so the fake returns a directory mode.
func statSet(exist map[string]bool) func(string) (os.FileInfo, error) {
	return lstatSet(func() map[string]os.FileMode {
		m := map[string]os.FileMode{}
		for p, ok := range exist {
			if ok {
				m[p] = os.ModeDir | 0o755
			}
		}
		return m
	}())
}

// lstatSet returns an os.Lstat-shaped func from an explicit path->mode map, so a
// test can seed a SYMLINK (converged) vs a real dir (pending).
func lstatSet(modes map[string]os.FileMode) func(string) (os.FileInfo, error) {
	return func(p string) (os.FileInfo, error) {
		if m, ok := modes[p]; ok {
			return fakeFileInfo{mode: m}, nil
		}
		return nil, os.ErrNotExist
	}
}

type fakeFileInfo struct {
	os.FileInfo
	mode os.FileMode
}

func (f fakeFileInfo) Mode() os.FileMode { return f.mode }
func (f fakeFileInfo) IsDir() bool       { return f.mode&os.ModeDir != 0 }

// linkSet returns an os.Readlink-shaped func from a path->target map, so a test
// can seed a converged (symlink→new) legacy path. nilReadlink is the "no symlinks
// seeded" default (every readlink misses).
func linkSet(links map[string]string) func(string) (string, error) {
	return func(p string) (string, error) {
		if t, ok := links[p]; ok {
			return t, nil
		}
		return "", os.ErrNotExist
	}
}

func nilReadlink(string) (string, error) { return "", os.ErrNotExist }

// TestBuildPathsReport_XDGBasesAndLegacy: the report resolves the three XDG bases
// from the env, surfaces set overrides, and lists exactly the legacy locations
// that exist.
func TestBuildPathsReport_XDGBasesAndLegacy(t *testing.T) {
	home := t.TempDir()
	cfgHome := filepath.Join(home, "cfg")
	dataHome := filepath.Join(home, "data")
	stateHome := filepath.Join(home, "state")

	// The bases resolve through config.*, which read the process env, so drive them
	// via t.Setenv; overrides are read through the injected getenv.
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("MEMORY_DB", "/custom/memory.db")
	getenv := os.Getenv
	homeDir := func() (string, error) { return home, nil }

	legacyMem := filepath.Join(home, ".pi-stack", "memory")
	legacyBundle := filepath.Join(home, ".config", "pi-stack", "knowledge")
	stat := statSet(map[string]bool{legacyMem: true, legacyBundle: true})

	rep := buildPathsReport(getenv, homeDir, stat, nilReadlink)

	if rep.configDir != filepath.Join(cfgHome, "pi-stack") {
		t.Errorf("configDir = %q", rep.configDir)
	}
	if rep.dataDir != filepath.Join(dataHome, "pi-stack") {
		t.Errorf("dataDir = %q", rep.dataDir)
	}
	if rep.stateDir != filepath.Join(stateHome, "pi-stack") {
		t.Errorf("stateDir = %q", rep.stateDir)
	}
	joined := strings.Join(rep.overrides, " ")
	for _, want := range []string{"MEMORY_DB=/custom/memory.db", "XDG_DATA_HOME=" + dataHome} {
		if !strings.Contains(joined, want) {
			t.Errorf("overrides %v missing %q", rep.overrides, want)
		}
	}
	if len(rep.legacy) != 2 {
		t.Fatalf("legacy = %v, want the 2 existing locations", rep.legacy)
	}
}

// TestBuildPathsReport_SymlinkAndRetainedBackupsNotPending (R2-2/R2-3): after a
// successful migrate a legacy memory path is a SYMLINK→NEW (converged) and must
// NOT be pending; the legacy knowledge INDEX dir (rebuilt not moved) and retained
// legacy backups are excluded entirely. A REAL legacy bundle dir IS still pending.
func TestBuildPathsReport_SymlinkAndRetainedBackupsNotPending(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	getenv := func(string) string { return "" }
	homeDir := func() (string, error) { return home, nil }

	legMem := filepath.Join(home, ".pi-stack", "memory") // converged -> symlink→new
	newMem := filepath.Join(home, ".local", "share", "pi-stack", "memory")
	legIndex := filepath.Join(home, ".pi-stack", "knowledge")            // legacy index: excluded
	legBundle := filepath.Join(home, ".config", "pi-stack", "knowledge") // real dir: pending
	legBackups := filepath.Join(home, ".pi-stack", "backups")            // retained: excluded

	lstat := lstatSet(map[string]os.FileMode{
		legMem:     os.ModeSymlink | 0o777, // converged: NOT pending
		newMem:     os.ModeDir | 0o755,     // the symlink target EXISTS -> converged
		legIndex:   os.ModeDir | 0o755,     // legacy index dir: NOT pending
		legBundle:  os.ModeDir | 0o755,     // real bundle dir: pending
		legBackups: os.ModeDir | 0o755,     // retained backups: NOT pending
	})
	readlink := linkSet(map[string]string{legMem: newMem})

	rep := buildPathsReport(getenv, homeDir, lstat, readlink)
	if len(rep.legacy) != 1 || rep.legacy[0] != legBundle {
		t.Fatalf("legacy = %v, want only the real legacy bundle dir %q", rep.legacy, legBundle)
	}
}

// TestBuildPathsReport_DanglingSymlinkIsPending (R6-1): a legacy path that is a
// symlink pointing at the expected new dir, but whose new target no longer EXISTS
// (the user deleted new), is a broken handoff — NOT converged. It must be surfaced
// as pending rather than silently suppressed.
func TestBuildPathsReport_DanglingSymlinkIsPending(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	getenv := func(string) string { return "" }
	homeDir := func() (string, error) { return home, nil }

	legMem := filepath.Join(home, ".pi-stack", "memory")
	newMem := filepath.Join(home, ".local", "share", "pi-stack", "memory")

	// legMem is a symlink -> newMem, but newMem is NOT seeded (deleted): dangling.
	lstat := lstatSet(map[string]os.FileMode{legMem: os.ModeSymlink | 0o777})
	readlink := linkSet(map[string]string{legMem: newMem})

	rep := buildPathsReport(getenv, homeDir, lstat, readlink)
	if len(rep.legacy) != 1 || rep.legacy[0] != legMem {
		t.Fatalf("legacy = %v, want the dangling legacy memory symlink surfaced %q", rep.legacy, legMem)
	}
}

// TestBuildPathsReport_RogueSymlinkAtNewIsPending (R7-2): a legacy symlink->NEW
// whose NEW path is itself a rogue symlink (not a real dir) is NOT converged —
// surface it as pending, matching migrate's conflict on a symlink at the new path.
func TestBuildPathsReport_RogueSymlinkAtNewIsPending(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	getenv := func(string) string { return "" }
	homeDir := func() (string, error) { return home, nil }

	legMem := filepath.Join(home, ".pi-stack", "memory")
	newMem := filepath.Join(home, ".local", "share", "pi-stack", "memory")
	// legMem -> newMem, but newMem is itself a SYMLINK, not a real dir: rogue/conflict.
	lstat := lstatSet(map[string]os.FileMode{
		legMem: os.ModeSymlink | 0o777,
		newMem: os.ModeSymlink | 0o777,
	})
	readlink := linkSet(map[string]string{legMem: newMem})

	rep := buildPathsReport(getenv, homeDir, lstat, readlink)
	if len(rep.legacy) != 1 || rep.legacy[0] != legMem {
		t.Fatalf("legacy = %v, want the rogue-new-symlink legacy memory path surfaced %q", rep.legacy, legMem)
	}
}

// TestBuildPathsReport_NoLegacyNoOverrides: a clean install reports no legacy
// locations and no overrides.
func TestBuildPathsReport_NoLegacyNoOverrides(t *testing.T) {
	home := t.TempDir()
	getenv := func(string) string { return "" }
	homeDir := func() (string, error) { return home, nil }
	stat := statSet(nil)

	rep := buildPathsReport(getenv, homeDir, stat, nilReadlink)
	if len(rep.legacy) != 0 {
		t.Errorf("legacy = %v, want none", rep.legacy)
	}
	if len(rep.overrides) != 0 {
		t.Errorf("overrides = %v, want none", rep.overrides)
	}
}

// TestPrintPathsReport_MigrationHint: the printer names all three bases and, when
// legacy locations exist, prints the migrate hint.
func TestPrintPathsReport_MigrationHint(t *testing.T) {
	rep := pathsReport{
		configDir: "/h/.config/pi-stack",
		dataDir:   "/h/.local/share/pi-stack",
		stateDir:  "/h/.local/state/pi-stack",
		legacy:    []string{"/h/.pi-stack/memory"},
	}
	var buf bytes.Buffer
	printPathsReport(&buf, rep, "")
	out := buf.String()
	for _, want := range []string{"config", "data", "state", "legacy locations", "pi-stack migrate"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestPrintPathsReport_NoMigrationWhenClean: a clean install prints "(none set)"
// and no migrate hint.
func TestPrintPathsReport_NoMigrationWhenClean(t *testing.T) {
	rep := pathsReport{configDir: "/c", dataDir: "/d", stateDir: "/s"}
	var buf bytes.Buffer
	printPathsReport(&buf, rep, "")
	out := buf.String()
	if !strings.Contains(out, "(none set)") {
		t.Errorf("want '(none set)', got:\n%s", out)
	}
	if strings.Contains(out, "pi-stack migrate") {
		t.Errorf("clean install must not print the migrate hint:\n%s", out)
	}
}
