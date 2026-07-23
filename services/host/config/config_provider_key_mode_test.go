package config

import (
	"strings"
	"testing"
)

// ValidProviderKeyMode accepts only the three legal states: unset, sbx, and
// 1password. Anything else must be rejected before it ever reaches disk.
func TestValidProviderKeyMode(t *testing.T) {
	for _, ok := range []string{"", "sbx", "1password"} {
		if !ValidProviderKeyMode(ok) {
			t.Errorf("ValidProviderKeyMode(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"Sbx", "SBX", "1Password", "oauth", " sbx", "sbx "} {
		if ValidProviderKeyMode(bad) {
			t.Errorf("ValidProviderKeyMode(%q) = true, want false", bad)
		}
	}
}

// provider_key_mode is sparse like every other scalar: an unset (empty)
// value never appears in the saved file, and an explicit value round-trips.
func TestProviderKeyMode_SparseSaveRoundTrips(t *testing.T) {
	path := tempConfig(t)

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProviderKeyMode != "" {
		t.Fatalf("default ProviderKeyMode = %q, want empty (legacy/unset)", cfg.ProviderKeyMode)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if raw := rawFile(t, path); strings.Contains(raw, "provider_key_mode") {
		t.Errorf("an unset provider_key_mode must not be written:\n%s", raw)
	}

	cfg.ProviderKeyMode = ProviderKeyModeSbx
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	raw := rawFile(t, path)
	if !strings.Contains(raw, `provider_key_mode = "sbx"`) {
		t.Errorf("explicit provider_key_mode=sbx must be written:\n%s", raw)
	}

	got, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProviderKeyMode != ProviderKeyModeSbx {
		t.Errorf("reloaded ProviderKeyMode = %q, want %q", got.ProviderKeyMode, ProviderKeyModeSbx)
	}
}
