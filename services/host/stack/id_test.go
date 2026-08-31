package stack

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// hexIDRe pins the exact grammar the tests below hold ID/Current to: 16
// lowercase hex characters, nothing else.
var hexIDRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

// TestID_StableAcrossPathSpellings: the same PIX_HOME, named via an
// absolute path, a non-canonical relative spelling, and a symlink to it,
// all produce the IDENTICAL stack ID.
func TestID_StableAcrossPathSpellings(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	want, err := ID(home)
	if err != nil {
		t.Fatalf("ID(%q) error = %v", home, err)
	}

	// Non-canonical relative spelling of the same directory.
	rel := filepath.Join(home, "..", filepath.Base(home))
	if got, err := ID(rel); err != nil {
		t.Fatalf("ID(%q) error = %v", rel, err)
	} else if got != want {
		t.Errorf("ID(%q) = %q, want %q (same canonical path as %q)", rel, got, want, home)
	}

	// A symlink to the same directory.
	link := filepath.Join(root, "home-link")
	if err := os.Symlink(home, link); err != nil {
		t.Skipf("symlink unsupported on this filesystem: %v", err)
	}
	if got, err := ID(link); err != nil {
		t.Fatalf("ID(%q) error = %v", link, err)
	} else if got != want {
		t.Errorf("ID(%q) = %q, want %q (symlink to the same PIX_HOME)", link, got, want)
	}
}

// TestID_DistinctHomesDiverge: two different PIX_HOME roots (even sharing a
// basename) produce two different IDs.
func TestID_DistinctHomesDiverge(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a", ".pix")
	b := filepath.Join(root, "b", ".pix")
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	idA, err := ID(a)
	if err != nil {
		t.Fatalf("ID(%q) error = %v", a, err)
	}
	idB, err := ID(b)
	if err != nil {
		t.Fatalf("ID(%q) error = %v", b, err)
	}
	if idA == idB {
		t.Fatalf("ID(%q) and ID(%q) both produced %q — distinct PIX_HOME roots must diverge", a, b, idA)
	}
}

// TestID_Grammar: ID always returns exactly IDLen lowercase hex characters,
// and IDLen is 16 as the contract states.
func TestID_Grammar(t *testing.T) {
	if IDLen != 16 {
		t.Fatalf("IDLen = %d, want 16", IDLen)
	}
	home := t.TempDir()
	id, err := ID(home)
	if err != nil {
		t.Fatalf("ID(%q) error = %v", home, err)
	}
	if len(id) != IDLen {
		t.Fatalf("ID(%q) = %q has length %d, want %d", home, id, len(id), IDLen)
	}
	if !hexIDRe.MatchString(id) {
		t.Fatalf("ID(%q) = %q does not match %s", home, id, hexIDRe.String())
	}
	if strings.ToLower(id) != id {
		t.Fatalf("ID(%q) = %q is not all-lowercase", home, id)
	}
}

// TestValidID: the grammar ValidID enforces is exactly the one ID produces —
// no more permissive, no less.
func TestValidID(t *testing.T) {
	home := t.TempDir()
	id, err := ID(home)
	if err != nil {
		t.Fatalf("ID(%q) error = %v", home, err)
	}
	if err := ValidID(id); err != nil {
		t.Errorf("ValidID(%q) error = %v, want nil (this is exactly what ID produces)", id, err)
	}

	for _, bad := range []string{
		"",
		"short",
		strings.Repeat("a", IDLen+1),
		strings.ToUpper(id),                // uppercase hex must be rejected
		id[:IDLen-1] + "g",                 // non-hex character
		id[:IDLen-1] + " ",                 // whitespace
		"../../../etc/passwd" + id[:IDLen], // path-shaped junk, still wrong length
	} {
		if err := ValidID(bad); err == nil {
			t.Errorf("ValidID(%q) = nil, want an error", bad)
		}
	}
}

// TestCurrent_DerivesFromPixhomeDir: Current() agrees with ID(pixhome
// resolution) for an explicit $PIX_HOME, and changing $PIX_HOME changes the
// answer.
func TestCurrent_DerivesFromPixhomeDir(t *testing.T) {
	homeA := t.TempDir()
	homeB := t.TempDir()

	t.Setenv("PIX_HOME", homeA)
	wantA, err := ID(homeA)
	if err != nil {
		t.Fatalf("ID(%q) error = %v", homeA, err)
	}
	gotA, err := Current()
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	if gotA != wantA {
		t.Errorf("Current() = %q with $PIX_HOME=%q, want %q", gotA, homeA, wantA)
	}

	t.Setenv("PIX_HOME", homeB)
	wantB, err := ID(homeB)
	if err != nil {
		t.Fatalf("ID(%q) error = %v", homeB, err)
	}
	gotB, err := Current()
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	if gotB != wantB {
		t.Errorf("Current() = %q with $PIX_HOME=%q, want %q", gotB, homeB, wantB)
	}
	if gotA == gotB {
		t.Fatalf("Current() returned the same id (%q) for two different $PIX_HOME values", gotA)
	}
}

// TestCurrent_XDGIndependence: setting every XDG_* variable to a decoy
// location must have zero effect on Current()'s answer — the same
// no-XDG-fallback invariant pixhome.Dir itself pins
// (pixhome.TestDir_NoXDGFallback), proven again here through this
// package's own derived identity.
func TestCurrent_XDGIndependence(t *testing.T) {
	home := t.TempDir()
	decoy := t.TempDir()

	t.Setenv("PIX_HOME", "")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", decoy)
	t.Setenv("XDG_DATA_HOME", decoy)
	t.Setenv("XDG_STATE_HOME", decoy)
	t.Setenv("XDG_CACHE_HOME", decoy)

	want, err := ID(filepath.Join(home, ".pix"))
	if err != nil {
		t.Fatalf("ID error = %v", err)
	}
	got, err := Current()
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	if got != want {
		t.Errorf("Current() = %q, want %q (XDG_* must never influence stack identity)", got, want)
	}
}
