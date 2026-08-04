package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestName_Deterministic: the same workspace, named twice (including via a
// non-canonical relative spelling), yields the identical name both times.
func TestName_Deterministic(t *testing.T) {
	dir := t.TempDir()
	first := Name(dir)
	second := Name(dir)
	if first != second {
		t.Fatalf("Name(%q) = %q, then %q on a second call — not deterministic", dir, first, second)
	}
	rel := filepath.Join(dir, "..", filepath.Base(dir))
	if got := Name(rel); got != first {
		t.Fatalf("Name(%q) = %q, want %q (same canonical path as %q)", rel, got, first, dir)
	}
}

// TestName_CollisionFreeAcrossDifferentPaths: two REAL directories sharing a
// basename ("proj") but living under different parents get different names —
// the whole point of digesting the full canonical path rather than the
// basename alone.
func TestName_CollisionFreeAcrossDifferentPaths(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a", "proj")
	b := filepath.Join(root, "b", "proj")
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	nameA, nameB := Name(a), Name(b)
	if nameA == nameB {
		t.Fatalf("Name(%q) and Name(%q) both produced %q — collided", a, b, nameA)
	}
	if !strings.HasPrefix(nameA, "pix-proj-") || !strings.HasPrefix(nameB, "pix-proj-") {
		t.Fatalf("expected both names to start with pix-proj-, got %q and %q", nameA, nameB)
	}
}

// TestName_SanitizesDisallowedCharacters: a basename with spaces/punctuation
// is mapped to a safe charset, never passed through raw.
func TestName_SanitizesDisallowedCharacters(t *testing.T) {
	root := t.TempDir()
	dirty := filepath.Join(root, "my cool app!!")
	if err := os.MkdirAll(dirty, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	got := Name(dirty)
	if strings.ContainsAny(got, " !") {
		t.Fatalf("Name(%q) = %q still contains disallowed characters", dirty, got)
	}
	if !strings.HasPrefix(got, "pix-my-cool-app-") {
		t.Fatalf("Name(%q) = %q, want prefix pix-my-cool-app-", dirty, got)
	}
}

// TestName_TruncatesLongBasenamePreservingDigest: an overlong basename is
// truncated, but the 8-hex digest suffix is always the FULL, correct digest
// of the canonical path — truncation never touches it.
func TestName_TruncatesLongBasenamePreservingDigest(t *testing.T) {
	root := t.TempDir()
	longBase := strings.Repeat("x", 200)
	dir := filepath.Join(root, longBase)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	got := Name(dir)
	if len(got) > MaxNameLen {
		t.Fatalf("Name(%q) = %q, length %d exceeds MaxNameLen %d", dir, got, len(got), MaxNameLen)
	}
	canon := canonicalPath(dir)
	wantDigest := pathDigest(canon)
	if !strings.HasSuffix(got, "-"+wantDigest) {
		t.Fatalf("Name(%q) = %q does not end with the expected digest suffix -%s", dir, got, wantDigest)
	}
	if len(wantDigest) != DigestLen {
		t.Fatalf("test setup: digest %q is not %d chars", wantDigest, DigestLen)
	}
}

// TestName_FallbackForRootPath: Name("/") has an empty/degenerate basename
// ("/".Base() == "/") and must fall back to fallbackBase rather than
// producing a bare "pix--<digest>".
func TestName_FallbackForRootPath(t *testing.T) {
	got := Name("/")
	if !strings.HasPrefix(got, "pix-"+fallbackBase+"-") {
		t.Fatalf(`Name("/") = %q, want prefix "pix-%s-"`, got, fallbackBase)
	}
}

// TestPathDigest_MatchesRawSHA256Prefix pins pathDigest's contract against a
// hand-computed sha256 so a future change to the algorithm is caught, not
// just its length.
func TestPathDigest_MatchesRawSHA256Prefix(t *testing.T) {
	const canon = "/tmp/example"
	sum := sha256.Sum256([]byte(canon))
	want := hex.EncodeToString(sum[:])[:DigestLen]
	if got := pathDigest(canon); got != want {
		t.Fatalf("pathDigest(%q) = %q, want %q", canon, got, want)
	}
}
