package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/sandbox"
	"pix/host/stack"
	"pix/host/workflow/launch"
)

// resolve_sandbox_name_test.go pins the U04c review fix (the ACTUAL sandbox
// name `pix run` targets by default is sandbox.Name's digest form, not a bare
// "pix-<basename>") AND the U3 coexistence fix: resolveSandboxName is ALWAYS
// scoped to this process's current stack, for both the default digest name
// and an explicit --name — there is no bypass of stack scoping through
// either path.

// isolatePixHome points $PIX_HOME (and therefore stack.Current()) at a fresh
// tempdir, returning its stack ID.
func isolatePixHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	id, err := stack.ID(home)
	if err != nil {
		t.Fatalf("stack.ID(%q): %v", home, err)
	}
	return id
}

// TestResolveSandboxName_ExplicitShortNameIsScoped: a short logical --name
// (no "pix-" prefix) is scoped into this stack's own namespace, not passed
// through verbatim.
func TestResolveSandboxName_ExplicitShortNameIsScoped(t *testing.T) {
	id := isolatePixHome(t)
	ws := t.TempDir()
	for _, explicit := range []string{"my-custom-box", "t"} {
		got, err := resolveSandboxName(explicit, ws)
		if err != nil {
			t.Fatalf("resolveSandboxName(%q, ws): %v", explicit, err)
		}
		want, werr := sandbox.ScopeExplicitName(id, explicit)
		if werr != nil {
			t.Fatalf("sandbox.ScopeExplicitName(%q, %q): %v", id, explicit, werr)
		}
		if got != want {
			t.Errorf("resolveSandboxName(%q, ws) = %q, want %q (scoped)", explicit, got, want)
		}
		if got == explicit {
			t.Errorf("resolveSandboxName(%q, ws) = %q, must not travel verbatim (bypasses stack scoping)", explicit, got)
		}
	}
}

// TestResolveSandboxName_ExplicitCurrentStackNameRoundTrips: a full name
// already scoped to THIS stack — e.g. one `pix ls` just printed — round-trips
// unchanged.
func TestResolveSandboxName_ExplicitCurrentStackNameRoundTrips(t *testing.T) {
	id := isolatePixHome(t)
	ws := t.TempDir()
	full := sandbox.Prefix + id + "-myproj-deadbeef"
	got, err := resolveSandboxName(full, ws)
	if err != nil {
		t.Fatalf("resolveSandboxName(%q, ws): %v", full, err)
	}
	if got != full {
		t.Errorf("resolveSandboxName(%q, ws) = %q, want it unchanged", full, got)
	}
}

// TestResolveSandboxName_ExplicitForeignStackNameRefused: a full name scoped
// to a DIFFERENT stack (or the pre-scoping legacy grammar, which carries no
// stack-id segment at all) is refused, never silently adopted.
func TestResolveSandboxName_ExplicitForeignStackNameRefused(t *testing.T) {
	isolatePixHome(t)
	ws := t.TempDir()
	for _, foreign := range []string{
		sandbox.Prefix + "fedcba9876543210-myproj-deadbeef",
		"pix-legacy-nodigest",
	} {
		if got, err := resolveSandboxName(foreign, ws); err == nil {
			t.Errorf("resolveSandboxName(%q, ws) = %q, nil, want an error (foreign/unscoped stack name)", foreign, got)
		}
	}
}

// TestResolveSandboxName_ExplicitUnsafeFormRefused: an argv-unsafe --name is
// refused outright.
func TestResolveSandboxName_ExplicitUnsafeFormRefused(t *testing.T) {
	isolatePixHome(t)
	ws := t.TempDir()
	for _, bad := range []string{" ", "has spaces", "a/b", "$(inject)"} {
		if got, err := resolveSandboxName(bad, ws); err == nil {
			t.Errorf("resolveSandboxName(%q, ws) = %q, nil, want an error", bad, got)
		}
	}
}

