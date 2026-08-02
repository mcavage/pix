// Moved from pack/pack_v2_test.go: subject is the LAUNCH side of the pack boundary.
package main

import (
	"testing"

	"pix/host/config"
)

// TestBuildSbxArgs_PackKits_NeverSuppressesBaseKit: PackKits must stack
// alongside the base git/local kit pin, unlike o.Kits (the escape hatch) which
// replaces it. This guards the ADR-2 deviation (see docs deviation note).
func TestBuildSbxArgs_PackKits_NeverSuppressesBaseKit(t *testing.T) {
	cfg := &config.Config{}
	args := buildSbxArgs(cfg, runOpts{Workspace: ".", PackKits: []string{"/pack/kit"}}, "0.0.99")
	if pinnedGitKit(args) == "" {
		t.Errorf("PackKits must not suppress the base git kit pin, got %v", args)
	}
	if !contains(args, []string{"--kit", "/pack/kit"}) {
		t.Errorf("pack kit missing from %v", args)
	}
}

// TestPackCapabilitiesJSON_LoadedAndMounted: a pack's capabilities.json is
// discovered by pack.LoadPack and mounted by pack.SynthesizePackKit into the sandbox at
// files/home/.pi/agent/capabilities.json — even with no [[proxy]] entries. This
// is what lets a pack carry its own capability routing.
