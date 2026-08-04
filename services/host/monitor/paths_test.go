package monitor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSlugifyReplacesUnsafeCharsAndCapsLength(t *testing.T) {
	cases := map[string]string{
		"sbx-1":            "sbx-1",
		"../../etc/passwd": ".._.._etc_passwd",
		"a/b\\c":           "a_b_c",
		"":                 "_",
		".":                "_",
		"..":               "_",
		"...":              "_",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
	huge := strings.Repeat("a", 500)
	if got := slugify(huge); len(got) > 80 {
		t.Errorf("slugify(huge) length = %d, want <= 80", len(got))
	}
}

func TestSlugifyNeverProducesTraversalShape(t *testing.T) {
	for _, in := range []string{"..", "../..", "....//....", "/", "\\", "..\\.."} {
		got := slugify(in)
		if got == ".." || got == "." || strings.Contains(got, "/") || strings.Contains(got, "\\") {
			t.Errorf("slugify(%q) = %q, still traversal-shaped", in, got)
		}
	}
}

func TestStreamDirNameDistinguishesPairsThatSlugifyIdentically(t *testing.T) {
	// Two wire-supplied ids that reduce to the SAME slug (all unsafe bytes)
	// must still land in different directories.
	a := streamDirName("///", "sess")
	b := streamDirName("***", "sess")
	if a == b {
		t.Fatalf("streamDirName collided for distinct inputs that slugify identically: %q", a)
	}
}

func TestStreamDirNameIsDeterministic(t *testing.T) {
	if streamDirName("sbx-1", "sess-1") != streamDirName("sbx-1", "sess-1") {
		t.Fatal("streamDirName is not deterministic for the same inputs")
	}
}

func TestBlobPathRejectsNonHexHash(t *testing.T) {
	for _, bad := range []string{"", "not-a-hash", strings.Repeat("g", 64), strings.Repeat("a", 63), "../../../etc/passwd"} {
		if _, err := blobPath(t.TempDir(), bad); err == nil {
			t.Errorf("blobPath(%q) = nil error, want a rejection", bad)
		}
	}
}

func TestBlobPathAcceptsValidHash(t *testing.T) {
	hash := strings.Repeat("a", 64)
	root := t.TempDir()
	p, err := blobPath(root, hash)
	if err != nil {
		t.Fatalf("blobPath: %v", err)
	}
	if !strings.HasPrefix(p, root) {
		t.Errorf("blobPath = %q, want it under root %q", p, root)
	}
}

// TestEnsureDir0700CreatesWithExactPerms proves a fresh directory is 0700,
// not the process umask default.
func TestEnsureDir0700CreatesWithExactPerms(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sub", "nested")
	if err := ensureDir0700(dir); err != nil {
		t.Fatalf("ensureDir0700: %v", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %o, want 0700", fi.Mode().Perm())
	}
}

// TestEnsureDir0700TightensLoosePerms proves a pre-existing, too-permissive
// directory gets chmod'd back down rather than left as-is.
func TestEnsureDir0700TightensLoosePerms(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod setup: %v", err)
	}
	if err := ensureDir0700(dir); err != nil {
		t.Fatalf("ensureDir0700: %v", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %o, want 0700 after tightening", fi.Mode().Perm())
	}
}

// TestEnsureDir0700RefusesSymlink proves a symlink planted at the target
// path is refused rather than followed/created-through.
func TestEnsureDir0700RefusesSymlink(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatalf("mkdir real: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := ensureDir0700(link); err == nil {
		t.Fatal("ensureDir0700(symlink) = nil error, want a refusal")
	}
}

// TestOpenAppend0600CreatesWithExactPerms proves a freshly created stream
// file is 0600, not the process umask default.
func TestOpenAppend0600CreatesWithExactPerms(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.ndjson")
	f, err := openAppend0600(path)
	if err != nil {
		t.Fatalf("openAppend0600: %v", err)
	}
	f.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %o, want 0600", fi.Mode().Perm())
	}
}

func TestOpenAppend0600RefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.ndjson")
	if err := os.WriteFile(real, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write real: %v", err)
	}
	link := filepath.Join(dir, "link.ndjson")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := openAppend0600(link); err == nil {
		t.Fatal("openAppend0600(symlink) = nil error, want a refusal")
	}
}

func TestWriteFileAtomic0600RefusesSymlinkAndWritesExactPerms(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")
	if err := writeFileAtomic0600(path, []byte("hello")); err != nil {
		t.Fatalf("writeFileAtomic0600: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "hello" {
		t.Fatalf("read back = %q, %v, want %q, nil", got, err, "hello")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %o, want 0600", fi.Mode().Perm())
	}

	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := writeFileAtomic0600(link, []byte("evil")); err == nil {
		t.Fatal("writeFileAtomic0600(symlink) = nil error, want a refusal")
	}
	// The symlink's target must be untouched by the refused write.
	got, err = os.ReadFile(path)
	if err != nil || string(got) != "hello" {
		t.Fatalf("symlink target was modified: %q, %v", got, err)
	}
}
