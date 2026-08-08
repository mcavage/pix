package main

import (
	"fmt"
	"strings"
	"testing"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/sandbox"
	"pix/host/sys"
	"pix/host/sys/systest"
	"pix/host/workflow/doctor"
	"pix/host/workflow/launch"
)

// TestPlanSandboxLaunch_AbsentCreates (a): no sandbox by that name -> a full
// create, argv carries --kit like any other fresh launch.
func TestPlanSandboxLaunch_AbsentCreates(t *testing.T) {
	cfg := &config.Config{}
	o := launch.RunOpts{Workspace: ".", Name: "pix-t"}
	plan := launch.PlanSandboxLaunch(launch.SbxAbsent, cfg, o, "0.0.99")

	if plan.Reattach {
		t.Error("absent sandbox must not reattach")
	}
	if !contains(plan.Args, []string{"--kit"}) {
		t.Errorf("create argv should carry --kit, got %v", plan.Args)
	}
}

// TestPlanSandboxLaunch_RunningReattaches (b): a RUNNING sandbox with no
// re-attaches: `run --name <name>`, none of the create-only flags,
// and the user's passthrough forwarded after --.
func TestPlanSandboxLaunch_RunningReattaches(t *testing.T) {
	cfg := &config.Config{}
	cfg.MCP = []string{"slack"}
	o := launch.RunOpts{
		Workspace:   ".",
		Name:        "pix-t",
		Kits:        []string{"/flag/kit"},
		Passthrough: []string{"--resume"},
	}
	plan := launch.PlanSandboxLaunch(launch.SbxRunning, cfg, o, "0.0.99")

	if !plan.Reattach {
		t.Error("running sandbox with no --replace should reattach")
	}
	want := []string{"run", "--name", "pix-t", "--", "--resume"}
	if len(plan.Args) != len(want) {
		t.Fatalf("reattach argv = %v, want %v", plan.Args, want)
	}
	for i := range want {
		if plan.Args[i] != want[i] {
			t.Fatalf("reattach argv = %v, want %v", plan.Args, want)
		}
	}
	for _, flag := range []string{"--kit", "--template", "--static-mcp"} {
		if contains(plan.Args, []string{flag}) {
			t.Errorf("reattach argv must not carry %s, got %v", flag, plan.Args)
		}
	}
}

// TestPlanSandboxLaunch_StoppedReattaches (c): a STOPPED sandbox with no
// --replace reattaches with the same shape as a running one.
func TestPlanSandboxLaunch_StoppedReattaches(t *testing.T) {
	cfg := &config.Config{}
	o := launch.RunOpts{Workspace: ".", Name: "pix-t"}
	plan := launch.PlanSandboxLaunch(launch.SbxStopped, cfg, o, "0.0.99")

	if !plan.Reattach {
		t.Error("stopped sandbox with no --replace should reattach")
	}
	want := []string{"run", "--name", "pix-t"}
	if len(plan.Args) != len(want) {
		t.Fatalf("reattach argv = %v, want %v", plan.Args, want)
	}
	for i := range want {
		if plan.Args[i] != want[i] {
			t.Fatalf("reattach argv = %v, want %v", plan.Args, want)
		}
	}
}

// TestPlanSandboxLaunch_UnknownFailsClosed: an indeterminate `sbx ls` can be
// neither created into nor attached to. It is the ONE fail-closed arm, and it
// must claim nothing at all.
func TestPlanSandboxLaunch_UnknownFailsClosed(t *testing.T) {
	o := launch.RunOpts{Workspace: ".", Name: "pix-t"}
	plan := launch.PlanSandboxLaunch(launch.SbxUnknown, &config.Config{}, o, "0.0.99")
	if plan.Err == nil {
		t.Fatal("an indeterminate sandbox state must fail closed with an error")
	}
	if plan.Reattach {
		t.Error("a failed-closed plan must not claim Reattach")
	}
	if len(plan.Args) != 0 {
		t.Errorf("a failed-closed plan must carry no Args, got %v", plan.Args)
	}
	if !strings.Contains(plan.Err.Error(), o.Name) {
		t.Errorf("error should name the sandbox, got: %v", plan.Err)
	}
}

// TestMcpLoadCommand_QuotesWorkspaceAndName pins closure finding #3: every
// generated copy-paste `mcp load` repair command shell-quotes BOTH the
// server name and the workspace via the shared sys.ShellQuote, so a workspace
// with spaces/apostrophe/shell metacharacters round-trips safely.
func TestMcpLoadCommand_QuotesWorkspaceAndName(t *testing.T) {
	for _, ws := range []string{
		"/home/mark/my repo's checkout",
		"/tmp/a;b",
		"/tmp/$HOME/proj",
	} {
		got := doctor.McpLoadCommand("slack", ws)
		want := "pix mcp load " + sys.ShellQuote("slack") + " " + sys.ShellQuote(ws)
		if got != want {
			t.Errorf("doctor.McpLoadCommand(%q) = %q, want %q", ws, got, want)
		}
	}
	// Bare form (no workspace) still quotes the name.
	if got := doctor.McpLoadCommand("slack", ""); got != "pix mcp load slack" {
		t.Errorf("doctor.McpLoadCommand bare = %q, want %q", got, "pix mcp load slack")
	}
}

