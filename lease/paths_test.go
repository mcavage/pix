package lease

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

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
	flags, err := fcntlGetFD(f.Fd())
	if err != nil {
		t.Fatalf("fcntlGetFD: %v", err)
	}
	if flags&syscall.FD_CLOEXEC == 0 {
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
