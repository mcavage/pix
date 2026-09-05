package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/stack"
)

const (
	testStackA = "0123456789abcdef"
	testStackB = "fedcba9876543210"
)

// isolatePixHome points $PIX_HOME at a fresh tempdir, returning its stack
// ID (used by the convenience Name/NameFor wrappers, which resolve
// stack.Current() from $PIX_HOME).
func isolatePixHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	return home
}

// TestNameForStack_Deterministic: the same (stack, workspace), named twice
// (including via a non-canonical relative spelling), yields the identical
// name both times.
func TestNameForStack_Deterministic(t *testing.T) {
	dir := t.TempDir()
	first, err := NameForStack(testStackA, dir, "")
	if err != nil {
		t.Fatalf("NameForStack: %v", err)
	}
	second, err := NameForStack(testStackA, dir, "")
	if err != nil {
		t.Fatalf("NameForStack: %v", err)
	}
	if first != second {
		t.Fatalf("NameForStack(%q) = %q, then %q on a second call — not deterministic", dir, first, second)
	}
	rel := filepath.Join(dir, "..", filepath.Base(dir))
	if got, err := NameForStack(testStackA, rel, ""); err != nil || got != first {
		t.Fatalf("NameForStack(%q) = %q, %v, want %q, nil (same canonical path as %q)", rel, got, err, first, dir)
	}
}

// TestNameForStack_DistinctStacksDiverge: the SAME workspace, named under
// two DIFFERENT stack ids, yields two DIFFERENT names — the whole
// coexistence property this package exists to give: two PIX_HOMEs may name
// the same workspace and never collide.
func TestNameForStack_DistinctStacksDiverge(t *testing.T) {
	dir := t.TempDir()
	nameA, err := NameForStack(testStackA, dir, "")
	if err != nil {
		t.Fatalf("NameForStack(A): %v", err)
	}
	nameB, err := NameForStack(testStackB, dir, "")
	if err != nil {
		t.Fatalf("NameForStack(B): %v", err)
	}
	if nameA == nameB {
		t.Fatalf("NameForStack(%q, %q) and NameForStack(%q, %q) both produced %q — two stacks must never collide on one workspace", testStackA, dir, testStackB, dir, nameA)
	}
	if !strings.Contains(nameA, testStackA) || !strings.Contains(nameB, testStackB) {
		t.Fatalf("expected each name to carry its own stack id: %q / %q", nameA, nameB)
	}
}

// TestName_TwoPixHomesYieldTwoNames is the end-to-end acceptance case: the
// SAME workspace, named through the convenience Name() wrapper under two
// DIFFERENT $PIX_HOME values, yields two different sandbox names.
func TestName_TwoPixHomesYieldTwoNames(t *testing.T) {
	dir := t.TempDir()

	isolatePixHome(t)
	nameA, err := Name(dir)
	if err != nil {
		t.Fatalf("Name under home A: %v", err)
	}

	isolatePixHome(t)
	nameB, err := Name(dir)
	if err != nil {
		t.Fatalf("Name under home B: %v", err)
	}

	if nameA == nameB {
		t.Fatalf("Name(%q) under two different PIX_HOMEs both produced %q", dir, nameA)
	}
}

// TestNameForStack_CollisionFreeAcrossDifferentPaths: two REAL directories
// sharing a basename ("proj") but living under different parents get
// different names within the SAME stack — the whole point of digesting the
// full canonical path rather than the basename alone.
func TestNameForStack_CollisionFreeAcrossDifferentPaths(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a", "proj")
	b := filepath.Join(root, "b", "proj")
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	nameA, err := NameForStack(testStackA, a, "")
	if err != nil {
		t.Fatalf("NameForStack(a): %v", err)
	}
	nameB, err := NameForStack(testStackA, b, "")
	if err != nil {
		t.Fatalf("NameForStack(b): %v", err)
	}
	if nameA == nameB {
		t.Fatalf("NameForStack(%q) and NameForStack(%q) both produced %q — collided", a, b, nameA)
	}
	wantPrefix := Prefix + testStackA + "-proj-"
	if !strings.HasPrefix(nameA, wantPrefix) || !strings.HasPrefix(nameB, wantPrefix) {
		t.Fatalf("expected both names to start with %q, got %q and %q", wantPrefix, nameA, nameB)
	}
}

