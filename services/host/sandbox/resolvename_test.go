package sandbox

import (
	"strings"
	"testing"
)

// TestScopeExplicitName_ShortLogicalNameIsScoped: a bare, safe token with no
// "pix-" prefix is scoped into this stack's own namespace.
func TestScopeExplicitName_ShortLogicalNameIsScoped(t *testing.T) {
	got, err := ScopeExplicitName(testStackA, "myproj")
	if err != nil {
		t.Fatalf("ScopeExplicitName: %v", err)
	}
	want := Prefix + testStackA + "-myproj"
	if got != want {
		t.Fatalf("ScopeExplicitName(%q, %q) = %q, want %q", testStackA, "myproj", got, want)
	}
}

// TestScopeExplicitName_CurrentStackFullNameRoundTrips: a full name already
// scoped to stackID (e.g. one `pix ls` just printed) travels verbatim.
func TestScopeExplicitName_CurrentStackFullNameRoundTrips(t *testing.T) {
	full := Prefix + testStackA + "-myproj-deadbeef"
	got, err := ScopeExplicitName(testStackA, full)
	if err != nil {
		t.Fatalf("ScopeExplicitName: %v", err)
	}
	if got != full {
		t.Fatalf("ScopeExplicitName(%q, %q) = %q, want it unchanged", testStackA, full, got)
	}
}

// TestScopeExplicitName_ForeignStackFullNameRefused: a full name scoped to
// a DIFFERENT stack is refused outright — never silently rescoped, never
// accepted as an attach target across stacks.
func TestScopeExplicitName_ForeignStackFullNameRefused(t *testing.T) {
	foreign := Prefix + testStackB + "-myproj-deadbeef"
	if got, err := ScopeExplicitName(testStackA, foreign); err == nil {
		t.Fatalf("ScopeExplicitName(%q, %q) = %q, nil, want an error (foreign stack)", testStackA, foreign, got)
	}
}

// TestScopeExplicitName_LegacyUnscopedFullNameRefused: the pre-scoping
// "pix-<basename>-<digest>" grammar carries no stack-id segment at all, so
// it is indistinguishable from (and must be treated as) a foreign/unowned
// name — refused, not silently adopted into the current stack.
func TestScopeExplicitName_LegacyUnscopedFullNameRefused(t *testing.T) {
	legacy := "pix-myproj-deadbeef"
	if got, err := ScopeExplicitName(testStackA, legacy); err == nil {
		t.Fatalf("ScopeExplicitName(%q, %q) = %q, nil, want an error (unscoped legacy name)", testStackA, legacy, got)
	}
}

// TestScopeExplicitName_RefusesUnsafeForms: anything outside the safe argv
// charset is refused regardless of whether it happens to start with "pix-".
func TestScopeExplicitName_RefusesUnsafeForms(t *testing.T) {
	for _, bad := range []string{
		"",
		" ",
		"has spaces",
		"../escape",
		"semi;colon",
		"$(inject)",
		"a/b",
		"pix-" + testStackA + "-has spaces",
	} {
		if got, err := ScopeExplicitName(testStackA, bad); err == nil {
			t.Errorf("ScopeExplicitName(%q, %q) = %q, nil, want an error", testStackA, bad, got)
		}
	}
}

// TestScopeExplicitName_RejectsMalformedStackID: a malformed stack id
// refuses regardless of what was requested.
func TestScopeExplicitName_RejectsMalformedStackID(t *testing.T) {
	if got, err := ScopeExplicitName("not-a-valid-id", "myproj"); err == nil {
		t.Fatalf("ScopeExplicitName with a malformed id = %q, nil, want an error", got)
	}
}

// TestScopeExplicitName_LongNameFits: an overlong short logical name is
// truncated to fit MaxNameLen rather than producing an oversized sandbox
// name sbx itself would refuse.
func TestScopeExplicitName_LongNameFits(t *testing.T) {
	got, err := ScopeExplicitName(testStackA, strings.Repeat("n", 200))
	if err != nil {
		t.Fatalf("ScopeExplicitName: %v", err)
	}
	if len(got) > MaxNameLen {
		t.Fatalf("ScopeExplicitName produced %q, length %d exceeds MaxNameLen %d", got, len(got), MaxNameLen)
	}
	if !strings.HasPrefix(got, Prefix+testStackA+"-") {
		t.Fatalf("ScopeExplicitName truncated form %q lost its stack scope", got)
	}
}

// TestNameForStack_EnvVariesTheDigest proves two environments on one
// workspace derive two different names within the same stack, and that
// naming is stable across non-canonical spellings of the same workspace.
func TestNameForStack_EnvVariesTheDigest(t *testing.T) {
	a, err := NameForStack(testStackA, "/home/u/proj", "work")
	if err != nil {
		t.Fatalf("NameForStack: %v", err)
	}
	b, err := NameForStack(testStackA, "/home/u/./proj", "work")
	if err != nil {
		t.Fatalf("NameForStack: %v", err)
	}
	c, err := NameForStack(testStackA, "/home/u/proj", "home")
	if err != nil {
		t.Fatalf("NameForStack: %v", err)
	}
	if a != b {
		t.Fatalf("the same workspace+env must derive one name: %s vs %s", a, b)
	}
	if a == c {
		t.Fatalf("two environments on one workspace must not share a sandbox: %s", a)
	}
	if !strings.HasPrefix(a, Prefix+testStackA+"-") {
		t.Fatalf("derived names stay stack-scoped, got %s", a)
	}
}
