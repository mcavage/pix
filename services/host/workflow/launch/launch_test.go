package launch

import (
	"strings"
	"testing"

	"errors"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/sys/systest"
)

// These are the launch behaviours the slim must not move: the create argv's
// inputs, the fail-closed unknown-state plan, and the reattach warnings'
// remediation pair. They live in-package so the contract is asserted next to
// the code that owns it.

func TestBuildSbxArgsKeepsCreateInputs(t *testing.T) {
	cfg := &config.Config{}
	cfg.Kits.Stack = []string{"/stacked"}
	args := BuildSbxArgs(cfg, RunOpts{
		Workspace: "/ws", Name: "pix-x", LocalKit: "/repo/pi-kit", LocalImageTag: "local-1",
		StaticMCP: []string{"slack"}, PackKits: []string{"/packkit"}, Model: "anthropic/x",
		Models: []string{"a", "b"}, Passthrough: []string{"-p", "hi"},
	}, "0.0.1")
	got := strings.Join(args, " ")
	for _, want := range []string{
		"run pix", "--name pix-x", "--template docker.io/mcavage/pix:local-1",
		"--kit /repo/pi-kit", "--kit /packkit", "--kit /stacked",
		"--static-mcp slack", "/ws", "-- ", "--model anthropic/x", "--models a,b", "-p hi",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("argv %q missing %q", got, want)
		}
	}
}

func TestPlanSandboxLaunchFailsClosedOnUnknown(t *testing.T) {
	p := PlanSandboxLaunch(SbxUnknown, &config.Config{}, RunOpts{Name: "pix-x"}, "0.0.1")
	if p.Err == nil || p.Args != nil {
		t.Fatalf("unknown state must fail closed with no argv, got %+v", p)
	}
	if WillCreate(SbxUnknown) {
		t.Fatal("unknown state must never count as creating")
	}
}

// TestWillCreateIsThePlanDecision is the property that lets ONE predicate
// replace the three U04e collapsed (WillCreate, DefinitelyCreating,
// plan.Reattach): for every state the runtime can report, the predicate the
// command layer gates create-only work on and the plan the launch actually
// executes are the same answer. A drift here is the class of bug the two
// spellings existed to cause — create-only inputs resolved for an attach, or a
// create argv sent with none of them resolved.
func TestWillCreateIsThePlanDecision(t *testing.T) {
	for _, st := range []SbxState{SbxAbsent, SbxRunning, SbxStopped, SbxUnknown} {
		p := PlanSandboxLaunch(st, &config.Config{}, RunOpts{Name: "pix-x"}, "0.0.1")
		switch {
		case st == SbxUnknown:
			if p.Err == nil || WillCreate(st) {
				t.Fatalf("%s: want a refusal and no create, got %+v", st, p)
			}
		case WillCreate(st) == p.Reattach:
			t.Fatalf("%s: WillCreate=%v but plan.Reattach=%v — the predicate and the plan disagree",
				st, WillCreate(st), p.Reattach)
		}
	}
}

func TestPlanSandboxLaunchReattachDropsCreateOnlyFlags(t *testing.T) {
	p := PlanSandboxLaunch(SbxRunning, &config.Config{}, RunOpts{Name: "pix-x", LocalKit: "/k", Model: "m"}, "0.0.1")
	if !p.Reattach {
		t.Fatalf("a running sandbox must attach, got %+v", p)
	}
	got := strings.Join(p.Args, " ")
	if strings.Contains(got, "--kit") || !strings.Contains(got, "--model m") {
		t.Fatalf("reattach argv %q must drop create-only flags but keep --model", got)
	}
}

// TestRecreateGuidanceIsProofGatedAndNamesTheBox: every "this is not the
// sandbox you asked for" outcome points at the SAME two steps, and the removal
// step is the proof-gated one — never a forced remove, and never a bare
// `pix rm` the user has to guess a digest name for.
func TestRecreateGuidanceIsProofGatedAndNamesTheBox(t *testing.T) {
	got := RecreateGuidance("pix-proj-a1b2c3d4")
	for _, want := range []string{"pix rm pix-proj-a1b2c3d4", "pix run"} {
		if !strings.Contains(got, want) {
			t.Errorf("guidance %q is missing %q", got, want)
		}
	}
	for _, never := range []string{"--replace", "--force", "rm -f"} {
		if strings.Contains(got, never) {
			t.Errorf("guidance %q offers %q; removal must stay explicit and proof-gated", got, never)
		}
	}
}

func TestLocalImageLoadedFailsOpenWithoutSignal(t *testing.T) {
	noSbx := hostenv.Env{System: &systest.Fake{
		LookPathFn: func(string) (string, error) { return "", errors.New("not found") },
	}}
	if !LocalImageLoaded(noSbx, "local-1") {
		t.Fatal("no sbx must fail open")
	}
	hung := hostenv.Env{System: &systest.Fake{
		LookPathFn: func(string) (string, error) { return "/sbx", nil },
		RunTimedFn: func(string, ...string) (string, bool, error) { return "", true, nil },
	}}
	if !LocalImageLoaded(hung, "local-1") {
		t.Fatal("a timed-out probe must fail open")
	}
}
