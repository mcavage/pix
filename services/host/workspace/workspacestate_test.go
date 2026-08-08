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

// ReadStateFile round-trips exactly what WriteStateFile wrote, and a missing
// marker or missing .pix dir is a clean os.IsNotExist, not a hard error.
func TestReadWorkspaceStateFileNormal(t *testing.T) {
	ws := t.TempDir()
	if err := WriteStateFile(ws, "profile", []byte("acme\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := ReadStateFile(ws, "profile")
	if err != nil {
		t.Fatalf("ReadStateFile: %v", err)
	}
	if string(b) != "acme\n" {
		t.Fatalf("content = %q, want %q", b, "acme\n")
	}

	if _, err := ReadStateFile(ws, "no-such-marker"); !os.IsNotExist(err) {
		t.Fatalf("missing marker: err = %v, want IsNotExist", err)
	}
	if _, err := ReadStateFile(t.TempDir(), "profile"); !os.IsNotExist(err) {
		t.Fatalf("missing .pix dir: err = %v, want IsNotExist", err)
	}
}

// ReadStateFile must refuse a symlinked .pix DIRECTORY outright — the read-side
// mirror of TestRemoveWorkspaceStateFileSymlinkedDirRefused: a hostile clone
// must not have its .pix point somewhere ReadStateFile will happily read.
func TestReadWorkspaceStateFileSymlinkedDirRefused(t *testing.T) {
	ws := t.TempDir()
	target := t.TempDir()
	const secret = "do not read through the symlink\n"
	if err := os.WriteFile(filepath.Join(target, "profile"), []byte(secret), 0o644); err != nil {
		t.Fatal(err)
	}
	RequireSymlink(t, target, filepath.Join(ws, ".pix"))

	if _, err := ReadStateFile(ws, "profile"); err == nil {
		t.Fatal("ReadStateFile through a symlinked .pix dir: want error, got nil")
	}
}

// ReadStateFile must refuse a symlinked state FILE too, even when .pix itself
// is a real directory — a hostile TRACKED symlink at .pix/profile must not be
// followed to whatever it points at.
func TestReadWorkspaceStateFileSymlinkedFileRefused(t *testing.T) {
	ws := t.TempDir()
	stateDir := filepath.Join(ws, ".pix")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "victim")
	const secret = "do not read through the symlink\n"
	if err := os.WriteFile(target, []byte(secret), 0o644); err != nil {
		t.Fatal(err)
	}
	RequireSymlink(t, target, filepath.Join(stateDir, "profile"))

	if _, err := ReadStateFile(ws, "profile"); err == nil {
		t.Fatal("ReadStateFile through a symlinked state file: want error, got nil")
	}
}

// ReadStateFile must refuse a state file SWAPPED for a symlink via an atomic
// rename, not just one created directly as a symlink from the start (the case
// TestReadWorkspaceStateFileSymlinkedFileRefused already covers). This is the
// shape a real TOCTOU attacker's substitution takes: the destination existed
// as an ordinary file a moment ago and is a symlink now, with the change
// landing in one atomic filesystem operation. The unix build's O_NOFOLLOW at
// open(2) has no Lstat-then-open window at all for that substitution to land
// in, regardless of when it happens; this proves the refusal holds whether
// the symlink was there from the start or arrived by a later swap.
func TestReadWorkspaceStateFileSymlinkSwapRefused(t *testing.T) {
	ws := t.TempDir()
	if err := WriteStateFile(ws, "profile", []byte("acme\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "victim")
	const secret = "do not read through the swapped symlink\n"
	if err := os.WriteFile(target, []byte(secret), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(ws, ".pix", "profile")
	swapLink := dest + ".swap"
	RequireSymlink(t, target, swapLink)
	if err := os.Rename(swapLink, dest); err != nil {
		t.Fatal(err)
	}

	if _, err := ReadStateFile(ws, "profile"); err == nil {
		t.Fatal("ReadStateFile after a rename-swapped symlink: want error, got nil")
	}
	// The swapped-in symlink itself must be untouched (still a symlink, still
	// pointing at target) — a refusal must not "fix" the swap by removing it.
	fi, err := os.Lstat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the swapped symlink was replaced or removed by a refused read")
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
