package launch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/envinfo"
	"pix/host/lease"
)

// The pure decision half of safe recreation is covered in recreate_test.go.
// This file covers the half that reads the REAL host state a launch has:
// the lease directory's keep marker and reference lock, the workspace mode,
// and the removal that a decided plan actually performs. Those are the
// facts the first cutover commit left unwired, so a template-only drift
// still refused with "not recreated automatically: ...".

func TestWorkspaceModeFor(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		in    string
		want  WorkspaceMode
		block bool
	}{
		{"host directory is a direct mount", dir, WorkspaceDirect, false},
		{"missing path is unknown", filepath.Join(dir, "nope"), WorkspaceUnknown, true},
		{"a file is not a workspace", file, WorkspaceUnknown, true},
		{"empty is unknown", "", WorkspaceUnknown, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := WorkspaceModeFor(tc.in)
			if got != tc.want {
				t.Fatalf("WorkspaceModeFor(%q) = %v, want %v", tc.in, got, tc.want)
			}
			p := RecreateProof{FreshListing: true, Holders: KnownHolders(0), Workspace: got}
			if p.Authorizes() == tc.block {
				t.Fatalf("workspace mode %v: Authorizes() = %v", got, p.Authorizes())
			}
		})
	}
}

func TestRecreateProofFor_IdleRecordedSandboxAuthorizes(t *testing.T) {
	isolateState(t)
	key := "pix-demo"
	seedRecordedSession(t, key, "inst-1")

	proof := RecreateProofFor(key, t.TempDir(), true)
	if !proof.Authorizes() {
		t.Fatalf("idle recorded sandbox did not authorize recreation: %v", proof.blockers())
	}
	if !proof.Holders.Zero() {
		t.Errorf("holders = %+v, want a positive zero census", proof.Holders)
	}
}

func TestRecreateProofFor_KeepMarkerBlocks(t *testing.T) {
	isolateState(t)
	key := "pix-demo"
	dir := seedRecordedSession(t, key, "inst-1")
	if err := lease.SetKeep(dir, "inst-1"); err != nil {
		t.Fatal(err)
	}

	proof := RecreateProofFor(key, t.TempDir(), true)
	if proof.Authorizes() {
		t.Fatalf("a kept sandbox must never be recreated automatically")
	}
	if !hasBlocker(proof, "keep marker") {
		t.Fatalf("blockers = %v, want the keep marker named", proof.blockers())
	}
}

