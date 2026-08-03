package main

// These two moved BACK from the workspace package. They exercise pack and run
// behaviour (pack.WriteMemoryScope, launch.WireKnowledgeScope) that happens to write a
// workspace state file — the subject is the caller, not the writer, so they
// belong with the caller.

import (
	"os"
	"path/filepath"
	"pix/host/workflow/launch"
	"pix/host/workflow/pack"
	"runtime"
	"testing"
	"time"
)

// fixedClock is a deterministic clock for receipt tests. It moved here with the
// tests that use it; the workspace package has its own copy for its own tests,
// which is the correct amount of duplication for a three-line test helper.
func fixedClock(ts string) func() time.Time {
	return func() time.Time {
		t, _ := time.Parse(time.RFC3339, ts)
		return t
	}
}

// End-to-end through a real caller: pack.WriteMemoryScope must not truncate the
// target of a symlinked .pix/profile (the launcher-write clobber).
func TestWriteMemoryScopeSymlinkSafe(t *testing.T) {
	ws := t.TempDir()
	stateDir := filepath.Join(ws, ".pix")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "victim")
	const secret = "do not clobber\n"
	if err := os.WriteFile(target, []byte(secret), 0o644); err != nil {
		t.Fatal(err)
	}
	requireSymlink(t, target, filepath.Join(stateDir, "profile"))

	// Needs an EXPLICIT memory_scope to write at all (a bare name no longer scopes).
	pack.WriteMemoryScope(ws, &pack.Info{Manifest: pack.Manifest{Name: "acme", MemoryScope: "acme"}})

	if b, err := os.ReadFile(target); err != nil || string(b) != secret {
		t.Fatalf("pack.WriteMemoryScope followed the symlink: target = %q (err %v), want %q", b, err, secret)
	}
	if b, _ := os.ReadFile(filepath.Join(stateDir, "profile")); string(b) != "acme\n" {
		t.Fatalf("profile = %q, want %q", b, "acme\n")
	}
}

// End-to-end through an error-returning caller: launch.WriteKnowledgeScope refuses a
// symlinked .pix dir instead of writing through it.
func TestWriteKnowledgeScopeSymlinkedDirRefused(t *testing.T) {
	ws := t.TempDir()
	requireSymlink(t, t.TempDir(), filepath.Join(ws, ".pix"))
	if err := launch.WriteKnowledgeScope(ws, []string{"/some/bundle"}); err == nil {
		t.Fatal("launch.WriteKnowledgeScope through a symlinked .pix dir: want error, got nil")
	}
}

// requireSymlink creates a symlink or skips on a platform that cannot. Each
// package carries its own three-line copy rather than importing a testing
// helper across a package boundary, which is the correct amount of duplication
// for something this size.
func requireSymlink(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Symlink(oldname, newname); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Fatalf("os.Symlink(%q, %q): %v", oldname, newname, err)
	}
}

// readFile is a three-line test helper that left with knowledge_test.go. Each
// package carrying its own copy is the right amount of duplication for this.
func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
