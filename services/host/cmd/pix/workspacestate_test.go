package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// requireSymlink creates a symlink or skips the test where symlinks are
// unavailable (Windows without developer mode).
func requireSymlink(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Symlink(oldname, newname); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Fatalf("os.Symlink(%q, %q): %v", oldname, newname, err)
	}
}

// Normal case: no .pix dir yet — the helper creates it as a real dir and
// the file round-trips with the requested permissions.
func TestWriteWorkspaceStateFileNormal(t *testing.T) {
	ws := t.TempDir()
	if err := writeWorkspaceStateFile(ws, "profile", []byte("work\n"), 0o644); err != nil {
		t.Fatalf("writeWorkspaceStateFile: %v", err)
	}
	dest := filepath.Join(ws, ".pix", "profile")
	b, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("reading %s: %v", dest, err)
	}
	if string(b) != "work\n" {
		t.Fatalf("content = %q, want %q", b, "work\n")
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Lstat(dest)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o644 {
			t.Fatalf("perm = %o, want 644", fi.Mode().Perm())
		}
	}
	// Overwrite of an existing regular file works too (temp+rename path).
	if err := writeWorkspaceStateFile(ws, "profile", []byte("personal\n"), 0o644); err != nil {
		t.Fatalf("second write: %v", err)
	}
	if b, _ := os.ReadFile(dest); string(b) != "personal\n" {
		t.Fatalf("after rewrite content = %q, want %q", b, "personal\n")
	}
}

// A hostile TRACKED symlink at .pix/profile must be REPLACED by the
// write, never followed: the symlink's target keeps its bytes, and the
// destination becomes a regular file with the new content.
func TestWriteWorkspaceStateFileSymlinkedDestination(t *testing.T) {
	ws := t.TempDir()
	stateDir := filepath.Join(ws, ".pix")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "victim")
	const secret = "precious host bytes\n"
	if err := os.WriteFile(target, []byte(secret), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(stateDir, "profile")
	requireSymlink(t, target, dest)

	if err := writeWorkspaceStateFile(ws, "profile", []byte("work\n"), 0o644); err != nil {
		t.Fatalf("writeWorkspaceStateFile over symlinked dest: %v", err)
	}
	// The symlink target must be untouched — os.WriteFile would have
	// truncated it; the temp+rename must not.
	if b, err := os.ReadFile(target); err != nil || string(b) != secret {
		t.Fatalf("symlink target clobbered: content = %q (err %v), want %q", b, err, secret)
	}
	// The destination is now a REGULAR file (the symlink was replaced).
	fi, err := os.Lstat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("%s is still a symlink after the write", dest)
	}
	if b, _ := os.ReadFile(dest); string(b) != "work\n" {
		t.Fatalf("dest content = %q, want %q", b, "work\n")
	}
}

// A symlinked .pix DIRECTORY must be refused outright: no state file may
// be written through it, and nothing appears in the symlink's target.
func TestWriteWorkspaceStateFileSymlinkedDir(t *testing.T) {
	ws := t.TempDir()
	target := t.TempDir() // where a hostile .pix symlink points
	existing := filepath.Join(target, "profile")
	const secret = "pre-existing\n"
	if err := os.WriteFile(existing, []byte(secret), 0o644); err != nil {
		t.Fatal(err)
	}
	requireSymlink(t, target, filepath.Join(ws, ".pix"))

	if err := writeWorkspaceStateFile(ws, "profile", []byte("work\n"), 0o644); err == nil {
		t.Fatal("writeWorkspaceStateFile through a symlinked .pix dir: want error, got nil")
	}
	// The target dir must be untouched: the existing file keeps its bytes and
	// nothing new (file or leftover temp) was created inside it.
	if b, err := os.ReadFile(existing); err != nil || string(b) != secret {
		t.Fatalf("file behind symlinked dir changed: %q (err %v), want %q", b, err, secret)
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "profile" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("symlink target dir polluted: %v", names)
	}
}

