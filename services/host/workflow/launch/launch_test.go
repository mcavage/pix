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
	p := PlanSandboxLaunch(SbxUnknown, true, &config.Config{}, RunOpts{Name: "pix-x"}, "0.0.1")
	if p.Err == nil || p.Args != nil || p.RmFirst {
		t.Fatalf("unknown state must fail closed with no argv, got %+v", p)
	}
	if WillCreate(SbxUnknown, true) || DefinitelyCreating(SbxUnknown, true) {
		t.Fatal("unknown state must never count as creating")
	}
}

func TestPlanSandboxLaunchReattachDropsCreateOnlyFlags(t *testing.T) {
	p := PlanSandboxLaunch(SbxRunning, false, &config.Config{}, RunOpts{Name: "pix-x", LocalKit: "/k", Model: "m"}, "0.0.1")
	if !p.Reattach || p.RmFirst {
		t.Fatalf("running + no --replace must reattach, got %+v", p)
	}
	got := strings.Join(p.Args, " ")
	if strings.Contains(got, "--kit") || !strings.Contains(got, "--model m") {
		t.Fatalf("reattach argv %q must drop create-only flags but keep --model", got)
	}
}

func TestReattachWarningsOfferBothRemediations(t *testing.T) {
	cfg := &config.Config{MCP: []string{"slack"}}
	o := RunOpts{Name: "pix-x", Workspace: "/ws"}
	msg := McpReattachWarning(cfg, o, true)
	if !strings.Contains(msg, "pix mcp load slack") || !strings.Contains(msg, "--replace") {
		t.Fatalf("mcp reattach warning must offer both paths, got %q", msg)
	}
	if McpReattachWarning(cfg, RunOpts{Name: "pix-x", Replace: true}, true) != "" {
		t.Fatal("--replace must silence the reattach warning")
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