// TestResolveSandboxName_DefaultIsDigestForm: with no explicit --name, the
// resolved name is exactly sandbox.Name(workspace)'s deterministic digest
// identity, not a plain "pix-<basename>".
func TestResolveSandboxName_DefaultIsDigestForm(t *testing.T) {
	isolatePixHome(t)
	ws := t.TempDir()
	got, err := resolveSandboxName("", ws)
	if err != nil {
		t.Fatalf("resolveSandboxName: %v", err)
	}
	want, werr := sandbox.Name(ws)
	if werr != nil {
		t.Fatalf("sandbox.Name: %v", werr)
	}
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

// TestResolveSandboxName_TwoPixHomesYieldTwoNames: the SAME workspace,
// resolved (default, no --name) under two different $PIX_HOME values,
// derives two DIFFERENT sandbox names — the coexistence property this unit
// exists to give.
func TestResolveSandboxName_TwoPixHomesYieldTwoNames(t *testing.T) {
	ws := t.TempDir()

	isolatePixHome(t)
	nameA, err := resolveSandboxName("", ws)
	if err != nil {
		t.Fatalf("resolveSandboxName under home A: %v", err)
	}

	isolatePixHome(t)
	nameB, err := resolveSandboxName("", ws)
	if err != nil {
		t.Fatalf("resolveSandboxName under home B: %v", err)
	}

	if nameA == nameB {
		t.Fatalf("resolveSandboxName(%q) under two different PIX_HOMEs both produced %q", ws, nameA)
	}
}

// TestResolveSandboxName_SameBasenameCollision: two DIFFERENT workspace
// directories that happen to share a basename must resolve to two DIFFERENT
// default sandbox names.
func TestResolveSandboxName_SameBasenameCollision(t *testing.T) {
	isolatePixHome(t)
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

	nameA, err := resolveSandboxName("", wsA)
	if err != nil {
		t.Fatalf("resolveSandboxName: %v", err)
	}
	nameB, err := resolveSandboxName("", wsB)
	if err != nil {
		t.Fatalf("resolveSandboxName: %v", err)
	}
	if nameA == nameB {
		t.Fatalf("two different workspaces sharing a basename collided on sandbox name %q", nameA)
	}
	// Both still calling twice for the SAME workspace must be stable.
	if again, err := resolveSandboxName("", wsA); err != nil || again != nameA {
		t.Errorf("resolveSandboxName(%q) is not deterministic: %q vs %q, err=%v", wsA, nameA, again, err)
	}
}

// TestSessionKeyFor_IsTheFinalResolvedName: lease identity follows the
// resolved sandbox name, not the raw workspace, in both the default (digest)
// case and the explicit --name case.
func TestSessionKeyFor_IsTheFinalResolvedName(t *testing.T) {
	isolatePixHome(t)
	ws := t.TempDir()
	defaultName, err := resolveSandboxName("", ws)
	if err != nil {
		t.Fatalf("resolveSandboxName: %v", err)
	}
	if got := sessionKeyFor(launch.RunOpts{Name: defaultName, Workspace: ws}); got != defaultName {
		t.Errorf("sessionKeyFor(default) = %q, want the resolved digest name %q", got, defaultName)
	}
	explicitName, err := resolveSandboxName("my-custom-box", ws)
	if err != nil {
		t.Fatalf("resolveSandboxName: %v", err)
	}
	if got := sessionKeyFor(launch.RunOpts{Name: explicitName, Workspace: ws}); got != explicitName {
		t.Errorf("sessionKeyFor(explicit) = %q, want %q", got, explicitName)
	}
}

// TestSessionKeyFor_DifferentExplicitNamesOnOneWorkspaceNeverCollide: the
// exact regression this fix closes. Section [3] and [4] of the host UAT
// script launch multiple DIFFERENTLY NAMED sandboxes from the SAME workspace
// directory; keying lease state by the workspace path (the old behavior)
// would alias their lease directories onto one another the instant that
// happens. Keying by the resolved sandbox name (what sbx itself treats as
// the unique identity) cannot alias, because sbx names are unique by
// construction.
func TestSessionKeyFor_DifferentExplicitNamesOnOneWorkspaceNeverCollide(t *testing.T) {
	isolatePixHome(t)
	ws := t.TempDir()
	nameA, err := resolveSandboxName("pix-uat-one", ws)
	if err == nil {
		// "pix-uat-one" has no stack-id segment, so it is a foreign/legacy
		// name and correctly refused; fall back to a short logical name for
		// the rest of this collision check.
		t.Fatalf("resolveSandboxName(%q) unexpectedly succeeded with %q; legacy pix-* names must be refused", "pix-uat-one", nameA)
	}
	nameA, err = resolveSandboxName("uat-one", ws)
	if err != nil {
		t.Fatalf("resolveSandboxName: %v", err)
	}
	nameB, err := resolveSandboxName("uat-two", ws)
	if err != nil {
		t.Fatalf("resolveSandboxName: %v", err)
	}
	keyA := sessionKeyFor(launch.RunOpts{Name: nameA, Workspace: ws})
	keyB := sessionKeyFor(launch.RunOpts{Name: nameB, Workspace: ws})
	if keyA == keyB {
		t.Fatalf("two differently-named sandboxes sharing workspace %q collided on lease key %q", ws, keyA)
	}
	if keyA != nameA || keyB != nameB {
		t.Errorf("sessionKeyFor did not track the resolved name: keyA=%q nameA=%q keyB=%q nameB=%q", keyA, nameA, keyB, nameB)
	}
}

// TestResolveSandboxName_LongDefaultNameFits: a workspace with a very long
// basename still resolves to a name within sandbox.MaxNameLen, stack-id
// segment included.
func TestResolveSandboxName_LongDefaultNameFits(t *testing.T) {
	isolatePixHome(t)
	root := t.TempDir()
	ws := filepath.Join(root, strings.Repeat("x", 200))
	if err := os.MkdirAll(ws, 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := resolveSandboxName("", ws)
	if err != nil {
		t.Fatalf("resolveSandboxName: %v", err)
	}
	if len(got) > sandbox.MaxNameLen {
		t.Fatalf("resolveSandboxName(%q) = %q, length %d exceeds MaxNameLen %d", ws, got, len(got), sandbox.MaxNameLen)
	}
}
