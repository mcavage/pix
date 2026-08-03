// Moved from pack/pack_v2_round3_test.go: subject is the LAUNCH side of the pack boundary.
package main

import (
	"os"
	"path/filepath"
	"testing"

	"pix/host/workflow/doctor"
	"pix/host/workflow/launch"
	"pix/host/workflow/pack"
)

// TestSandboxPackMarker_NotOverwrittenOnInconclusiveProbe: the create-time pack
// marker is persisted state, so it gates on launch.DefinitelyCreating — an launch.SbxUnknown
// probe (sbx may re-attach the old sandbox) must leave an existing marker
// untouched, while a real create (absent, or --replace) writes it.
func TestSandboxPackMarker_NotOverwrittenOnInconclusiveProbe(t *testing.T) {
	// An unknown state now fails closed before any create preparation or marker
	// write; launch.DefinitelyCreating must remain false too.
	if launch.WillCreate(launch.SbxUnknown, false) {
		t.Fatal("launch.WillCreate(launch.SbxUnknown) must fail closed")
	}
	cases := []struct {
		State   doctor.SbxState
		replace bool
		want    bool
	}{
		{launch.SbxAbsent, false, true},
		{launch.SbxUnknown, false, false}, // R3: inconclusive probe never writes
		{launch.SbxRunning, false, false},
		{launch.SbxStopped, false, false},
		// round-4 F3: --replace on an INCONCLUSIVE probe is NOT a definite
		// create — launch.PlanSandboxLaunch skips the rm on launch.SbxUnknown (RmFirst is
		// false), so sbx may re-attach the old sandbox; the marker must not be
		// overwritten (or launch.StalePackReattachWarning would wrongly go silent).
		{launch.SbxUnknown, true, false},
		{launch.SbxAbsent, true, true},  // absent + replace: rm is a no-op, create is certain
		{launch.SbxRunning, true, true}, // --replace with a positive probe really removes + creates
		{launch.SbxStopped, true, true},
	}
	oldPack := pack.CanonicalizePackRoot(filepath.Join(t.TempDir(), "old-pack"))
	newPack := filepath.Join(t.TempDir(), "new-pack")
	for _, tc := range cases {
		if got := launch.DefinitelyCreating(tc.State, tc.replace); got != tc.want {
			t.Errorf("launch.DefinitelyCreating(%v, %v) = %v, want %v", tc.State, tc.replace, got, tc.want)
		}
		// Behavioral: run.go's gate over an existing marker.
		ws := t.TempDir()
		if err := os.MkdirAll(filepath.Join(ws, ".pix"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(launch.SandboxPackMarkerPath(ws), []byte(oldPack+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if launch.DefinitelyCreating(tc.State, tc.replace) { // mirrors runRun's marker gate
			launch.WriteSandboxPackMarker(ws, newPack)
		}
		got := launch.ReadSandboxPackMarker(ws)
		if tc.want && got != pack.CanonicalizePackRoot(newPack) {
			t.Errorf("state=%v replace=%v: a definite create must write the marker, got %q", tc.State, tc.replace, got)
		}
		if !tc.want && got != oldPack {
			t.Errorf("state=%v replace=%v: a non-create must leave the marker untouched, got %q", tc.State, tc.replace, got)
		}
	}
}
