package stack

import (
	"os"
	"path/filepath"
	"testing"
)

// canonicalpath_test.go pins the one property every stack-scoped name rests
// on: a path has ONE canonical spelling, and that spelling does not change
// when the directory it names is created. CanonicalPath used to swallow an
// EvalSymlinks failure and return the unresolved absolute path, so
// "$PIX_HOME under a symlinked parent, before `pix setup` creates it"
// canonicalized one way and the same home canonicalized the other way
// afterwards. Two different answers for one PIX_HOME is two stack ids, which
// is two sets of runtime resource names for one home.

// TestCanonicalPath_MissingLeafUnderASymlinkedParentIsStableAcrossCreation is
// the regression: the parent is reached through a symlink and the leaf does
// not exist yet. The answer must already be the RESOLVED parent plus the
// missing leaf, and creating the leaf must not change it.
func TestCanonicalPath_MissingLeafUnderASymlinkedParentIsStableAcrossCreation(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported on this filesystem: %v", err)
	}

	home := filepath.Join(link, ".pix")
	before, err := CanonicalPath(home)
	if err != nil {
		t.Fatalf("CanonicalPath(%q) before creation: %v", home, err)
	}
	resolvedReal, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", real, err)
	}
	want := filepath.Join(resolvedReal, ".pix")
	if before != want {
		t.Errorf("CanonicalPath(%q) = %q before creation, want the resolved parent %q", home, before, want)
	}

	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	after, err := CanonicalPath(home)
	if err != nil {
		t.Fatalf("CanonicalPath(%q) after creation: %v", home, err)
	}
	if after != before {
		t.Errorf("creating the directory changed its canonical path: %q -> %q", before, after)
	}
}

// TestCanonicalPath_SeveralMissingComponentsAreAppendedInOrder: more than one
// component can be absent (a home two levels below anything that exists), and
// the missing suffix must survive in order, cleanly joined.
func TestCanonicalPath_SeveralMissingComponentsAreAppendedInOrder(t *testing.T) {
	root := t.TempDir()
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", root, err)
	}
	deep := filepath.Join(root, "a", "b", "c")
	got, err := CanonicalPath(deep)
	if err != nil {
		t.Fatalf("CanonicalPath(%q): %v", deep, err)
	}
	want := filepath.Join(resolvedRoot, "a", "b", "c")
	if got != want {
		t.Errorf("CanonicalPath(%q) = %q, want %q", deep, got, want)
	}
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	after, err := CanonicalPath(deep)
	if err != nil {
		t.Fatalf("CanonicalPath(%q) after creation: %v", deep, err)
	}
	if after != got {
		t.Errorf("creating the directory changed its canonical path: %q -> %q", got, after)
	}
}

// TestCanonicalPath_UnresolvableExistingPathIsAnError: a path that IS there
// but cannot be resolved (a symlink loop) must fail loudly. Returning the
// unresolved spelling would silently mint an identity for a location nobody
// can name.
func TestCanonicalPath_UnresolvableExistingPathIsAnError(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	if err := os.Symlink(b, a); err != nil {
		t.Skipf("symlink unsupported on this filesystem: %v", err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Skipf("symlink unsupported on this filesystem: %v", err)
	}
	if got, err := CanonicalPath(a); err == nil {
		t.Fatalf("CanonicalPath(%q) = %q, nil; want an error for a path that exists and cannot be resolved", a, got)
	}
}

// TestCanonicalPath_UnreadableAncestorIsAnError: the same posture for the
// permission case. Skipped for root, who is refused nothing.
func TestCanonicalPath_UnreadableAncestorIsAnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root traverses a 0000 directory, so there is no permission failure to observe")
	}
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	if err := os.MkdirAll(filepath.Join(locked, "inner"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(locked, 0o700)

	target := filepath.Join(locked, "inner")
	if got, err := CanonicalPath(target); err == nil {
		t.Fatalf("CanonicalPath(%q) = %q, nil; want an error when an ancestor cannot be traversed", target, got)
	}
}

// TestID_StableAcrossHomeCreation is the property that actually matters to a
// user: the stack id of a PIX_HOME that does not exist yet is the id it will
// have once `pix setup` creates it, even when its parent is a symlink.
func TestID_StableAcrossHomeCreation(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported on this filesystem: %v", err)
	}

	home := filepath.Join(link, ".pix")
	before, err := ID(home)
	if err != nil {
		t.Fatalf("ID(%q) before creation: %v", home, err)
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	after, err := ID(home)
	if err != nil {
		t.Fatalf("ID(%q) after creation: %v", home, err)
	}
	if before != after {
		t.Errorf("creating PIX_HOME changed its stack id: %q -> %q", before, after)
	}
	// And the resolved spelling of the same home agrees with both.
	viaReal, err := ID(filepath.Join(real, ".pix"))
	if err != nil {
		t.Fatalf("ID via the real path: %v", err)
	}
	if viaReal != after {
		t.Errorf("the symlinked and resolved spellings of one home disagree: %q vs %q", after, viaReal)
	}
}