func TestRecreateProofFor_LiveHolderBlocks(t *testing.T) {
	isolateState(t)
	key := "pix-demo"
	dir := seedRecordedSession(t, key, "inst-1")

	ref, err := lease.AttachRefUnderLifecycle(context.Background(), dir, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer ref.Close()

	proof := RecreateProofFor(key, t.TempDir(), true)
	if proof.Authorizes() {
		t.Fatalf("a sandbox somebody still holds must never be recreated automatically")
	}
	if !hasBlocker(proof, "hold it") {
		t.Fatalf("blockers = %v, want the live holder named", proof.blockers())
	}
}

// A key with no lease state at all cannot prove anything about holders, so
// it fails closed rather than reading "no refs directory" as "nobody is
// using it".
func TestRecreateProofFor_NoLeaseStateFailsClosed(t *testing.T) {
	isolateState(t)

	proof := RecreateProofFor("pix-never-created", t.TempDir(), true)
	if proof.Authorizes() {
		t.Fatalf("unreadable lease state authorized a recreate: %+v", proof)
	}
}

// A listing that was not re-read on this launch is not liveness proof, even
// when everything else is clean.
func TestRecreateProofFor_StaleListingBlocks(t *testing.T) {
	isolateState(t)
	key := "pix-demo"
	seedRecordedSession(t, key, "inst-1")

	proof := RecreateProofFor(key, t.TempDir(), false)
	if proof.Authorizes() {
		t.Fatalf("a stale listing authorized a recreate: %+v", proof)
	}
}

func TestExecuteRecreate_RemovesUnderTheOrdinaryProofGate(t *testing.T) {
	isolateState(t)
	fixture := installFakeSbx(t, removableFixture)
	key := "pix-demo"
	seedRecordedSession(t, key, "inst-1")

	plan := &RecreatePlan{SandboxName: "pix-demo", InstanceID: "inst-1", Reason: "pinned template changed"}
	if err := ExecuteRecreate(realEnv(), plan, key, fastTeardown(t)); err != nil {
		t.Fatalf("ExecuteRecreate: %v", err)
	}
	var sawRm bool
	for _, l := range argvLines(t, fixture) {
		if strings.HasPrefix(l, "rm") {
			sawRm = true
			if l != "rm -f pix-demo" {
				t.Errorf("removal argv = %q, want the ordinary scoped removal %q", l, "rm -f pix-demo")
			}
		}
	}
	if !sawRm {
		t.Fatalf("no removal reached sbx")
	}
}

// The proof is re-taken INSIDE teardown, so a holder that appears between
// the decision and the removal still stops it — and the sandbox survives.
func TestExecuteRecreate_HeldSandboxIsNotRemoved(t *testing.T) {
	isolateState(t)
	fixture := installFakeSbx(t, removableFixture)
	key := "pix-demo"
	dir := seedRecordedSession(t, key, "inst-1")
	ref, err := lease.AttachRefUnderLifecycle(context.Background(), dir, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer ref.Close()

	plan := &RecreatePlan{SandboxName: "pix-demo", InstanceID: "inst-1", Reason: "pinned template changed"}
	err = ExecuteRecreate(realEnv(), plan, key, fastTeardown(t))
	if err == nil {
		t.Fatalf("ExecuteRecreate removed a held sandbox")
	}
	for _, l := range sbxArgv(t, fixture) {
		if strings.HasPrefix(l, "rm") {
			t.Fatalf("a removal reached sbx despite a live holder: %q", l)
		}
	}
}

// A plan whose name escaped the pix-* namespace, or that lost its recorded
// instance id, never reaches a removal at all.
func TestExecuteRecreate_ValidatesBeforeMutating(t *testing.T) {
	isolateState(t)
	fixture := installFakeSbx(t, removableFixture)
	seedRecordedSession(t, "pix-demo", "inst-1")

	for _, plan := range []*RecreatePlan{
		{SandboxName: "not-pix", InstanceID: "inst-1"},
		{SandboxName: "pix-demo", InstanceID: ""},
	} {
		if err := ExecuteRecreate(realEnv(), plan, "pix-demo", fastTeardown(t)); err == nil {
			t.Fatalf("ExecuteRecreate(%+v) did not refuse", plan)
		}
	}
	if argv := sbxArgv(t, fixture); len(argv) != 0 {
		t.Fatalf("an unvalidated plan reached sbx: %v", argv)
	}
}

// The end-to-end decision a `pix run` makes after a Pix image upgrade: the
// pinned template is the ONLY drifted facet, the host state is idle, and the
// gate returns a plan instead of the manual-recovery refusal.
func TestDecideEnvAttach_UsesRealHostProof(t *testing.T) {
	isolateState(t)
	key := "pix-proj-abcd1234"
	seedRecordedSession(t, key, "inst-1")
	stored, current := templateDrift()

	g := gateFor(stored, current, RecreateProofFor(key, t.TempDir(), true))
	d := DecideEnvAttach(g, key, "work")
	if d.Recreate == nil {
		t.Fatalf("no recreate plan; refusal was: %s", d.Refusal)
	}
	if d.Recreate.SandboxName != key || d.Recreate.InstanceID != "inst-1" {
		t.Errorf("plan = %+v, want the recorded name and instance", d.Recreate)
	}
	if len(d.Drifts) == 0 || !envinfo.RecreationSafe(d.Drifts) {
		t.Errorf("drifts = %v, want a recreation-safe set", d.Drifts)
	}
}

func hasBlocker(p RecreateProof, want string) bool {
	for _, b := range p.blockers() {
		if strings.Contains(b, want) {
			return true
		}
	}
	return false
}
