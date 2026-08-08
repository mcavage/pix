package provision

import (
	"bytes"
	"strings"
	"testing"
)

// TestEnsureSetupSbxSession_NotSignedInFailsClosedNonInteractive is the sbx
// launch/setup boundary this task's fix must NOT weaken: an sbx-session pack
// binding is now marked structurally Available the moment its pack clears
// Tier-1 trust (workflow/pack.ApplyPackInference), but "Available" is a wiring
// fact, never a claim that Docker Sandboxes is actually signed in and reachable
// right now. That is EnsureSetupSbxSession's job, exercised here against a real
// `sbx` fixture that behaves like a box with no session: `sbx ls` fails, and a
// non-interactive caller (a script, or setup running under --yes) must get a
// named, actionable error rather than sail through on the pack's say-so.
func TestEnsureSetupSbxSession_NotSignedInFailsClosedNonInteractive(t *testing.T) {
	fixtureBin(t, "sbx", "exit 1")
	var out bytes.Buffer
	err := EnsureSetupSbxSession(realEnv(), &out, false)
	if err == nil {
		t.Fatal("an unreachable/not-signed-in sbx must fail non-interactive setup")
	}
	if !strings.Contains(err.Error(), "sbx login") {
		t.Fatalf("error = %v, want it to name the repair command `sbx login`", err)
	}
	if out.Len() != 0 {
		t.Fatalf("a non-interactive failure must not print an interactive prompt, got %q", out.String())
	}
}

// TestEnsureSetupSbxSession_AlreadySignedInIsANoOp: a working `sbx ls` needs no
// login flow and no output — the common case setup runs on every launch.
func TestEnsureSetupSbxSession_AlreadySignedInIsANoOp(t *testing.T) {
	fixtureBin(t, "sbx", "echo ok")
	var out bytes.Buffer
	if err := EnsureSetupSbxSession(realEnv(), &out, true); err != nil {
		t.Fatalf("a reachable, signed-in sbx must not error: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("an already-usable session must not narrate a login flow, got %q", out.String())
	}
}
