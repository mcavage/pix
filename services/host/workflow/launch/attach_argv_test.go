//go:build unix

// attach_argv_test.go — the U04c review fix: an existing, POSITIVELY
// IDENTIFIED, RUNNING sandbox is re-attached via `sbx exec -it/-i NAME pi
// <invocation>` (BuildAttachArgv), never `sbx run --name` — while CREATION
// still composes the full `sbx run` argv (BuildSbxArgs/PlanSandboxLaunch on
// SbxAbsent) unchanged. Real fixtures throughout: a genuinely exec'd `sbx`
// shell script (installFakeSbx) and real t.TempDir() workspaces, no mocked
// hostenv.System.
package launch

import (
	"strings"
	"testing"

	"pix/host/config"
)

// argvEqual is a small, local helper — this package's tests intentionally
// don't share cmd/pix's own `contains` (different package, different scope).
func argvEqual(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\x1f") != strings.Join(want, "\x1f") {
		t.Errorf("argv = %v, want %v", got, want)
	}
}

// TestCreateArgv_StillUsesSbxRun: a fresh create (the sandbox absent) composes
// the FULL `sbx run` argv, unchanged by this fix — only the ATTACH side moves
// to `sbx exec`.
func TestCreateArgv_StillUsesSbxRun(t *testing.T) {
	cfg := &config.Config{}
	o := RunOpts{Workspace: ".", Name: "pix-t", Model: "anthropic/claude-sonnet-5"}
	plan := PlanSandboxLaunch(SbxAbsent, cfg, o, "0.0.99")

	if plan.Reattach {
		t.Fatal("a fresh create must not be flagged as a reattach")
	}
	if len(plan.Args) == 0 || plan.Args[0] != "run" {
		t.Fatalf("create argv = %v, want to start with sbx run", plan.Args)
	}
	argvEqual(t, plan.Args[:3], []string{"run", "pix", "--name"})
}

// TestBuildAttachArgv_TTY: an interactive attach composes `exec -it NAME pi
// <invocation>` — never `run --name`.
func TestBuildAttachArgv_TTY(t *testing.T) {
	got, err := BuildAttachArgv("pix-t", true, []string{"--skill", "/opt/skills", "--model", "anthropic/claude-sonnet-5"})
	if err != nil {
		t.Fatalf("BuildAttachArgv: %v", err)
	}
	argvEqual(t, got, []string{"exec", "-it", "pix-t", "pi", "--skill", "/opt/skills", "--model", "anthropic/claude-sonnet-5"})
}

// TestBuildAttachArgv_NonTTY: a piped/scripted attach composes `exec -i NAME
// pi <invocation>` — the exec flag pi's own -it/-i convention mirrors, never
// left to a pty that isn't there.
func TestBuildAttachArgv_NonTTY(t *testing.T) {
	got, err := BuildAttachArgv("pix-t", false, []string{"--resume"})
	if err != nil {
		t.Fatalf("BuildAttachArgv: %v", err)
	}
	argvEqual(t, got, []string{"exec", "-i", "pix-t", "pi", "--resume"})
}

// TestFindPositivelyIdentifiedRunning_TTYAndNonTTY_RealFixture drives the
// FULL decision end to end against a REAL `sbx` fixture executable
// (installFakeSbx — genuinely exec'd, not a mocked System): a
// schema-verified, running row authorizes `sbx exec`, and the composed argv
// replays the STORED create-time invocation exactly, for both TTY shapes.
func TestFindPositivelyIdentifiedRunning_TTYAndNonTTY_RealFixture(t *testing.T) {
	isolateState(t)
	installFakeSbx(t, `
if [ "$1" = "ls" ] && [ "$2" = "--json" ]; then
  echo '[{"name":"pix-demo","state":"running","instance_id":"inst-1"}]'
  exit 0
fi
exit 1
`)
	key := SessionName(t.TempDir())
	stored := []string{"--skill", "/opt/skills", "--model", "anthropic/claude-sonnet-5"}
	if err := writeSessionState(key, sessionInvocationFileName, stored); err != nil {
		t.Fatalf("WriteSessionInvocation: %v", err)
	}

	found, ok := FindPositivelyIdentifiedRunning(realEnv(), "pix-demo")
	if !ok || found == nil {
		t.Fatalf("expected a positively identified running row, got ok=%v found=%v", ok, found)
	}

	invocation, has := readSessionInvocation(key)
	if !has {
		t.Fatal("expected the stored invocation to be readable back")
	}
	argvEqual(t, invocation, stored)

	ttyArgs, err := BuildAttachArgv("pix-demo", true, invocation)
	if err != nil {
		t.Fatalf("BuildAttachArgv (tty): %v", err)
	}
	argvEqual(t, ttyArgs, []string{"exec", "-it", "pix-demo", "pi", "--skill", "/opt/skills", "--model", "anthropic/claude-sonnet-5"})

	nonTTYArgs, err := BuildAttachArgv("pix-demo", false, invocation)
	if err != nil {
		t.Fatalf("BuildAttachArgv (non-tty): %v", err)
	}
	argvEqual(t, nonTTYArgs, []string{"exec", "-i", "pix-demo", "pi", "--skill", "/opt/skills", "--model", "anthropic/claude-sonnet-5"})
}

