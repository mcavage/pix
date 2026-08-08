//go:build unix

package lease

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestEnsureSandboxDir_CreatesThenIsIdempotent (Story04c): the exported
// wrapper is the SAME guarded path CreateRecord uses internally — a caller
// that only needs the directory to exist (Open, SetKeep) gets the identical
// TOCTOU-safe, symlink-refusing create, and calling it twice is a no-op.
func TestEnsureSandboxDir_CreatesThenIsIdempotent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sess")
	if err := EnsureSandboxDir(dir); err != nil {
		t.Fatalf("EnsureSandboxDir: %v", err)
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		t.Fatalf("expected %s to exist as a dir, err=%v", dir, err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("mode = %v, want 0700", fi.Mode().Perm())
	}
	if err := EnsureSandboxDir(dir); err != nil {
		t.Fatalf("second EnsureSandboxDir call: %v", err)
	}
}

// TestEnsureSandboxDir_RefusesSymlink: the same symlink refusal CreateRecord
// gets must apply here too — a caller must never be handed a directory that
// is actually a symlink to somewhere else.
func TestEnsureSandboxDir_RefusesSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "elsewhere")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "sess")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSandboxDir(link); err == nil {
		t.Fatal("expected a symlink at the sandbox dir path to be refused")
	}
}

func TestValidateInstanceID_AcceptsSafeIDs(t *testing.T) {
	for _, id := range []string{"a", "abc123", "sbx-1", "sbx_1.2", strings.Repeat("a", 128)} {
		if err := ValidateInstanceID(id); err != nil {
			t.Errorf("ValidateInstanceID(%q) = %v, want nil", id, err)
		}
	}
}

func TestValidateInstanceID_RejectsUnsafeIDs(t *testing.T) {
	cases := []string{
		"",
		"..",
		".",
		"../etc",
		"a/../../etc/passwd",
		"a/b",
		"a\\b",
		"/etc/passwd",
		".hidden",
		"has space",
		"has$dollar",
		strings.Repeat("a", 129),
	}
	for _, id := range cases {
		if err := ValidateInstanceID(id); err == nil {
			t.Errorf("ValidateInstanceID(%q) = nil, want error", id)
		}
	}
}

func TestSandboxDir_JoinsUnderRoot(t *testing.T) {
	root := t.TempDir()
	dir, err := SandboxDir(root, "sbx-1")
	if err != nil {
		t.Fatalf("SandboxDir: %v", err)
	}
	want := filepath.Join(root, "sbx-1")
	if dir != want {
		t.Errorf("SandboxDir = %q, want %q", dir, want)
	}
}

func TestSandboxDir_RefusesTraversal(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"../escape", "a/../../escape", ".."} {
		if _, err := SandboxDir(root, id); err == nil {
			t.Errorf("SandboxDir(root, %q) = nil error, want refusal", id)
		}
	}
}

func TestSandboxDir_RefusesSymlinkComponent(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "sbx-1")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if err := ensureSandboxDir(link); err == nil {
		t.Error("ensureSandboxDir on a pre-existing symlink = nil, want refusal")
	}
}

func TestOpenNoFollow_RefusesSymlinkLeaf(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if _, err := openNoFollow(link, syscall.O_RDONLY, 0); err == nil {
		t.Error("openNoFollow on a symlink = nil error, want refusal")
	}
}

func TestOpenNoFollow_SetsCLOEXEC(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "f")
	f, err := openNoFollow(path, syscall.O_WRONLY|syscall.O_CREAT, 0o600)
	if err != nil {
		t.Fatalf("openNoFollow: %v", err)
	}
	defer f.Close()
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), uintptr(syscall.F_GETFD), 0)
	if errno != 0 {
		t.Fatalf("F_GETFD: %v", errno)
	}
	if int(flags)&syscall.FD_CLOEXEC == 0 {
		t.Error("openNoFollow fd is not close-on-exec")
	}
}

func TestEnsureSandboxDir_Creates0700(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sbx-1")
	if err := ensureSandboxDir(dir); err != nil {
		t.Fatalf("ensureSandboxDir: %v", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir perm = %o, want 0700", perm)
	}
}
