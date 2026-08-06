package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

// isolateConfig points config.Load at a throwaway config.toml so these tests
// never touch a real user's config.
func isolateConfig(t *testing.T) {
	t.Helper()
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
}

// TestReadProfileScope_UnscopedByDefault: a workspace with no .pix/profile
// marker (the common case — no pack, or a pack with no explicit
// memory_scope) resolves to the unscoped "".
func TestReadProfileScope_UnscopedByDefault(t *testing.T) {
	t.Chdir(t.TempDir())
	if got := ReadProfileScope(); got != "" {
		t.Fatalf("ReadProfileScope() = %q, want %q", got, "")
	}
}

// TestReadProfileScope_ResolvesWrittenScope: the exact scope
// packinfo.WriteMemoryScope wrote to <cwd>/.pix/profile comes back trimmed,
// matching the writer's semantics (this is the round trip memory_cmd.go's
// withMemory depends on to forward the right scope to the daemon).
func TestReadProfileScope_ResolvesWrittenScope(t *testing.T) {
	ws := t.TempDir()
	if err := WriteStateFile(ws, "profile", []byte("acme\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(ws)
	if got := ReadProfileScope(); got != "acme" {
		t.Fatalf("ReadProfileScope() = %q, want %q", got, "acme")
	}
}

// TestReadProfileScope_SymlinkedPixDirFailsSafe: a hostile symlinked .pix
// dir must not be followed, but it also must not turn an unrelated command
// (pix memory, doctor, status) into a hard failure — it degrades to the
// unscoped "".
func TestReadProfileScope_SymlinkedPixDirFailsSafe(t *testing.T) {
	ws := t.TempDir()
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "profile"), []byte("victim\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	RequireSymlink(t, target, filepath.Join(ws, ".pix"))
	t.Chdir(ws)

	if got := ReadProfileScope(); got != "" {
		t.Fatalf("ReadProfileScope() through a symlinked .pix dir = %q, want %q (fail-safe, not the victim's scope)", got, "")
	}
}

// TestLoadResolvedConfig_ReturnsRealProfileNotAlwaysEmpty: the regression this
// whole file guards against. LoadResolvedConfig used to hardcode "" for the
// profile return no matter what — silently breaking memory scoping for every
// CLI caller (pix memory, doctor, status) since profiles were retired. It
// must now resolve the SAME scope ReadProfileScope/packinfo.WriteMemoryScope
// agree on.
func TestLoadResolvedConfig_ReturnsRealProfileNotAlwaysEmpty(t *testing.T) {
	isolateConfig(t)
	ws := t.TempDir()
	if err := WriteStateFile(ws, "profile", []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(ws)

	cfg, profile, err := LoadResolvedConfig()
	if err != nil {
		t.Fatalf("LoadResolvedConfig: %v", err)
	}
	if cfg == nil {
		t.Fatal("LoadResolvedConfig returned a nil config with no error")
	}
	if profile != "work" {
		t.Fatalf("LoadResolvedConfig profile = %q, want %q", profile, "work")
	}

	// And the un-scoped case still resolves to "", not an error.
	t.Chdir(t.TempDir())
	if _, profile, err := LoadResolvedConfig(); err != nil || profile != "" {
		t.Fatalf("LoadResolvedConfig (unscoped) = (%q, %v), want (\"\", nil)", profile, err)
	}
}