// TestBuildReattachArgs_NoPassthrough: with no passthrough there is no trailing
// `--`.
func TestBuildReattachArgs_NoPassthrough(t *testing.T) {
	args := launch.BuildReattachArgs(launch.RunOpts{Name: "pix-t"})
	want := "run --name pix-t"
	if strings.Join(args, " ") != want {
		t.Errorf("launch.BuildReattachArgs = %v, want %q", args, want)
	}
}

// TestBuildReattachArgs_ForwardsModel (review finding 1, HIGH): a resolved
// o.Model (set directly by --model, or by --intent resolving into it in
// run.go) MUST reach pi on a re-attach exactly like it does on create —
// --model is a pi RUNTIME arg, not a create-only sbx flag.
func TestBuildReattachArgs_ForwardsModel(t *testing.T) {
	args := launch.BuildReattachArgs(launch.RunOpts{Name: "pix-t", Model: "openai/gpt-5.6-sol"})
	want := []string{"run", "--name", "pix-t", "--", "--model", "openai/gpt-5.6-sol"}
	if len(args) != len(want) {
		t.Fatalf("launch.BuildReattachArgs = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("launch.BuildReattachArgs = %v, want %v", args, want)
		}
	}
}

// TestBuildReattachArgs_ModelAndPassthrough: --model precedes the user's own
// passthrough after `--`, mirroring launch.BuildSbxArgs' own ordering.
func TestBuildReattachArgs_ModelAndPassthrough(t *testing.T) {
	args := launch.BuildReattachArgs(launch.RunOpts{Name: "pix-t", Model: "anthropic/claude-sonnet-5", Passthrough: []string{"--resume"}})
	want := []string{"run", "--name", "pix-t", "--", "--model", "anthropic/claude-sonnet-5", "--resume"}
	if len(args) != len(want) {
		t.Fatalf("launch.BuildReattachArgs = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("launch.BuildReattachArgs = %v, want %v", args, want)
		}
	}
}

// TestBuildReattachArgs_PassthroughOnly_NoModel: with o.Model empty, only the
// passthrough forwards — no stray --model.
func TestBuildReattachArgs_PassthroughOnly_NoModel(t *testing.T) {
	args := launch.BuildReattachArgs(launch.RunOpts{Name: "pix-t", Passthrough: []string{"--resume"}})
	want := []string{"run", "--name", "pix-t", "--", "--resume"}
	if len(args) != len(want) {
		t.Fatalf("launch.BuildReattachArgs = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("launch.BuildReattachArgs = %v, want %v", args, want)
		}
	}
	if contains(args, []string{"--model"}) {
		t.Errorf("no --model expected when o.Model is empty, got %v", args)
	}
}

// TestBuildReattachArgs_NeitherModelNorPassthrough: with neither set, there is
// no trailing `--` at all (matches TestBuildReattachArgs_NoPassthrough, kept
// explicit here since it's the third case review finding 1 calls out).
func TestBuildReattachArgs_NeitherModelNorPassthrough(t *testing.T) {
	args := launch.BuildReattachArgs(launch.RunOpts{Name: "pix-t"})
	if contains(args, []string{"--"}) {
		t.Errorf("no trailing -- expected with neither model nor passthrough, got %v", args)
	}
}

// TestRunDevResolution_SkippedOnAttach (review finding 3, MEDIUM): the bug was
// that --dev/checkout resolution ran BEFORE the state probe, so `pix run --name
// existing --dev` with no resolvable checkout exited on the checkout error
// instead of attaching (--dev is create-only). This exercises the actual
// predicate run gates that resolution behind.
func TestRunDevResolution_SkippedOnAttach(t *testing.T) {
	for _, state := range []sandbox.State{launch.SbxRunning, launch.SbxStopped} {
		if launch.WillCreate(state) {
			t.Fatalf("state=%v: launch.WillCreate must be false so --dev/checkout resolution (which needs a real repo"+
				" checkout) is skipped on an attach", state)
		}
	}
	if !launch.WillCreate(launch.SbxAbsent) {
		t.Fatal("a positively absent sandbox must resolve create-only inputs")
	}
	if launch.WillCreate(launch.SbxUnknown) {
		t.Fatal("unknown state must fail closed before resolving create-only inputs")
	}
}

