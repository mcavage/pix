package main

import (
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/pixhome"
	"pix/host/release"
	"pix/host/workflow/provision"
)

// unselectHome clears config.toml's DefaultEnvironment field, preserving
// every sibling — the same load-modify-save-under-lock shape
// config.SetDefaultEnvironmentAt itself uses.
func unselectHome(t *testing.T, home string) {
	t.Helper()
	if err := config.WithLockAt(home, func(c *config.Config) error {
		c.DefaultEnvironment = ""
		return nil
	}); err != nil {
		t.Fatalf("unselectHome: %v", err)
	}
}

// setup_selection_test.go proves the Round-7 fix end to end at the REAL
// `pix run` selection boundary: a fresh host that only ever ran the
// scaffolding half of `pix setup` (provision.EnsureDefaultEnvironment,
// exactly what provision.Setup calls) resolves `pix run`'s bare (no
// --env) selection to that SAME scaffolded environment — never `none` —
// because EnsureDefaultEnvironment now also selects it, atomically, under
// the config lock (baseline.go).

func selectionTestManifest() release.Manifest {
	digest := "sha256:" + strings.Repeat("a", 64)
	return release.Manifest{Version: "2.0.0", PixAgentDigest: digest, PixMemoryDigest: digest, RuntimeDigest: digest, KitRevision: "k1"}
}

func TestFreshSetupThenRun_SelectsTheScaffoldedDefaultEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)

	created, err := provision.EnsureDefaultEnvironment(pixhome.New(home), selectionTestManifest())
	if err != nil {
		t.Fatalf("EnsureDefaultEnvironment: %v", err)
	}
	if !created {
		t.Fatalf("created = false, want true on a fresh PIX_HOME")
	}

	// The exact call `pix run` (no --env) makes to resolve its environment
	// (run_env.go's resolveRunEnvironment): an explicit name would win, but
	// an empty one must now resolve through config.Config.DefaultEnvironment
	// to the environment setup just scaffolded, never fall through to D17's
	// `none`.
	sel, _, err := resolveRunEnvironment("")
	if err != nil {
		t.Fatalf("resolveRunEnvironment(\"\"): %v", err)
	}
	if sel.Name != provision.DefaultEnvironmentName {
		t.Fatalf("selection.Name = %q, want %q — a fresh setup must select its own scaffold, not guess or stay unselected", sel.Name, provision.DefaultEnvironmentName)
	}
	if !sel.Selected() {
		t.Fatalf("selection.Selected() = false, want true")
	}
}

func TestFreshSetupThenRun_ExistingEnvironmentWithNoDefaultStaysUnselected(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)

	// A host that already has an environment (created outside setup's own
	// scaffolding, e.g. by hand) gets nothing guessed for it: `pix run`
	// (no --env) resolves to D17's `none`, and the doctor remedy
	// (`pix env default NAME`) is the only path to a selection — never a
	// silent inference from "there happens to be exactly one".
	if _, err := provision.EnsureDefaultEnvironment(pixhome.New(home), selectionTestManifest()); err != nil {
		t.Fatalf("EnsureDefaultEnvironment: %v", err)
	}
	// Simulate the "environment exists, default never recorded" state a
	// hand-restored config.toml could leave behind.
	unselectHome(t, home)

	sel, _, err := resolveRunEnvironment("")
	if err != nil {
		t.Fatalf("resolveRunEnvironment(\"\"): %v", err)
	}
	if sel.Selected() {
		t.Fatalf("selection = %+v, want unselected: no default is recorded and pix must never guess one", sel)
	}
}
