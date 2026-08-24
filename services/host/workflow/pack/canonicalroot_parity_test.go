package pack

// canonicalroot_parity_test.go guards a migration-in-progress fact, not a
// permanent architecture: workflow/pack today calls BOTH
// hosttrust.CanonicalRoot (truststore.go, the extracted E1.4 path) and
// packinfo.CanonicalizePackRoot (pack.go, setup.go and the bulk of this
// package's own test fixtures — the legacy, pre-extraction path) for the
// SAME kind of value: a pack root normalized for identity comparison. The
// two are duplicated on purpose (hosttrust is L1 and may not import its L1
// sibling packinfo — see hosttrust/identity.go's doc comment), so nothing
// stops them drifting apart silently the day someone "fixes" one without
// the other. This test is the tripwire: as long as any caller in this
// package still reaches for packinfo.CanonicalizePackRoot, the two
// algorithms MUST keep agreeing on every input, byte for byte.
//
// It lives in workflow/pack (L3), which already imports both L1 packages
// from its own production code (pack.go, truststore.go) — this file adds
// no new import edge, and being a _test.go file it would be exempt from the
// L1-sibling-import rule (arch_test.go's scanPackages) even if it did:
// this is deliberately NOT reproduced as a hosttrust-internal or
// packinfo-internal test, because neither L1 package may import the other
// even from a test importing both would need to, and doing it here instead
// keeps the "no production L1 sibling import" invariant honestly at zero
// production edges added.
//
// Delete this test only once every caller in this package (and its
// fixtures) has migrated onto hosttrust.CanonicalRoot and
// packinfo.CanonicalizePackRoot has no remaining pack-identity caller here.

import (
	"os"
	"path/filepath"
	"testing"

	"pix/host/hosttrust"
	"pix/host/packinfo"
)

// assertCanonicalRootParity is the one place both algorithms are invoked
// side by side, so a future divergence shows up as one failing assertion
// naming the exact input, not a scattered set of near-duplicate checks.
func assertCanonicalRootParity(t *testing.T, input string) {
	t.Helper()
	got := hosttrust.CanonicalRoot(input)
	want := packinfo.CanonicalizePackRoot(input)
	if got != want {
		t.Errorf("hosttrust.CanonicalRoot(%q) = %q, want it to equal packinfo.CanonicalizePackRoot(%q) = %q",
			input, got, input, want)
	}
}

func TestCanonicalRootParity_EmptyAndBlank(t *testing.T) {
	for _, in := range []string{"", "   ", "\t\n"} {
		assertCanonicalRootParity(t, in)
	}
}

func TestCanonicalRootParity_Tilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory available: %v", err)
	}
	for _, in := range []string{"~", "~/", "~/pack", "~/a/b/../b/c", "~/.config/pix/pack"} {
		assertCanonicalRootParity(t, in)
	}
	// Sanity: both algorithms actually DID expand ~, not merely agree on
	// leaving it untouched — a parity test that passed on two unexpanded
	// "~/pack" strings would prove nothing about the expansion behavior.
	got := hosttrust.CanonicalRoot("~/pack")
	if got != filepath.Clean(filepath.Join(home, "pack")) {
		t.Fatalf("hosttrust.CanonicalRoot(%q) = %q, want it under the real home dir %q (expansion did not happen; parity above would be vacuous)", "~/pack", got, home)
	}
}

func TestCanonicalRootParity_RelativeAndRedundantSegments(t *testing.T) {
	for _, in := range []string{
		"pack",
		"./pack",
		"a/b/../b/work",
		"/tmp/x/y/../y/work",
		"/tmp/x//y///work",
		"/tmp/x/./y/work",
		"../pack",
		"/tmp/x/y/work/",
	} {
		assertCanonicalRootParity(t, in)
	}
}

func TestCanonicalRootParity_AlreadyCleanedAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	assertCanonicalRootParity(t, dir)
	assertCanonicalRootParity(t, filepath.Clean(dir))
}

// TestCanonicalRootParity_SymlinkPathItselfIsNotDereferenced proves the two
// algorithms agree even for a path that IS a symlink: neither
// hosttrust.CanonicalRoot nor packinfo.CanonicalizePackRoot calls
// filepath.EvalSymlinks — both are Abs+Clean only — so canonicalizing a
// symlink's OWN path must return that path (textually normalized), never
// its target. Refusing to READ or WRITE through a symlink is a separate
// concern the trust-document I/O owns (hosttrust.ReadDocumentBytes /
// SaveDocument, hosttrust.HashFile); CanonicalRoot's job is identity, and
// identity here is "the path a human typed", not "what it resolves to".
func TestCanonicalRootParity_SymlinkPathItselfIsNotDereferenced(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real-pack")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link-pack")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	assertCanonicalRootParity(t, link)
	assertCanonicalRootParity(t, filepath.Join(link, "..", "link-pack"))

	// Both must report the LINK's own path, not the target it points at.
	if got := hosttrust.CanonicalRoot(link); got != filepath.Clean(link) {
		t.Errorf("hosttrust.CanonicalRoot(%q) = %q, want the symlink's own cleaned path %q (not dereferenced)", link, got, filepath.Clean(link))
	}
	if got := packinfo.CanonicalizePackRoot(link); got != filepath.Clean(link) {
		t.Errorf("packinfo.CanonicalizePackRoot(%q) = %q, want the symlink's own cleaned path %q (not dereferenced)", link, got, filepath.Clean(link))
	}
}

// TestCanonicalRootParity_DanglingSymlinkStillAgrees covers the refusal-
// adjacent case: a symlink whose target does not exist. filepath.Abs+Clean
// never touches the filesystem to resolve it, so a caller downstream that
// decides to REFUSE a dangling or symlinked root (as hosttrust's document
// I/O does for the file it reads/writes, not for the root path itself)
// still needs an agreed-upon identity string to key that refusal by — this
// proves that string is the same from either package even when the target
// is absent.
func TestCanonicalRootParity_DanglingSymlinkStillAgrees(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "dangling-pack")
	if err := os.Symlink(filepath.Join(dir, "does-not-exist"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	assertCanonicalRootParity(t, link)
}
