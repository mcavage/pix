// Moved from pack/pack_v2_round2_test.go: subject is the LAUNCH side of the pack boundary.
package main

import (
	"path/filepath"
	"testing"

	"pix/host/workflow/launch"
	"pix/host/workflow/pack"
)

// TestSandboxPackMarker_RoundTrip: the marker records the canonicalized pack
// root at create, reads back identically, and an empty pack removes it.
func TestSandboxPackMarker_RoundTrip(t *testing.T) {
	ws := t.TempDir()
	root := filepath.Join(t.TempDir(), "work")
	launch.WriteSandboxPackMarker(ws, root)
	if got := launch.ReadSandboxPackMarker(ws); got != pack.CanonicalizePackRoot(root) {
		t.Errorf("marker round-trip = %q, want %q", got, pack.CanonicalizePackRoot(root))
	}
	launch.WriteSandboxPackMarker(ws, "") // pack-less create removes it
	if got := launch.ReadSandboxPackMarker(ws); got != "" {
		t.Errorf("pack-less create must remove the marker, got %q", got)
	}
}
