// Moved from pack/pack_v2_review_test.go: subject is the LAUNCH side of the pack boundary.
package main

import (
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/workflow/launch"
	"pix/host/workflow/pack"
)

// TestStalePackReattachWarning_FiresWhenCreateTimePackDiffers (finding G): the
// warning keys on the CREATE-TIME marker vs the active pack — it fires when
// they differ (a pack switched since create, or removed via `pack rm`), and
// names both sides.
func TestStalePackReattachWarning_FiresWhenCreateTimePackDiffers(t *testing.T) {
	ws := t.TempDir()
	oldRoot := filepath.Join(t.TempDir(), "old-pack")
	newRoot := filepath.Join(t.TempDir(), "new-pack")
	launch.WriteSandboxPackMarker(ws, oldRoot)

	// Switched pack since create: marker != active -> warn.
	cfg := &config.Config{Pack: newRoot}
	msg := launch.StalePackReattachWarning(cfg, launch.RunOpts{Workspace: ws}, true)
	if msg == "" {
		t.Fatal("expected a stale-pack warning when the create-time pack differs from the active pack")
	}
	if !strings.Contains(msg, pack.CanonicalizePackRoot(oldRoot)) || !strings.Contains(msg, pack.CanonicalizePackRoot(newRoot)) || !strings.Contains(msg, "--replace") {
		t.Errorf("warning should name both packs and the fix, got: %q", msg)
	}

	// `pack rm` case: marker set, active pack EMPTY -> still warn (the old
	// sandbox keeps the removed pack's create-time facets).
	msgRm := launch.StalePackReattachWarning(&config.Config{}, launch.RunOpts{Workspace: ws}, true)
	if msgRm == "" {
		t.Fatal("expected a warning after pack rm (marker set, active pack empty)")
	}
	if !strings.Contains(msgRm, pack.CanonicalizePackRoot(oldRoot)) || !strings.Contains(msgRm, "--replace") {
		t.Errorf("rm-case warning should name the create-time pack and the fix, got: %q", msgRm)
	}
}

// TestStalePackReattachWarning_SilentWhenNoMarkerOrIdentical (finding G): no
// marker (a pre-marker or pack-less sandbox) or an identical marker (the
// sandbox already carries the active pack) must NOT warn — the identical case
// is the false positive the marker exists to kill.

// TestStalePackReattachWarning_SilentWhenNoMarkerOrIdentical (finding G): no
// marker (a pre-marker or pack-less sandbox) or an identical marker (the
// sandbox already carries the active pack) must NOT warn — the identical case
// is the false positive the marker exists to kill.
func TestStalePackReattachWarning_SilentWhenNoMarkerOrIdentical(t *testing.T) {
	root := filepath.Join(t.TempDir(), "work")
	cfg := &config.Config{Pack: root}

	// No marker in the workspace: silent, even with an active pack.
	if msg := launch.StalePackReattachWarning(cfg, launch.RunOpts{Workspace: t.TempDir()}, true); msg != "" {
		t.Errorf("no marker must not warn, got %q", msg)
	}

	// Marker matches the active pack: the sandbox already has it — silent.
	ws := t.TempDir()
	launch.WriteSandboxPackMarker(ws, root)
	if msg := launch.StalePackReattachWarning(cfg, launch.RunOpts{Workspace: ws}, true); msg != "" {
		t.Errorf("marker == active pack must not warn (false positive), got %q", msg)
	}

	// Create / --replace paths never warn, marker or not.
	if msg := launch.StalePackReattachWarning(cfg, launch.RunOpts{Workspace: ws}, false); msg != "" {
		t.Errorf("a create/first-launch (reattaching=false) must not warn, got %q", msg)
	}
	launch.WriteSandboxPackMarker(ws, filepath.Join(t.TempDir(), "other"))
	if msg := launch.StalePackReattachWarning(cfg, launch.RunOpts{Workspace: ws, Replace: true}, true); msg != "" {
		t.Errorf("--replace recreates, so must not warn, got %q", msg)
	}

	// Marker removed when the sandbox is created pack-less: silent afterwards.
	launch.WriteSandboxPackMarker(ws, "")
	if msg := launch.StalePackReattachWarning(&config.Config{}, launch.RunOpts{Workspace: ws}, true); msg != "" {
		t.Errorf("pack-less create removes the marker, so no warning, got %q", msg)
	}
}
