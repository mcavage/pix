package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"pix/host/sys"
	"runtime"
	"testing"
)

// RequireSymlink creates a symlink or skips the test where symlinks are
// unavailable (Windows without developer mode).
func RequireSymlink(t *testing.T, oldname, newname string) {
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
	if err := WriteStateFile(ws, "profile", []byte("work\n"), 0o644); err != nil {
		t.Fatalf("WriteStateFile: %v", err)
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
	if err := WriteStateFile(ws, "profile", []byte("personal\n"), 0o644); err != nil {
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
	RequireSymlink(t, target, dest)

	if err := WriteStateFile(ws, "profile", []byte("work\n"), 0o644); err != nil {
		t.Fatalf("WriteStateFile over symlinked dest: %v", err)
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
	RequireSymlink(t, target, filepath.Join(ws, ".pix"))

	if err := WriteStateFile(ws, "profile", []byte("work\n"), 0o644); err == nil {
		t.Fatal("WriteStateFile through a symlinked .pix dir: want error, got nil")
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

// A .pix that is a SYMLINK to another repo's .pix must be refused
// outright: RemoveStateFile must not traverse the symlinked parent
// to delete the TARGET repo's profile/knowledge.scope/sandbox.pack, which a
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
	RequireSymlink(t, target, filepath.Join(ws, ".pix"))

	for name := range victims {
		if err := RemoveStateFile(ws, name); err == nil {
			t.Fatalf("RemoveStateFile(%q) through a symlinked .pix dir: want error, got nil", name)
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

// A normal (real) .pix dir: RemoveStateFile actually removes
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

	if err := RemoveStateFile(ws, "profile"); err != nil {
		t.Fatalf("RemoveStateFile: %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("profile still present after removal (err=%v)", err)
	}

	// Removing an already-absent file (or an absent .pix dir entirely)
	// is a no-op, not an error.
	if err := RemoveStateFile(ws, "profile"); err != nil {
		t.Fatalf("RemoveStateFile on already-absent file: %v", err)
	}
	if err := RemoveStateFile(t.TempDir(), "profile"); err != nil {
		t.Fatalf("RemoveStateFile with no .pix dir at all: %v", err)
	}
}

// atomicWriteInDir must land the file with the REQUESTED mode (fchmod'd on
// the open handle BEFORE fsync, so data + metadata are flushed together under
// the intended mode — CreateTemp starts at 0600).
func TestAtomicWriteInDir_AppliesRequestedMode(t *testing.T) {
	dir := t.TempDir()
	for _, perm := range []os.FileMode{0o600, 0o644} {
		name := fmt.Sprintf("f-%o.txt", perm)
		if err := sys.AtomicWriteInDir(dir, name, []byte("data"), perm); err != nil {
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