// TestRunReplaceIsRetiredAndInert: `pix run --replace` was a forced `sbx rm -f`
// with no zero-holder proof — it could destroy a sandbox another shell was live
// in, which is exactly what U04d's proof-gated teardown exists to prevent. It is
// retired, and a retirement has three obligations: it still PARSES (a stale
// script gets a recovery path, not a syntax error), it answers with the
// machine-greppable marker and the replacement, and it is INERT — this runs
// before any config load, probe or exec.
func TestRunReplaceIsRetiredAndInert(t *testing.T) {
	if _, ok := retiredSurfaces()[retiredKey("run", "--replace")]; !ok {
		t.Fatal("run --replace must be in the retirement table so typing it answers")
	}
	o, err := parseRunOpts([]string{"--replace"})
	if err != nil {
		t.Fatalf("--replace must still parse, got: %v", err)
	}
	_ = o
	var errOut strings.Builder
	rerr := retiredFlag(&errOut, "run", "--replace")
	if got := cli.ExitCode(rerr); got != 2 {
		t.Errorf("exit code = %d, want 2", got)
	}
	msg := errOut.String()
	if !strings.HasPrefix(msg, "PIX_RETIRED") || !strings.Contains(msg, "pix rm") {
		t.Errorf("notice = %q, want the PIX_RETIRED marker and the pix rm replacement", msg)
	}
	if strings.Contains(msg, "rm -f") {
		t.Errorf("the replacement must not be a forced removal: %q", msg)
	}
}

// launch.LocalImageLoaded: present tag -> true; absent tag -> false; fails OPEN when it
// can't check (no sbx / ls error) so it never falsely refuses a launch.
func TestLocalImageLoaded(t *testing.T) {
	lsOut := launch.DockerImageRepo + "  local-111  abc123\n" +
		launch.DockerImageRepo + "  local-222  def456\n"
	present := hostenv.Env{System: &systest.Fake{LookPathFn: func(string) (string, error) { return "/usr/bin/sbx", nil }, RunFn: func(string, ...string) (string, error) { return lsOut, nil }}}
	if !launch.LocalImageLoaded(present, "local-222") {
		t.Error("a loaded tag must be reported present")
	}
	if launch.LocalImageLoaded(present, "local-999") {
		t.Error("an unloaded tag must be reported absent")
	}
	// Combined `repo:tag id` column form (the round-2 regression) must match too.
	combined := hostenv.Env{System: &systest.Fake{LookPathFn: func(string) (string, error) { return "/usr/bin/sbx", nil }, RunFn: func(string, ...string) (string, error) { return launch.DockerImageRepo + ":local-333  ghi789\n", nil }}}
	if !launch.LocalImageLoaded(combined, "local-333") {
		t.Error("combined repo:tag column must be recognized as present")
	}
	if launch.LocalImageLoaded(combined, "local-999") {
		t.Error("combined form: an unloaded tag must be absent")
	}
	// No sbx on PATH -> fail OPEN (true).
	noSbx := hostenv.Env{System: &systest.Fake{LookPathFn: func(string) (string, error) { return "", fmt.Errorf("not found") }, RunFn: func(string, ...string) (string, error) { return "", nil }}}
	if !launch.LocalImageLoaded(noSbx, "local-222") {
		t.Error("must fail open (true) when sbx is unavailable")
	}
	// ls error -> fail OPEN (true).
	lsErr := hostenv.Env{System: &systest.Fake{LookPathFn: func(string) (string, error) { return "/usr/bin/sbx", nil }, RunFn: func(string, ...string) (string, error) { return "", fmt.Errorf("boom") }}}
	if !launch.LocalImageLoaded(lsErr, "local-222") {
		t.Error("must fail open (true) when `sbx template ls` errors")
	}
	// Empty ls output -> no signal -> fail OPEN (true).
	empty := hostenv.Env{System: &systest.Fake{LookPathFn: func(string) (string, error) { return "/usr/bin/sbx", nil }, RunFn: func(string, ...string) (string, error) { return "   \n", nil }}}
	if !launch.LocalImageLoaded(empty, "local-222") {
		t.Error("must fail open (true) when ls output is empty")
	}
	// Store fully pruned (non-empty ls, tag absent) -> REFUSE (would otherwise pull).
	pruned := hostenv.Env{System: &systest.Fake{LookPathFn: func(string) (string, error) { return "/usr/bin/sbx", nil }, RunFn: func(string, ...string) (string, error) { return "REPOSITORY TAG ID\nother/img latest xyz\n", nil }}}
	if launch.LocalImageLoaded(pruned, "local-222") {
		t.Error("must refuse when the tag is absent from a non-empty store (pruned)")
	}
}