// TestNameForStack_SanitizesDisallowedCharacters: a basename with
// spaces/punctuation is mapped to a safe charset, never passed through raw.
func TestNameForStack_SanitizesDisallowedCharacters(t *testing.T) {
	root := t.TempDir()
	dirty := filepath.Join(root, "my cool app!!")
	if err := os.MkdirAll(dirty, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	got, err := NameForStack(testStackA, dirty, "")
	if err != nil {
		t.Fatalf("NameForStack: %v", err)
	}
	if strings.ContainsAny(got, " !") {
		t.Fatalf("NameForStack(%q) = %q still contains disallowed characters", dirty, got)
	}
	wantPrefix := Prefix + testStackA + "-my-cool-app-"
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("NameForStack(%q) = %q, want prefix %q", dirty, got, wantPrefix)
	}
}

// TestNameForStack_TruncatesLongBasenamePreservingDigest: an overlong
// basename is truncated, but the 8-hex digest suffix is always the FULL,
// correct digest of the canonical path — truncation never touches it — and
// the whole name still fits MaxNameLen even with the stack-id segment
// added.
func TestNameForStack_TruncatesLongBasenamePreservingDigest(t *testing.T) {
	root := t.TempDir()
	longBase := strings.Repeat("x", 200)
	dir := filepath.Join(root, longBase)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	got, err := NameForStack(testStackA, dir, "")
	if err != nil {
		t.Fatalf("NameForStack: %v", err)
	}
	if len(got) > MaxNameLen {
		t.Fatalf("NameForStack(%q) = %q, length %d exceeds MaxNameLen %d", dir, got, len(got), MaxNameLen)
	}
	canon := canonicalPath(dir)
	wantDigest := pathDigest(canon)
	if !strings.HasSuffix(got, "-"+wantDigest) {
		t.Fatalf("NameForStack(%q) = %q does not end with the expected digest suffix -%s", dir, got, wantDigest)
	}
	if len(wantDigest) != DigestLen {
		t.Fatalf("test setup: digest %q is not %d chars", wantDigest, DigestLen)
	}
}

// TestNameForStack_FallbackForRootPath: NameForStack(id, "/") has an
// empty/degenerate basename ("/".Base() == "/") and must fall back to
// fallbackBase rather than producing a bare "pix-<id>--<digest>".
func TestNameForStack_FallbackForRootPath(t *testing.T) {
	got, err := NameForStack(testStackA, "/", "")
	if err != nil {
		t.Fatalf("NameForStack: %v", err)
	}
	want := Prefix + testStackA + "-" + fallbackBase + "-"
	if !strings.HasPrefix(got, want) {
		t.Fatalf(`NameForStack(id, "/") = %q, want prefix %q`, got, want)
	}
}

// TestNameForStack_RejectsMalformedStackID: a malformed stack id refuses
// rather than silently composing an unscoped or wrongly-scoped name.
func TestNameForStack_RejectsMalformedStackID(t *testing.T) {
	if got, err := NameForStack("not-a-valid-id", t.TempDir(), ""); err == nil {
		t.Fatalf("NameForStack with a malformed id = %q, nil, want an error", got)
	}
}

// TestPathDigest_MatchesStackHashPrefix pins pathDigest's contract against
// stack.HashPrefix directly so a future change to the algorithm is caught,
// not just its length.
func TestPathDigest_MatchesStackHashPrefix(t *testing.T) {
	const canon = "/tmp/example"
	want := stack.HashPrefix(canon, DigestLen)
	if got := pathDigest(canon); got != want {
		t.Fatalf("pathDigest(%q) = %q, want %q", canon, got, want)
	}
}
