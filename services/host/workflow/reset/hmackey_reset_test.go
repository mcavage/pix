// hmackey_reset_test.go — E2.2's C1/C2 architect correction: `pix reset`
// invalidates/rotates the ONE launcher-owned creation-fingerprint HMAC key
// (hosttrust.EnsureCreationHMACKey) TOGETHER with every acceptance record
// (F6 extension), via the SAME whole-config-dir move-aside every other
// trust document already rides — no environment- or key-specific code path
// in reset.go at all. See reset.go's own doc comment for the invariant this
// proves true rather than merely documents.
package reset

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"pix/host/config"
	"pix/host/hosttrust"
)

// TestReset_InvalidatesTheCreationHMACKeyWithEveryAcceptanceRecord proves
// the key: generated once, present beside config.toml before reset; gone
// from that path after reset (moved into the config dir's .bak- sibling,
// byte-identical); and a fresh load post-reset reports
// ErrCreationHMACKeyMissing — the exact signal envinfo's attribution layer
// turns into ONE ResetInvalidatedDrift record rather than a per-field
// flood.
func TestReset_InvalidatesTheCreationHMACKeyWithEveryAcceptanceRecord(t *testing.T) {
	f := newEnvHostFixture(t)
	stubServeProbe(t, false, false)

	key, err := hosttrust.EnsureCreationHMACKey(f.configDir)
	if err != nil {
		t.Fatalf("EnsureCreationHMACKey: %v", err)
	}
	keyPath := filepath.Join(f.configDir, "creation-hmac.key")
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("key record must exist before reset: %v", err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	out, err := f.runReset(t, cfg, NewOpts(false, false, true, false))
	if err != nil {
		t.Fatalf("reset: %v\n%s", err, out)
	}

	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("key record must be GONE from its live path after reset (stat err = %v)", err)
	}

	dataBackup := backupOf(t, f.configDir)
	movedKeyPath := filepath.Join(dataBackup, "creation-hmac.key")
	movedKey, err := hosttrust.LoadCreationHMACKey(dataBackup)
	if err != nil {
		t.Fatalf("LoadCreationHMACKey(backup dir): %v", err)
	}
	if string(movedKey) != string(key) {
		t.Errorf("the key that traveled to %s must be byte-identical to the original", movedKeyPath)
	}

	if _, err := hosttrust.LoadCreationHMACKey(f.configDir); !errors.Is(err, hosttrust.ErrCreationHMACKeyMissing) {
		t.Fatalf("LoadCreationHMACKey(post-reset config dir) error = %v, want errors.Is ErrCreationHMACKeyMissing", err)
	}
}

// TestReset_RegeneratedKeyAfterResetDiffersFromThePreResetOne proves
// "rotate": a fresh EnsureCreationHMACKey call after reset must never
// silently recover the OLD key from anywhere reset left behind on the live
// path — it generates a genuinely new one, matching "generated once" per
// config dir, not "generated once ever".
func TestReset_RegeneratedKeyAfterResetDiffersFromThePreResetOne(t *testing.T) {
	f := newEnvHostFixture(t)
	stubServeProbe(t, false, false)

	oldKey, err := hosttrust.EnsureCreationHMACKey(f.configDir)
	if err != nil {
		t.Fatalf("EnsureCreationHMACKey: %v", err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if _, err := f.runReset(t, cfg, NewOpts(false, false, true, false)); err != nil {
		t.Fatalf("reset: %v", err)
	}

	newKey, err := hosttrust.EnsureCreationHMACKey(f.configDir)
	if err != nil {
		t.Fatalf("EnsureCreationHMACKey (post-reset): %v", err)
	}
	if string(newKey) == string(oldKey) {
		t.Error("the post-reset key must be freshly generated, never the pre-reset one")
	}
}
