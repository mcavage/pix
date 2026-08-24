package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConfig_WarnsAboutHandEditedEnvironmentBeforeAnySave is the user-facing
// half of the noncanonical-[environments]-entry regression: config.go's
// dropNoncanonicalEnvironments fails closed on a hand-edited entry (fires
// during Load, inside applyDefaults) and Save() later erases it from disk for
// good. That is deliberate, but it must never be SILENT — Deps.Config is the
// path almost every `pix` command takes to get a *config.Config at all, and it
// warns on d.Err the moment the config is loaded, well before anything calls
// Save(). This test proves the exact "environments.<name>" diagnostic for a
// hand-edited entry reaches that real, ordinary command path, not just
// config.UnknownKeys() in isolation.
func TestConfig_WarnsAboutHandEditedEnvironmentBeforeAnySave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	const toml = "environment = \"home\"\n\n[environments]\n" +
		"home = \"~/envs/home\"\n" +
		"good = \"/abs/canonical/good\"\n"
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIX_CONFIG", path)

	var errBuf bytes.Buffer
	d := &Deps{Err: &errBuf}

	cfg, err := d.Config()
	if err != nil {
		t.Fatalf("Config: %v", err)
	}

	// The warning must already be on stderr from THIS call — before any test
	// code (or command) goes on to mutate and Save the config.
	warning := errBuf.String()
	if !strings.Contains(warning, "environments.home") {
		t.Fatalf("Deps.Config() stderr = %q, want it to name the dropped %q entry", warning, "environments.home")
	}
	if strings.Contains(warning, "environments.good") {
		t.Errorf("Deps.Config() stderr = %q, wrongly warned about the canonical entry", warning)
	}

	// Sanity: the in-memory config really did drop it (the warning is not a
	// stale/unrelated message), which is what makes a later Save() the
	// point of no return this diagnostic exists to warn about in advance.
	if _, ok := cfg.Environments["home"]; ok {
		t.Errorf("Environments kept the hand-edited entry: %v", cfg.Environments)
	}
}
