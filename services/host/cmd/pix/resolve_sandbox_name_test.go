package main

import (
	"os"
	"path/filepath"
	"testing"

	"pix/host/sandbox"
)

// resolve_sandbox_name_test.go pins the U04c review fix: the ACTUAL sandbox
// name `pix run` targets by default is sandbox.Name's digest form, not
// workspace.DeriveSandboxName's bare "pix-<basename>" — so two workspaces
// sharing a basename never collide on the sandbox they create/attach to.
// An explicit --name is untouched either way.

// TestResolveSandboxName_ExplicitNameIsVerbatim: --name always wins, and is
// never reshaped through sandbox.Name — a custom name (a legacy non-digest
// name, or one that predates this package) travels exactly as given.
func TestResolveSandboxName_ExplicitNameIsVerbatim(t *testing.T) {
	ws := t.TempDir()
	for _, explicit := range []string{"pix-t", "my-custom-box", "pix-legacy-nodigest"} {
		if got := resolveSandboxName(explicit, ws); got != explicit {
			t.Errorf("resolveSandboxName(%q, ws) = %q, want %q verbatim", explicit, got, explicit)
		}
	}
}

// TestResolveSandboxName_DefaultIsDigestForm: with no explicit --name, the
// resolved name is exactly sandbox.Name(workspace) — the deterministic
// digest identity, not workspace.DeriveSandboxName's plain "pix-<basename>".
func TestResolveSandboxName_DefaultIsDigestForm(t *testing.T) {
	ws := t.TempDir()
	got := resolveSandboxName("", ws)
	want := sandbox.Name(ws)
	if got != want {
		t.Errorf("resolveSandboxName(\"\", %q) = %q, want sandbox.Name's digest form %q", ws, got, want)
	}
	// It must NOT be the plain, digest-less form a basename-only deriver
	// would produce (this is the exact regression the review flagged).
	plain := "pix-" + filepath.Base(ws)
	if got == plain {
		t.Errorf("resolveSandboxName(\"\", %q) = %q, looks like a bare basename default with no digest", ws, got)
	}
}

// TestResolveSandboxName_SameBasenameCollision: two DIFFERENT workspace
// directories that happen to share a basename must resolve to two DIFFERENT
// default sandbox names — the exact case workspace.DeriveSandboxName (bare
// "pix-<basename>") could not distinguish, and the reason SessionName/lease
// state moved to sandbox.Name's digest identity in the first place.
func TestResolveSandboxName_SameBasenameCollision(t *testing.T) {
	parentA := t.TempDir()
	parentB := t.TempDir()
	wsA := filepath.Join(parentA, "myproj")
	wsB := filepath.Join(parentB, "myproj")
	if err := os.MkdirAll(wsA, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wsB, 0o700); err != nil {
		t.Fatal(err)
	}

	nameA := resolveSandboxName("", wsA)
	nameB := resolveSandboxName("", wsB)
	if nameA == nameB {
		t.Fatalf("two different workspaces sharing a basename collided on sandbox name %q", nameA)
	}
	// Both still calling twice for the SAME workspace must be stable.
	if again := resolveSandboxName("", wsA); again != nameA {
		t.Errorf("resolveSandboxName(%q) is not deterministic: %q vs %q", wsA, nameA, again)
	}
}
