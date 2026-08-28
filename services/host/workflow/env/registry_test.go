package env

import (
	"path/filepath"
	"slices"
	"testing"

	"pix/host/config"
)

// tempConfig isolates $PIX_CONFIG at a fresh temp file, mirroring the same
// helper every other workflow package's config-backed tests use (e.g.
// workflow/pack/pack_rename_test.go's isolatePackRenameHost). config's own
// tempConfig (config/environment_test.go) is unexported and package-local,
// so this package needs its own copy rather than importing one.
func tempConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	t.Setenv("PIX_CONFIG", path)
	return path
}

func loadConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// ── Register/Unregister/Root/Known are thin config wrappers ─────────────

func TestRegisterDelegatesToConfigAddEnvironment(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)

	got, err := Register(cfg, "home", "~/envs/home")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("Register canonical root = %q, want absolute", got)
	}
	if cfg.Environments["home"] != got {
		t.Errorf("cfg.Environments[home] = %q, want %q (Register must go through config.AddEnvironment)", cfg.Environments["home"], got)
	}
}

func TestRegisterRejectsWhatConfigRejects(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)

	if _, err := Register(cfg, "bad name", "/abs/home"); err == nil {
		t.Fatal("Register with a space in the name must be refused, same as config.AddEnvironment")
	}
	if len(cfg.Environments) != 0 {
		t.Errorf("a refused Register must not register anything, got %v", cfg.Environments)
	}
}

func TestRootIsExactNoFuzzyOrPrefix(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)
	want, err := Register(cfg, "home", "/abs/envs/home")
	if err != nil {
		t.Fatal(err)
	}

	if got, ok := Root(cfg, "home"); !ok || got != want {
		t.Fatalf("Root(home) = (%q, %v), want (%q, true)", got, ok, want)
	}
	// A prefix or partial match must never resolve — exact names only.
	for _, name := range []string{"hom", "home2", "Home", " home", "home "} {
		if _, ok := Root(cfg, name); ok {
			t.Errorf("Root(%q) resolved; only the exact registered name %q may resolve", name, "home")
		}
	}
}

func TestUnregisterDelegatesToConfigRemoveEnvironment(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)
	if _, err := Register(cfg, "home", "/abs/envs/home"); err != nil {
		t.Fatal(err)
	}

	if !Unregister(cfg, "home") {
		t.Fatal("Unregister(home) reported no change")
	}
	if _, ok := Root(cfg, "home"); ok {
		t.Error("home must no longer resolve after Unregister")
	}
	if Unregister(cfg, "home") {
		t.Error("Unregister of an already-unregistered name must report no change")
	}
}

// TestUnregisterDeletesNoSourceFile is the AC-15-adjacent proof this unit
// carries even though `pix env forget` itself is E1.11: Unregister mutates
// only cfg.Environments (an in-memory config map). It must never touch the
// environment directory on disk at all — no file created, none removed.
func TestUnregisterDeletesNoSourceFile(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)
	envDir := t.TempDir()
	sentinel := filepath.Join(envDir, ".sbxenv.yaml")
	if err := writeFile(t, sentinel, "schemaVersion: \"1\"\n"); err != nil {
		t.Fatal(err)
	}

	if _, err := Register(cfg, "home", envDir); err != nil {
		t.Fatal(err)
	}
	if !Unregister(cfg, "home") {
		t.Fatal("Unregister(home) reported no change")
	}

	if !fileExists(sentinel) {
		t.Fatalf("Unregister must never delete environment source files; %s is gone", sentinel)
	}
}

func TestKnownIsSortedAndFresh(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)
	if _, err := Register(cfg, "work", "/abs/work"); err != nil {
		t.Fatal(err)
	}
	if _, err := Register(cfg, "home", "/abs/home"); err != nil {
		t.Fatal(err)
	}

	got := Known(cfg)
	want := []string{"home", "work"}
	if !slices.Equal(got, want) {
		t.Fatalf("Known() = %v, want %v", got, want)
	}

	// Mutating the returned slice must not corrupt cfg's own registry.
	got[0] = "corrupted"
	if again := Known(cfg); !slices.Equal(again, want) {
		t.Errorf("Known() after mutating a prior result = %v, want unchanged %v", again, want)
	}
}
