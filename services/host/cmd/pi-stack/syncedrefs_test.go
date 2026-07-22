// syncedrefs_test.go: symlink-safety regression coverage for the synced-refs
// store (syncedrefs.go). See its own doc comment for why this exists: sbx
// secret values are write-only, so "did the 1Password ref change since we
// last synced it" is answered by this launcher-owned record, never sbx's own
// store — and like every other host-state file in an attacker-influenced
// XDG config dir, it must refuse to read or write through a symlink rather
// than follow it (item 8: restore this regression coverage).
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A symlinked synced-refs.json (in place of the real file) must be refused on
// LOAD, not silently followed and read as if it were the real store — a
// load error degrades to "no record" (an extra confirm-before-overwrite
// prompt), never a silent read-through.
func TestSyncedRefsStore_SymlinkRefusedOnLoad(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "cfg", "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	cfgDir := filepath.Dir(configPathForTest(t))
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "victim.json")
	if err := os.WriteFile(target, []byte(`{"version":1,"synced":{"ANTHROPIC_API_KEY":"op://evil/x/y"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(cfgDir, syncedRefsStoreName)
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	if _, err := loadSyncedRefsStore(); err == nil {
		t.Fatal("a symlinked synced-refs.json must be refused, not read through")
	}
	// syncedRef degrades a load error to "no record" rather than propagating —
	// confirm that degrade path is exactly what happens, and it never returns
	// the planted value.
	if ref, ok := syncedRef("ANTHROPIC_API_KEY"); ok {
		t.Errorf("must degrade to no-record, not surface the symlinked file's content: %q", ref)
	}
}

// A symlinked synced-refs.json must also be refused on SAVE — recordSyncedRef
// must error rather than write through the link (which would corrupt
// whatever the link points at).
func TestSyncedRefsStore_SymlinkRefusedOnSave(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "cfg", "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	cfgDir := filepath.Dir(configPathForTest(t))
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "victim.json")
	const victimContent = `{"do":"not touch"}`
	if err := os.WriteFile(target, []byte(victimContent), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(cfgDir, syncedRefsStoreName)
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	if err := recordSyncedRef("ANTHROPIC_API_KEY", "op://v/a/k"); err == nil {
		t.Fatal("recordSyncedRef must refuse to write through a symlinked store")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != victimContent {
		t.Errorf("symlink target must be untouched, got:\n%s", got)
	}
}

// configPathForTest resolves config.Path() the same way the production code
// does, after PI_STACK_CONFIG is set — a small local helper so these tests
// don't need to import the config package's internals directly.
func configPathForTest(t *testing.T) string {
	t.Helper()
	return syncedRefsStorePath()
}

// --- new-item: digest storage, mode, and symlink safety --------------------

// recordSyncedRefWithDigest must persist BOTH the ref and the digest
// atomically, and syncedRefKnownSame must require both to match — a digest
// mismatch (rotated value at the same ref) must not be treated as same.
func TestSyncedRefsStore_DigestRoundtripAndKnownSame(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "cfg", "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	if err := recordSyncedRefWithDigest("ANTHROPIC_API_KEY", "op://v/a/k", secretDigestHex("sk-val")); err != nil {
		t.Fatal(err)
	}
	if ref, ok := syncedRef("ANTHROPIC_API_KEY"); !ok || ref != "op://v/a/k" {
		t.Fatalf("ref not recorded: %q, %v", ref, ok)
	}
	if !syncedRefKnownSame("ANTHROPIC_API_KEY", "op://v/a/k", "sk-val") {
		t.Error("matching ref + matching value must be known-same")
	}
	if syncedRefKnownSame("ANTHROPIC_API_KEY", "op://v/a/k", "sk-DIFFERENT") {
		t.Error("same ref but a different resolved value must NOT be known-same")
	}
	if syncedRefKnownSame("ANTHROPIC_API_KEY", "op://v/a/DIFFERENT", "sk-val") {
		t.Error("a different ref must NOT be known-same even with a matching value")
	}
}

// A legacy record (recordSyncedRef, no digest) must never be known-same, even
// when the ref matches exactly and the value would hash to nothing recorded —
// there is simply no digest to compare against, so it must fail closed.
func TestSyncedRefsStore_LegacyRecordNeverKnownSame(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "cfg", "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	if err := recordSyncedRef("ANTHROPIC_API_KEY", "op://v/a/k"); err != nil {
		t.Fatal(err)
	}
	if syncedRefDigest("ANTHROPIC_API_KEY") != "" {
		t.Error("a legacy record must carry no digest")
	}
	if syncedRefKnownSame("ANTHROPIC_API_KEY", "op://v/a/k", "sk-val") {
		t.Error("a legacy record (ref matches, no digest) must never be known-same")
	}
}

// recordSyncedRef (legacy path) must clear any previously-recorded digest for
// that envVar — a NEW ref must never inherit the OLD ref's digest.
func TestSyncedRefsStore_RecordSyncedRefClearsStaleDigest(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "cfg", "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	if err := recordSyncedRefWithDigest("ANTHROPIC_API_KEY", "op://v/a/k-OLD", secretDigestHex("sk-old")); err != nil {
		t.Fatal(err)
	}
	if err := recordSyncedRef("ANTHROPIC_API_KEY", "op://v/a/k-NEW"); err != nil {
		t.Fatal(err)
	}
	if digest := syncedRefDigest("ANTHROPIC_API_KEY"); digest != "" {
		t.Errorf("a new ref recorded without a digest must clear the old digest, got %q", digest)
	}
}

// The digest is metadata only: the store file itself must never be readable
// as anything but 0600 (same posture as the rest of the store), and — the
// point of this whole feature — the digest must never equal or contain the
// resolved secret value verbatim (it's a one-way hash).
func TestSyncedRefsStore_DigestFileModeAndNeverContainsRawValue(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "cfg", "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	const secretVal = "sk-should-never-appear-in-the-store-file"
	if err := recordSyncedRefWithDigest("ANTHROPIC_API_KEY", "op://v/a/k", secretDigestHex(secretVal)); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(syncedRefsStorePath())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("synced-refs.json must be 0600, got %o", fi.Mode().Perm())
	}
	b, err := os.ReadFile(syncedRefsStorePath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), secretVal) {
		t.Fatalf("the resolved secret value must never be persisted, got:\n%s", b)
	}
	if !strings.Contains(string(b), secretDigestHex(secretVal)) {
		t.Errorf("the digest itself must be persisted as metadata, got:\n%s", b)
	}
}

// A symlinked synced-refs.json must be refused on save even when the mutation
// is recordSyncedRefWithDigest (not just the plain ref-only recordSyncedRef
// already covered above) — the digest write path must share the exact same
// symlink-refusal guard, not a parallel one that could regress independently.
func TestSyncedRefsStore_DigestWriteSymlinkRefused(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "cfg", "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	cfgDir := filepath.Dir(configPathForTest(t))
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "victim.json")
	const victimContent = `{"do":"not touch"}`
	if err := os.WriteFile(target, []byte(victimContent), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(cfgDir, syncedRefsStoreName)
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	if err := recordSyncedRefWithDigest("ANTHROPIC_API_KEY", "op://v/a/k", secretDigestHex("sk-val")); err == nil {
		t.Fatal("recordSyncedRefWithDigest must refuse to write through a symlinked store")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != victimContent {
		t.Errorf("symlink target must be untouched, got:\n%s", got)
	}
}
