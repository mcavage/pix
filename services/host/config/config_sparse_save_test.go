package config

import (
	"os"
	"path/filepath"
	"testing"
)

// These tests are the regression guard for config-default PETRIFICATION: the
// first `pix config set <anything>` used to freeze the then-current
// defaults (applied in memory by Load/applyDefaults) into config.toml forever,

// tempConfig points PIX_HOME at a fresh temp dir and returns config.toml's
// path under it (the only file config.Path resolves in production now).
func tempConfig(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	return filepath.Join(home, "config.toml")
}

func TestSaveOmitsAppliedDefaultsAndEmptyLegacySections(t *testing.T) {
	path := tempConfig(t)
	c, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	c.DefaultEnvironment = "default"
	if err := SaveTo(path, c); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "default_environment = \"default\"\n"
	if string(got) != want {
		t.Fatalf("saved config = %q, want sparse %q", got, want)
	}
}

func TestSaveKeepsExplicitNondefaultMemoryChoice(t *testing.T) {
	path := tempConfig(t)
	c, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	c.MemoryCapture = MemoryCaptureExperimentalAuto
	if err := SaveTo(path, c); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "memory_capture = \"experimental-auto\"\n" {
		t.Fatalf("saved config = %q", got)
	}
}

// (d) Simulate a DEFAULT CHANGE reaching an existing user: a config file with
// no watcher key must resolve to the CURRENT DefaultMemoryWatcherModel, proving
// future default bumps propagate to saved configs.
func TestLoadResolvesCurrentDefaultWhenKeyAbsent(t *testing.T) {
	path := tempConfig(t)
	if err := os.WriteFile(path, []byte("version_pin = \"1.2.3\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.MemoryWatcherModel != DefaultMemoryWatcherModel {
		t.Errorf("MemoryWatcherModel = %q, want current default %q (default changes must reach users)",
			got.MemoryWatcherModel, DefaultMemoryWatcherModel)
	}
	if got.MemoryEmbedModel != DefaultMemoryEmbedModel || got.OllamaBridgeModel != DefaultOllamaBridgeModel {
		t.Errorf("embed/bridge models = %q/%q, want current defaults", got.MemoryEmbedModel, got.OllamaBridgeModel)
	}
	if got.VersionPin != "1.2.3" {
		t.Errorf("VersionPin = %q, want the explicit value", got.VersionPin)
	}
}
