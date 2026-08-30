package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/envinfo"
	"pix/host/lease"
	"pix/host/sandbox"
	"pix/host/workflow/launch"
)

// The command layer's half of safe automatic recreation: `pix run` must hand
// DecideEnvAttach a REAL proof, or the whole decision layer is inert and an
// ordinary Pix image upgrade refuses forever with a manual `pix rm && pix
// run`. That is exactly what shipped in the first cutover commit, so these
// tests drive the gate builder run actually uses.

func seedRunSandbox(t *testing.T, key, instanceID string) string {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("PIX_IDENTITY", "test@fixture")
	state, err := config.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(state, "sandboxes", key)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := lease.CreateRecord(dir, instanceID); err != nil {
		t.Fatal(err)
	}
	return dir
}

func runningEntry(name, instanceID string) *sandbox.Entry {
	return &sandbox.Entry{
		Name:             name,
		State:            sandbox.StateRunning,
		InstanceID:       &instanceID,
		IdentityVerified: true,
	}
}

// pinnedBuildDrift is the ONLY drift an ordinary Pix upgrade produces: the
// pinned agent image moved, nothing the user authored changed.
func pinnedBuildDrift() (stored, current sandbox.Fingerprint) {
	stored = sandbox.Fingerprint{"sandboxOptions.template": "pix:0.0.15", "env.PIX_MEMORY_SCOPE": "work"}
	current = sandbox.Fingerprint{"sandboxOptions.template": "pix:0.0.16", "env.PIX_MEMORY_SCOPE": "work"}
	return
}

func baseGate(stored, current sandbox.Fingerprint) launch.AttachGate {
	return launch.AttachGate{
		RecordedInstanceID: "inst-1",
		Stored:             stored,
		StoredFound:        true,
		Current:            current,
		Reviewed:           true,
		Tree:               &envinfo.Tree{},
	}
}

func TestAttachGateFor_PinnedBuildDriftOnAnIdleSandboxRecreates(t *testing.T) {
	key := "pix-proj-abcd1234"
	seedRunSandbox(t, key, "inst-1")
	stored, current := pinnedBuildDrift()

	g := attachGateFor(key, t.TempDir(), runningEntry(key, "inst-1"), baseGate(stored, current))
	d := launch.DecideEnvAttach(g, key, "work")

	if d.Attach {
		t.Fatalf("attached to a sandbox built from a different image")
	}
	if d.Recreate == nil {
		t.Fatalf("run did not decide to recreate; refusal was:\n%s", d.Refusal)
	}
	if d.Recreate.SandboxName != key || d.Recreate.InstanceID != "inst-1" {
		t.Errorf("plan = %+v, want the resolved name and the recorded instance", d.Recreate)
	}
}

// Every non-Pix-owned drift still refuses with the manual sequence, whatever
// the host state is.
func TestAttachGateFor_AuthoredDriftStillRefuses(t *testing.T) {
	key := "pix-proj-abcd1234"
	seedRunSandbox(t, key, "inst-1")
	stored := sandbox.Fingerprint{"env.API_BASE": "old"}
	current := sandbox.Fingerprint{"env.API_BASE": "new"}

	d := launch.DecideEnvAttach(
		attachGateFor(key, t.TempDir(), runningEntry(key, "inst-1"), baseGate(stored, current)), key, "work")

	if d.Recreate != nil {
		t.Fatalf("an authored env-var drift was recreated automatically")
	}
	if !strings.Contains(d.Refusal, "pix rm "+key) {
		t.Errorf("refusal did not print the manual sequence:\n%s", d.Refusal)
	}
}

// A keep marker is the user saying "this box stays". It blocks the automatic
// path and the blocker is named in the refusal.
func TestAttachGateFor_KeptSandboxRefusesAndNamesTheBlocker(t *testing.T) {
	key := "pix-proj-abcd1234"
	dir := seedRunSandbox(t, key, "inst-1")
	if err := lease.SetKeep(dir, "inst-1"); err != nil {
		t.Fatal(err)
	}
	stored, current := pinnedBuildDrift()

	d := launch.DecideEnvAttach(
		attachGateFor(key, t.TempDir(), runningEntry(key, "inst-1"), baseGate(stored, current)), key, "work")

	if d.Recreate != nil {
		t.Fatalf("a kept sandbox was recreated automatically")
	}
	if !strings.Contains(d.Refusal, "keep marker") {
		t.Errorf("refusal did not name the keep marker:\n%s", d.Refusal)
	}
}

// A workspace that is not positively a host directory cannot be proven to be
// a direct mount, so the automatic path fails closed.
func TestAttachGateFor_UnprovableWorkspaceRefuses(t *testing.T) {
	key := "pix-proj-abcd1234"
	seedRunSandbox(t, key, "inst-1")
	stored, current := pinnedBuildDrift()

	d := launch.DecideEnvAttach(
		attachGateFor(key, filepath.Join(t.TempDir(), "gone"), runningEntry(key, "inst-1"), baseGate(stored, current)),
		key, "work")

	if d.Recreate != nil {
		t.Fatalf("recreated a sandbox whose workspace mode could not be determined")
	}
	if !strings.Contains(d.Refusal, "workspace mode could not be determined") {
		t.Errorf("refusal did not name the workspace blocker:\n%s", d.Refusal)
	}
}
