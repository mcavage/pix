// Moved from pack/pack_v2_round3_test.go: subject is the LAUNCH side of the pack boundary.
package main

import (
	"os"
	"path/filepath"
	"testing"

	"pix/host/workflow/doctor"
	"pix/host/workflow/pack"
)

// TestSandboxPackMarker_NotOverwrittenOnInconclusiveProbe: the create-time pack
// marker is persisted state, so it gates on definitelyCreating — an sbxUnknown
// probe (sbx may re-attach the old sandbox) must leave an existing marker
// untouched, while a real create (absent, or --replace) writes it.
func TestSandboxPackMarker_NotOverwrittenOnInconclusiveProbe(t *testing.T) {
	// An unknown state now fails closed before any create preparation or marker
	// write; definitelyCreating must remain false too.
	if willCreate(sbxUnknown, false) {
		t.Fatal("willCreate(sbxUnknown) must fail closed")
	}
	cases := []struct {
		State   doctor.SbxState
		replace bool
		want    bool
	}{
		{sbxAbsent, false, true},
		{sbxUnknown, false, false}, // R3: inconclusive probe never writes
		{sbxRunning, false, false},
		{sbxStopped, false, false},
		// round-4 F3: --replace on an INCONCLUSIVE probe is NOT a definite
		// create — planSandboxLaunch skips the rm on sbxUnknown (RmFirst is
		// false), so sbx may re-attach the old sandbox; the marker must not be
		// overwritten (or stalePackReattachWarning would wrongly go silent).
		{sbxUnknown, true, false},
		{sbxAbsent, true, true},  // absent + replace: rm is a no-op, create is certain
		{sbxRunning, true, true}, // --replace with a positive probe really removes + creates
		{sbxStopped, true, true},
	}
	oldPack := pack.CanonicalizePackRoot(filepath.Join(t.TempDir(), "old-pack"))
	newPack := filepath.Join(t.TempDir(), "new-pack")
	for _, tc := range cases {
		if got := definitelyCreating(tc.State, tc.replace); got != tc.want {
			t.Errorf("definitelyCreating(%v, %v) = %v, want %v", tc.State, tc.replace, got, tc.want)
		}
		// Behavioral: run.go's gate over an existing marker.
		ws := t.TempDir()
		if err := os.MkdirAll(filepath.Join(ws, ".pix"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sandboxPackMarkerPath(ws), []byte(oldPack+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if definitelyCreating(tc.State, tc.replace) { // mirrors runRun's marker gate
			writeSandboxPackMarker(ws, newPack)
		}
		got := readSandboxPackMarker(ws)
		if tc.want && got != pack.CanonicalizePackRoot(newPack) {
			t.Errorf("state=%v replace=%v: a definite create must write the marker, got %q", tc.State, tc.replace, got)
		}
		if !tc.want && got != oldPack {
			t.Errorf("state=%v replace=%v: a non-create must leave the marker untouched, got %q", tc.State, tc.replace, got)
		}
	}
}
