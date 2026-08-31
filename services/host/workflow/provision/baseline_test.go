package provision

import (
	"testing"

	"pix/host/config"
	"pix/host/pixhome"
)

// baseline_test.go proves the Round-7 fix to EnsureDefaultEnvironment: a
// FRESH host (zero environments) that gets the scaffolded `default`
// environment also gets it SELECTED, atomically, under the config lock,
// with every sibling config.toml field left exactly as it was — and a host
// that already names a default (however that happened) is never
// second-guessed.

func TestEnsureDefaultEnvironment_SelectsItAtomically_PreservingSiblings(t *testing.T) {
	home := pixhome.New(t.TempDir())

	// A sibling field written BEFORE this call, the way `pix setup`'s own
	// memory-port allocation would have already run by the time
	// EnsureDefaultEnvironment executes (Setup's step 5 runs after steps
	// 3/4, but this proves the invariant independent of that ordering: ANY
	// writer's field must survive).
	if err := config.WithLockAt(home.Home, func(c *config.Config) error {
		c.MemoryPort = 54321
		c.VersionPin = "9.9.9"
		return nil
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	created, err := EnsureDefaultEnvironment(home, testManifest())
	if err != nil {
		t.Fatalf("EnsureDefaultEnvironment: %v", err)
	}
	if !created {
		t.Fatalf("created = false, want true on a fresh home")
	}

	c, err := config.LoadFrom(config.PathAt(home.Home))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if c.DefaultEnvironment != DefaultEnvironmentName {
		t.Errorf("DefaultEnvironment = %q, want %q", c.DefaultEnvironment, DefaultEnvironmentName)
	}
	if c.MemoryPort != 54321 {
		t.Errorf("MemoryPort = %d, want 54321 (sibling field must survive)", c.MemoryPort)
	}
	if c.VersionPin != "9.9.9" {
		t.Errorf("VersionPin = %q, want 9.9.9 (sibling field must survive)", c.VersionPin)
	}
}

func TestEnsureDefaultEnvironment_NeverOverwritesAnExistingDefault(t *testing.T) {
	home := pixhome.New(t.TempDir())
	if err := config.WithLockAt(home.Home, func(c *config.Config) error {
		c.DefaultEnvironment = "already-chosen"
		return nil
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	created, err := EnsureDefaultEnvironment(home, testManifest())
	if err != nil {
		t.Fatalf("EnsureDefaultEnvironment: %v", err)
	}
	if !created {
		t.Fatalf("created = false, want true: the host still has zero environment directories")
	}

	c, err := config.LoadFrom(config.PathAt(home.Home))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if c.DefaultEnvironment != "already-chosen" {
		t.Errorf("DefaultEnvironment = %q, want the pre-existing value left untouched", c.DefaultEnvironment)
	}
}

func TestEnsureDefaultEnvironment_ExistingEnvironmentLeavesDefaultAlone(t *testing.T) {
	home := pixhome.New(t.TempDir())
	// An environment already exists (a user-authored one, not created by
	// this function): EnsureDefaultEnvironment must not create a second
	// one and must not touch config.toml at all — "do not guess" belongs
	// to `pix doctor`'s remedy, not a silent write here.
	if _, err := EnsureDefaultEnvironment(home, testManifest()); err != nil {
		t.Fatalf("first EnsureDefaultEnvironment: %v", err)
	}
	// Reset the default to simulate a host with an environment but no
	// selection (e.g. its config.toml was hand-edited or restored).
	if err := config.WithLockAt(home.Home, func(c *config.Config) error {
		c.DefaultEnvironment = ""
		return nil
	}); err != nil {
		t.Fatalf("clear default: %v", err)
	}

	created, err := EnsureDefaultEnvironment(home, testManifest())
	if err != nil {
		t.Fatalf("second EnsureDefaultEnvironment: %v", err)
	}
	if created {
		t.Fatalf("created = true, want false: the host already has an environment")
	}
	c, err := config.LoadFrom(config.PathAt(home.Home))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if c.DefaultEnvironment != "" {
		t.Errorf("DefaultEnvironment = %q, want empty: EnsureDefaultEnvironment must never guess one for a host it did not scaffold", c.DefaultEnvironment)
	}
}