// TestFindPositivelyIdentifiedRunning_NoStoredInvocation_SafeDefault: an
// attach that finds no recorded invocation (a legacy sandbox, or one this
// host never proved it created) falls back to a freshly recomputed "safe
// current/default" one (BuildPiInvocation over the CURRENT RunOpts) — never
// refusing the attach and never inventing an ownership record.
func TestFindPositivelyIdentifiedRunning_NoStoredInvocation_SafeDefault(t *testing.T) {
	isolateState(t)
	installFakeSbx(t, `
if [ "$1" = "ls" ] && [ "$2" = "--json" ]; then
  echo '[{"name":"pix-legacy","state":"running","instance_id":"inst-9"}]'
  exit 0
fi
exit 1
`)
	key := SessionName(t.TempDir())
	if _, has := readSessionInvocation(key); has {
		t.Fatal("precondition: nothing should be stored yet")
	}
	_, ok := FindPositivelyIdentifiedRunning(realEnv(), "pix-legacy")
	if !ok {
		t.Fatal("expected a positively identified running row")
	}

	cfg := &config.Config{}
	o := RunOpts{Workspace: ".", Model: "anthropic/claude-sonnet-5"}
	safeDefault := BuildPiInvocation(LiveSkillDirs(cfg, o), o)
	execArgs, err := BuildAttachArgv("pix-legacy", true, safeDefault)
	if err != nil {
		t.Fatalf("BuildAttachArgv: %v", err)
	}
	// The personal skill tree is always loaded, so the safe default carries it
	// too: a re-attach must load the same skill layer the original launch did,
	// or a resumed session quietly loses the user's own skills.
	argvEqual(t, execArgs, []string{"exec", "-it", "pix-legacy", "pi",
		"--skill", PersonalSkillsDir(), "--model", "anthropic/claude-sonnet-5"})
}

// TestFindPositivelyIdentifiedRunning_Stopped_RefusesExec: a STOPPED sandbox
// (present, schema-verified, but not running) is NOT positively identified
// as running — exec has no "start" of its own, so a caller must fall back to
// the legacy `sbx run --name` reattach path instead, preserving existing
// compat for that case.
func TestFindPositivelyIdentifiedRunning_Stopped_RefusesExec(t *testing.T) {
	isolateState(t)
	installFakeSbx(t, `
if [ "$1" = "ls" ] && [ "$2" = "--json" ]; then
  echo '[{"name":"pix-demo","state":"stopped","instance_id":"inst-1"}]'
  exit 0
fi
exit 1
`)
	if _, ok := FindPositivelyIdentifiedRunning(realEnv(), "pix-demo"); ok {
		t.Fatal("a stopped sandbox must not be treated as positively-identified-running")
	}
}

// TestFindPositivelyIdentifiedRunning_UnverifiedRow_RefusesExec: a row that
// required a key alias (IdentityVerified=false) must not authorize `sbx
// exec` either — the same fail-closed posture RecordSessionCreation already
// holds for writing a lease record.
func TestFindPositivelyIdentifiedRunning_UnverifiedRow_RefusesExec(t *testing.T) {
	isolateState(t)
	installFakeSbx(t, `
if [ "$1" = "ls" ] && [ "$2" = "--json" ]; then
  echo '[{"Sandbox":"pix-demo","state":"running","instance_id":"inst-1"}]'
  exit 0
fi
exit 1
`)
	if _, ok := FindPositivelyIdentifiedRunning(realEnv(), "pix-demo"); ok {
		t.Fatal("an unverified row must not authorize sbx exec")
	}
}
