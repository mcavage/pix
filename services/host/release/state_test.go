package release

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInstallStatePath(t *testing.T) {
	got := InstallStatePath(filepath.FromSlash("/home/u/.pix"))
	want := filepath.FromSlash("/home/u/.pix/state/release.json")
	if got != want {
		t.Errorf("InstallStatePath = %q, want %q", got, want)
	}
}

func TestLoadInstalled_MissingFileReturnsNilNil(t *testing.T) {
	home := t.TempDir()
	m, err := LoadInstalled(home)
	if err != nil {
		t.Fatalf("LoadInstalled() error = %v, want nil for a machine with nothing installed yet", err)
	}
	if m != nil {
		t.Errorf("LoadInstalled() = %+v, want nil", m)
	}
}

func TestSaveInstalled_ThenLoad_RoundTrips(t *testing.T) {
	home := t.TempDir()
	m := validManifest()
	if err := SaveInstalled(home, m); err != nil {
		t.Fatalf("SaveInstalled() error = %v", err)
	}

	got, err := LoadInstalled(home)
	if err != nil {
		t.Fatalf("LoadInstalled() error = %v", err)
	}
	if got == nil {
		t.Fatal("LoadInstalled() = nil, want the saved manifest back")
	}
	if *got != m {
		t.Errorf("LoadInstalled() = %+v, want %+v", *got, m)
	}
}

func TestSaveInstalled_FileMode0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on windows")
	}
	home := t.TempDir()
	if err := SaveInstalled(home, validManifest()); err != nil {
		t.Fatalf("SaveInstalled() error = %v", err)
	}
	info, err := os.Stat(InstallStatePath(home))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("release.json mode = %o, want 0600 (it records machine state)", perm)
	}
}

func TestSaveInstalled_RejectsInvalidManifest_NoFileWritten(t *testing.T) {
	home := t.TempDir()
	if err := SaveInstalled(home, Manifest{}); err == nil {
		t.Fatal("SaveInstalled() = nil error, want an invalid manifest refused")
	}
	if _, err := os.Stat(InstallStatePath(home)); !os.IsNotExist(err) {
		t.Errorf("expected no release.json to exist after a refused save, stat err = %v", err)
	}
}

func TestSaveInstalled_UpgradeOverwritesAtomically(t *testing.T) {
	home := t.TempDir()
	v1 := validManifest()
	if err := SaveInstalled(home, v1); err != nil {
		t.Fatalf("SaveInstalled(v1) error = %v", err)
	}

	v2 := v1
	v2.Version = "2.1.0"
	v2.KitRevision = "kit-rev-2"
	if err := SaveInstalled(home, v2); err != nil {
		t.Fatalf("SaveInstalled(v2) error = %v", err)
	}

	got, err := LoadInstalled(home)
	if err != nil {
		t.Fatalf("LoadInstalled() error = %v", err)
	}
	if got == nil || *got != v2 {
		t.Errorf("LoadInstalled() = %+v, want the upgraded manifest %+v", got, v2)
	}
}

func TestLoadInstalled_CorruptFileErrorsWithPath(t *testing.T) {
	home := t.TempDir()
	path := InstallStatePath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadInstalled(home)
	if err == nil {
		t.Fatal("LoadInstalled() = nil error, want the corrupt file refused")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("LoadInstalled() error = %q, want it to name the offending path %q", err, path)
	}
}