// End-to-end through a real caller: writeMemoryScope must not truncate the
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
	writeMemoryScope(ws, &packInfo{Manifest: packManifest{Name: "acme", MemoryScope: "acme"}})

	if b, err := os.ReadFile(target); err != nil || string(b) != secret {
		t.Fatalf("writeMemoryScope followed the symlink: target = %q (err %v), want %q", b, err, secret)
	}
	if b, _ := os.ReadFile(filepath.Join(stateDir, "profile")); string(b) != "acme\n" {
		t.Fatalf("profile = %q, want %q", b, "acme\n")
	}
}

// End-to-end through an error-returning caller: writeKnowledgeScope refuses a
// symlinked .pix dir instead of writing through it.
func TestWriteKnowledgeScopeSymlinkedDirRefused(t *testing.T) {
	ws := t.TempDir()
	requireSymlink(t, t.TempDir(), filepath.Join(ws, ".pix"))
	if err := writeKnowledgeScope(ws, []string{"/some/bundle"}); err == nil {
		t.Fatal("writeKnowledgeScope through a symlinked .pix dir: want error, got nil")
	}
}

// A .pix that is a SYMLINK to another repo's .pix must be refused
// outright: removeWorkspaceStateFile must not traverse the symlinked parent
// to delete the TARGET repo's profile/knowledge.scope/sandbox.pack, which a
// plain os.Remove(filepath.Join(workspace, ".pix", name)) would do (it
// only refuses to follow a symlinked *destination file*, not a symlinked
// *parent directory*).
func TestRemoveWorkspaceStateFileSymlinkedDirRefused(t *testing.T) {
	ws := t.TempDir()
	target := t.TempDir() // another "repo"'s real .pix dir
	victims := map[string]string{
		"profile":         "acme\n",
		"knowledge.scope": "/bundle/a\n",
		"sandbox.pack":    "/packs/acme\n",
	}
	for name, content := range victims {
		if err := os.WriteFile(filepath.Join(target, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	requireSymlink(t, target, filepath.Join(ws, ".pix"))

	for name := range victims {
		if err := removeWorkspaceStateFile(ws, name); err == nil {
			t.Fatalf("removeWorkspaceStateFile(%q) through a symlinked .pix dir: want error, got nil", name)
		}
	}

	// The target repo's files must all still be intact, byte for byte.
	for name, want := range victims {
		b, err := os.ReadFile(filepath.Join(target, name))
		if err != nil {
			t.Fatalf("target %s missing after refused removal: %v", name, err)
		}
		if string(b) != want {
			t.Fatalf("target %s = %q, want %q", name, b, want)
		}
	}
}

// A normal (real) .pix dir: removeWorkspaceStateFile actually removes
// the file, and a missing file is a clean no-op (not an error).
func TestRemoveWorkspaceStateFileNormal(t *testing.T) {
	ws := t.TempDir()
	stateDir := filepath.Join(ws, ".pix")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(stateDir, "profile")
	if err := os.WriteFile(dest, []byte("acme\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := removeWorkspaceStateFile(ws, "profile"); err != nil {
		t.Fatalf("removeWorkspaceStateFile: %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("profile still present after removal (err=%v)", err)
	}

	// Removing an already-absent file (or an absent .pix dir entirely)
	// is a no-op, not an error.
	if err := removeWorkspaceStateFile(ws, "profile"); err != nil {
		t.Fatalf("removeWorkspaceStateFile on already-absent file: %v", err)
	}
	if err := removeWorkspaceStateFile(t.TempDir(), "profile"); err != nil {
		t.Fatalf("removeWorkspaceStateFile with no .pix dir at all: %v", err)
	}
}

// atomicWriteInDir must land the file with the REQUESTED mode (fchmod'd on
// the open handle BEFORE fsync, so data + metadata are flushed together under
// the intended mode — CreateTemp starts at 0600).
func TestAtomicWriteInDir_AppliesRequestedMode(t *testing.T) {
	dir := t.TempDir()
	for _, perm := range []os.FileMode{0o600, 0o644} {
		name := fmt.Sprintf("f-%o.txt", perm)
		if err := atomicWriteInDir(dir, name, []byte("data"), perm); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != perm {
			t.Errorf("mode = %o, want %o", got, perm)
		}
	}
}
