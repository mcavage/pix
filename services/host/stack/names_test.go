package stack

import (
	"testing"
)

const testID = "0123456789abcdef"

// TestNameHelpers_Grammar pins the exact literal shape every naming helper
// produces for a valid id — the resource-name grammar the contract names.
func TestNameHelpers_Grammar(t *testing.T) {
	cases := []struct {
		name string
		fn   func(string) (string, error)
		want string
	}{
		{"SandboxPrefix", SandboxPrefix, "pix-" + testID + "-"},
		{"MemoryContainerName", MemoryContainerName, "pix-memory-" + testID},
		{"MCPMemoryName", MCPMemoryName, "pix-memory-" + testID},
		{"MCPSessionName", MCPSessionName, "pix-session-" + testID},
		{"LocalTemplateTagPrefix", LocalTemplateTagPrefix, "local-" + testID + "-"},
	}
	for _, c := range cases {
		got, err := c.fn(testID)
		if err != nil {
			t.Errorf("%s(%q) error = %v", c.name, testID, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s(%q) = %q, want %q", c.name, testID, got, c.want)
		}
	}

	// MCPMemoryName and MCPSessionName must never collide with each other, or
	// with MemoryContainerName reusing a session's slot, for the same id.
	if mem, _ := MCPMemoryName(testID); mem == "" {
		t.Fatal("MCPMemoryName returned empty string")
	}
	if sess, _ := MCPSessionName(testID); sess == "" {
		t.Fatal("MCPSessionName returned empty string")
	}
	mem, _ := MCPMemoryName(testID)
	sess, _ := MCPSessionName(testID)
	if mem == sess {
		t.Fatalf("MCPMemoryName and MCPSessionName both produced %q for id %q", mem, testID)
	}
}

// TestNameHelpers_DistinctIDsDiverge: two different stack ids never produce
// the same name from the same helper — the whole point of scoping.
func TestNameHelpers_DistinctIDsDiverge(t *testing.T) {
	otherID := "fedcba9876543210"
	fns := map[string]func(string) (string, error){
		"SandboxPrefix":          SandboxPrefix,
		"MemoryContainerName":    MemoryContainerName,
		"MCPMemoryName":          MCPMemoryName,
		"MCPSessionName":         MCPSessionName,
		"LocalTemplateTagPrefix": LocalTemplateTagPrefix,
	}
	for name, fn := range fns {
		a, err := fn(testID)
		if err != nil {
			t.Fatalf("%s(%q) error = %v", name, testID, err)
		}
		b, err := fn(otherID)
		if err != nil {
			t.Fatalf("%s(%q) error = %v", name, otherID, err)
		}
		if a == b {
			t.Errorf("%s produced %q for both %q and %q", name, a, testID, otherID)
		}
	}
}

// TestNameHelpers_RejectMalformedID: every naming helper VALIDATES id and
// returns an error rather than composing an unsafe name or falling back to
// a global (unscoped) name.
func TestNameHelpers_RejectMalformedID(t *testing.T) {
	malformed := []string{
		"",
		"too-short",
		"UPPERCASE0123456",
		"has spaces here!",
		"../../../etc/passwd",
		testID + "x", // one character too long
	}
	fns := map[string]func(string) (string, error){
		"SandboxPrefix":          SandboxPrefix,
		"MemoryContainerName":    MemoryContainerName,
		"MCPMemoryName":          MCPMemoryName,
		"MCPSessionName":         MCPSessionName,
		"LocalTemplateTagPrefix": LocalTemplateTagPrefix,
	}
	for name, fn := range fns {
		for _, id := range malformed {
			if got, err := fn(id); err == nil {
				t.Errorf("%s(%q) = %q, nil — want an error, not a silently composed name", name, id, got)
			}
		}
	}
}

// TestLocalTemplateTag_Grammar: LocalTemplateTag composes
// "local-<id>-<stamp>" for a valid id and a safe stamp.
func TestLocalTemplateTag_Grammar(t *testing.T) {
	got, err := LocalTemplateTag(testID, "1700000000")
	if err != nil {
		t.Fatalf("LocalTemplateTag error = %v", err)
	}
	want := "local-" + testID + "-1700000000"
	if got != want {
		t.Errorf("LocalTemplateTag(%q, %q) = %q, want %q", testID, "1700000000", got, want)
	}
}

// TestLocalTemplateTag_RejectsMalformedInput covers both halves of the
// composed name: a malformed id and a malformed (argv-unsafe) stamp must
// each be refused, never silently sanitized or dropped.
func TestLocalTemplateTag_RejectsMalformedInput(t *testing.T) {
	if _, err := LocalTemplateTag("not-a-valid-id", "1700000000"); err == nil {
		t.Error("LocalTemplateTag with a malformed id: got nil error, want one")
	}
	for _, stamp := range []string{"", "has spaces", "../escape", "semi;colon", "$(inject)"} {
		if got, err := LocalTemplateTag(testID, stamp); err == nil {
			t.Errorf("LocalTemplateTag(%q, %q) = %q, nil — want an error for an unsafe stamp", testID, stamp, got)
		}
	}
}

// TestIsScopedSandboxName: the predicate matches exactly this stack's own
// SandboxPrefix, and rejects a name scoped to a DIFFERENT stack, an
// unscoped legacy pix-* name, a non-pix name, and the bare prefix itself
// (no basename/digest suffix is not a real sandbox name).
func TestIsScopedSandboxName(t *testing.T) {
	prefix, err := SandboxPrefix(testID)
	if err != nil {
		t.Fatalf("SandboxPrefix error = %v", err)
	}
	scoped := prefix + "myproj-deadbeef"
	if !IsScopedSandboxName(testID, scoped) {
		t.Errorf("IsScopedSandboxName(%q, %q) = false, want true", testID, scoped)
	}

	otherID := "fedcba9876543210"
	if IsScopedSandboxName(otherID, scoped) {
		t.Errorf("IsScopedSandboxName(%q, %q) = true, want false (name scoped to a DIFFERENT stack)", otherID, scoped)
	}

	legacy := "pix-myproj-deadbeef" // pre-scoping global name, no stack id segment
	if IsScopedSandboxName(testID, legacy) {
		t.Errorf("IsScopedSandboxName(%q, %q) = true, want false (unscoped legacy name)", testID, legacy)
	}

	if IsScopedSandboxName(testID, "not-pix-at-all") {
		t.Error("IsScopedSandboxName matched a name outside the pix- namespace entirely")
	}

	if IsScopedSandboxName(testID, prefix) {
		t.Errorf("IsScopedSandboxName(%q, %q) = true, want false (bare prefix, no suffix)", testID, prefix)
	}

	if IsScopedSandboxName("not-a-valid-id", scoped) {
		t.Error("IsScopedSandboxName with a malformed id matched a real scoped name")
	}
}

// TestIsAnyPixSandboxName: the coarser predicate matches ANY pix-owned
// name — scoped to any stack, or the pre-scoping unscoped grammar — and
// rejects everything outside the pix- namespace, including the bare prefix.
func TestIsAnyPixSandboxName(t *testing.T) {
	scoped, err := SandboxPrefix(testID)
	if err != nil {
		t.Fatalf("SandboxPrefix error = %v", err)
	}
	cases := map[string]bool{
		scoped + "myproj-deadbeef": true,
		"pix-myproj-deadbeef":      true, // pre-scoping legacy grammar
		"pix-x":                    true,
		"pix-":                     false, // bare prefix, no suffix
		"":                         false,
		"not-pix-at-all":           false,
		"prefix-pix-myproj":        false, // "pix-" must be a PREFIX, not merely a substring
	}
	for name, want := range cases {
		if got := IsAnyPixSandboxName(name); got != want {
			t.Errorf("IsAnyPixSandboxName(%q) = %v, want %v", name, got, want)
		}
	}
}
