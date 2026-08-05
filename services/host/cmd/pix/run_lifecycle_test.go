package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	plan := launch.PlanSandboxLaunch(launch.SbxAbsent, false, cfg, o, "0.0.99")

	if plan.Reattach {
		t.Error("absent sandbox must not reattach")
	}
	if plan.RmFirst {
		t.Error("absent sandbox must not rm first")
	}
	if !contains(plan.Args, []string{"--kit"}) {
		t.Errorf("create argv should carry --kit, got %v", plan.Args)
	}
}

// TestPlanSandboxLaunch_RunningReattaches (b): a RUNNING sandbox with no
// --replace re-attaches: `run --name <name>`, none of the create-only flags,
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
	plan := launch.PlanSandboxLaunch(launch.SbxRunning, false, cfg, o, "0.0.99")

	if !plan.Reattach {
		t.Error("running sandbox with no --replace should reattach")
	}
	if plan.RmFirst {
		t.Error("a plain reattach must not rm first")
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
	plan := launch.PlanSandboxLaunch(launch.SbxStopped, false, cfg, o, "0.0.99")

	if !plan.Reattach {
		t.Error("stopped sandbox with no --replace should reattach")
	}
	if plan.RmFirst {
		t.Error("a plain reattach must not rm first")
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

// TestPlanSandboxLaunch_ReplaceRecreates (d): --replace forces rm-then-create
// regardless of the probed state, for both running and stopped.
func TestPlanSandboxLaunch_ReplaceRecreates(t *testing.T) {
	cfg := &config.Config{}
	o := launch.RunOpts{Workspace: ".", Name: "pix-t", Replace: true}

	for _, state := range []sandbox.State{launch.SbxRunning, launch.SbxStopped} {
		plan := launch.PlanSandboxLaunch(state, true, cfg, o, "0.0.99")
		if plan.Reattach {
			t.Errorf("state=%v --replace must not reattach", state)
		}
		if !plan.RmFirst {
			t.Errorf("state=%v --replace should rm first", state)
		}
		if !contains(plan.Args, []string{"--kit"}) {
			t.Errorf("state=%v --replace create argv should carry --kit, got %v", state, plan.Args)
		}
	}
}

// TestPlanSandboxLaunch_ReplaceOnAbsent (e): --replace on an absent sandbox is
// harmless — no rm is strictly needed, but it must still create.
func TestPlanSandboxLaunch_ReplaceOnAbsent(t *testing.T) {
	cfg := &config.Config{}
	o := launch.RunOpts{Workspace: ".", Name: "pix-t", Replace: true}
	plan := launch.PlanSandboxLaunch(launch.SbxAbsent, true, cfg, o, "0.0.99")

	if plan.Reattach {
		t.Error("--replace on an absent sandbox must not reattach")
	}
	if plan.RmFirst {
		t.Error("--replace on an absent sandbox should skip the rm (nothing to remove)")
	}
	if !contains(plan.Args, []string{"--kit"}) {
		t.Errorf("--replace on absent should still create, got %v", plan.Args)
	}
}

// TestPlanSandboxLaunch_UnknownFailsClosed: an indeterminate `sbx ls` can be
// neither safely created nor reattached because sbx run may replay arguments
// into an existing sandbox.
func TestPlanSandboxLaunch_UnknownFailsClosed(t *testing.T) {
	cfg := &config.Config{}
	o := launch.RunOpts{Workspace: ".", Name: "pix-t"}
	plan := launch.PlanSandboxLaunch(launch.SbxUnknown, false, cfg, o, "0.0.99")
	if plan.Err == nil || plan.Reattach || plan.RmFirst || len(plan.Args) != 0 {
		t.Fatalf("unknown state must fail closed with no action: %+v", plan)
	}
}

// Replace on unknown follows the same fail-closed rule as a plain launch and
// carries no action in its plan.
func TestPlanSandboxLaunch_ReplaceOnUnknown_FailsClosed(t *testing.T) {
	cfg := &config.Config{}
	o := launch.RunOpts{Workspace: ".", Name: "pix-t", Replace: true}
	plan := launch.PlanSandboxLaunch(launch.SbxUnknown, true, cfg, o, "0.0.99")
	if plan.Err == nil {
		t.Fatal("--replace on an indeterminate (unknown) sandbox state must fail closed with an error")
	}
	if plan.Reattach {
		t.Error("a failed-closed plan must not claim Reattach")
	}
	if plan.RmFirst {
		t.Error("a failed-closed plan must not claim RmFirst")
	}
	if len(plan.Args) != 0 {
		t.Errorf("a failed-closed plan must carry no Args, got %v", plan.Args)
	}
	if !strings.Contains(plan.Err.Error(), o.Name) {
		t.Errorf("error should name the sandbox, got: %v", plan.Err)
	}
}

// --- item 12: reattach/recreate recovery commands preserve an explicit
// workspace path (never print a bare command that would target cwd's
// sandbox instead of the one that actually failed/is stale) ---------------

func TestRunReplaceCommand_BareForCwdDefault(t *testing.T) {
	for _, ws := range []string{".", ""} {
		if got := launch.RunReplaceCommand(ws); got != "pix run --replace" {
			t.Errorf("launch.RunReplaceCommand(%q) = %q, want bare %q", ws, got, "pix run --replace")
		}
	}
}

func TestRunReplaceCommand_PreservesExplicitWorkspace(t *testing.T) {
	if got := launch.RunReplaceCommand("/home/mark/myproject"); got != "pix run /home/mark/myproject --replace" {
		t.Errorf("launch.RunReplaceCommand(explicit) = %q, want the explicit path preserved", got)
	}
}

// A workspace path needing shell-quoting (spaces, apostrophe) must be quoted
// POSIX-safely via the existing sys.ShellQuote, not printed raw (which would
// paste-and-break, or worse, silently split into multiple shell words).
func TestRunReplaceCommand_QuotesUnsafeWorkspace(t *testing.T) {
	ws := "/home/mark/my repo's"
	got := launch.RunReplaceCommand(ws)
	want := "pix run " + sys.ShellQuote(ws) + " --replace"
	if got != want {
		t.Errorf("launch.RunReplaceCommand(%q) = %q, want %q", ws, got, want)
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

// launch.StalePackReattachWarning must embed the SAME explicit-workspace-preserving
// recovery command, not a bare --replace that would target the wrong sandbox
// if cwd differs from the workspace that triggered the warning.
func TestStalePackReattachWarning_PreservesExplicitWorkspaceInFix(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "sub dir") // needs quoting
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	oldRoot := filepath.Join(t.TempDir(), "old-pack")
	launch.WriteSandboxPackMarker(ws, oldRoot)
	cfg := &config.Config{Pack: filepath.Join(t.TempDir(), "new-pack")}

	msg := launch.StalePackReattachWarning(cfg, launch.RunOpts{Workspace: ws}, true)
	want := launch.RunReplaceCommand(ws)
	if !strings.Contains(msg, want) {
		t.Errorf("warning must embed the explicit-workspace recovery command %q, got: %q", want, msg)
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

// TestApplyReplaceRm_FailurePropagatesAndBlocksCreate (review finding 2,
// MEDIUM): when `sbx rm -f` fails, launch.ApplyReplaceRm must return a non-nil error
// naming the sandbox — run.go's caller then os.Exit(1)s WITHOUT ever building
// or execing the create argv. We mirror that control flow here (a `created`
// flag that only flips on err==nil) to prove the failure blocks the create
// step, and assert exactly one rm attempt was made (no retry, no fallback).
func TestApplyReplaceRm_FailurePropagatesAndBlocksCreate(t *testing.T) {
	var recorded [][]string
	env := hostenv.Env{System: &systest.Fake{RunFn: func(cmd string, args ...string) (string, error) {
		recorded = append(recorded, append([]string{cmd}, args...))
		return "Error: cannot remove sandbox", fmt.Errorf("exit status 1")
	}}}
	plan := launch.RunLaunchPlan{RmFirst: true, Args: []string{"run", "pix", "."}}

	err := launch.ApplyReplaceRm(env, plan, "pix-t")
	if err == nil {
		t.Fatal("expected an error when `sbx rm -f` fails")
	}
	if !strings.Contains(err.Error(), "pix-t") {
		t.Errorf("error should name the sandbox, got: %v", err)
	}

	// Mirror run.go's control flow: it only proceeds to exec the create argv when
	// launch.ApplyReplaceRm returns nil.
	created := err == nil
	if created {
		t.Error("create must NOT be attempted when rm -f fails")
	}
	if len(recorded) != 1 || recorded[0][0] != "sbx" || recorded[0][1] != "rm" || recorded[0][2] != "-f" {
		t.Errorf("expected exactly one `sbx rm -f` call, got %v", recorded)
	}
}

// TestApplyReplaceRm_SuccessAllowsCreate: the mirror-image case — a successful
// rm returns nil, so run.go's caller proceeds to create.
func TestApplyReplaceRm_SuccessAllowsCreate(t *testing.T) {
	env := hostenv.Env{System: &systest.Fake{RunFn: func(cmd string, args ...string) (string, error) { return "", nil }}}
	plan := launch.RunLaunchPlan{RmFirst: true}
	if err := launch.ApplyReplaceRm(env, plan, "pix-t"); err != nil {
		t.Errorf("expected nil error on a successful rm, got %v", err)
	}
}

// TestApplyReplaceRm_NoOpWhenNotNeeded: RmFirst=false (a plain reattach or a
// create with nothing to remove) never calls env.Run at all.
func TestApplyReplaceRm_NoOpWhenNotNeeded(t *testing.T) {
	called := false
	env := hostenv.Env{System: &systest.Fake{RunFn: func(cmd string, args ...string) (string, error) { called = true; return "", nil }}}
	if err := launch.ApplyReplaceRm(env, launch.RunLaunchPlan{RmFirst: false}, "pix-t"); err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
	if called {
		t.Error("launch.ApplyReplaceRm must not call env.Run when RmFirst is false")
	}
}

// TestWillCreate_MatchesPlanSandboxLaunchReattachDecision (review finding 3,
// MEDIUM): launch.WillCreate is the single source of truth run.go uses to decide
// whether to resolve create-only inputs (checkout/--dev/kit) BEFORE the
// state probe's create-vs-reattach-vs-replace decision is even known. It must
// stay in lockstep with launch.PlanSandboxLaunch's own branching: false exactly when
// launch.PlanSandboxLaunch would produce a plain re-attach.
func TestWillCreate_MatchesPlanSandboxLaunchReattachDecision(t *testing.T) {
	cfg := &config.Config{}
	o := launch.RunOpts{Workspace: ".", Name: "pix-t"}
	for _, tc := range []struct {
		State   sandbox.State
		replace bool
		want    bool
	}{
		{launch.SbxAbsent, false, true},
		{launch.SbxUnknown, false, false},
		{launch.SbxRunning, false, false},
		{launch.SbxStopped, false, false},
		{launch.SbxRunning, true, true},
		{launch.SbxStopped, true, true},
		{launch.SbxAbsent, true, true},
		{launch.SbxUnknown, true, false},
	} {
		got := launch.WillCreate(tc.State, tc.replace)
		if got != tc.want {
			t.Errorf("launch.WillCreate(%v, %v) = %v, want %v", tc.State, tc.replace, got, tc.want)
		}
		plan := launch.PlanSandboxLaunch(tc.State, tc.replace, cfg, o, "0.0.99")
		if plan.Err == nil && got == plan.Reattach {
			t.Errorf("launch.WillCreate(%v, %v) = %v must be the inverse of plan.Reattach = %v", tc.State, tc.replace, got, plan.Reattach)
		}
	}
}

// TestRunDevResolution_SkippedOnReattach (review finding 3, MEDIUM): the
// bug was that --dev/checkout resolution ran BEFORE the state probe, so
// `pix run --name existing --dev` with no resolvable checkout would exit
// on the checkout error instead of re-attaching (--dev is create/replace-only).
// This test exercises the actual decision predicate run.go now gates the
// launch.ResolveRepoRoot() call behind: for a state that reattaches, launch.WillCreate must
// be false so run.go's `if launch.WillCreate(state, o.Replace) { ...resolveRepoRoot...
// }` branch is skipped entirely — a missing checkout is never even asked
// about, let alone allowed to fail the launch.
func TestRunDevResolution_SkippedOnReattach(t *testing.T) {
	for _, state := range []sandbox.State{launch.SbxRunning, launch.SbxStopped} {
		if launch.WillCreate(state, false) {
			t.Fatalf("state=%v: launch.WillCreate must be false so --dev/checkout resolution (which needs a real repo"+
				" checkout) is skipped on a plain re-attach", state)
		}
	}
	// And the create/replace paths DO need it resolved.
	for _, state := range []sandbox.State{launch.SbxAbsent} {
		if !launch.WillCreate(state, false) {
			t.Fatalf("state=%v: launch.WillCreate must be true so --dev/checkout resolution runs for a fresh create", state)
		}
	}
	if launch.WillCreate(launch.SbxUnknown, false) {
		t.Fatal("unknown state must fail closed before resolving create-only inputs")
	}
	if !launch.WillCreate(launch.SbxRunning, true) {
		t.Fatal("--replace on a running sandbox must still resolve --dev/checkout (it recreates)")
	}
}

// TestParseRunArgs_Replace: --replace parses as a bare boolean flag.
func TestParseRunArgs_Replace(t *testing.T) {
	o, err := launch.ParseRunArgs([]string{"--replace"})
	if err != nil {
		t.Fatalf("launch.ParseRunArgs(--replace) error: %v", err)
	}
	if !o.Replace {
		t.Error("expected Replace=true")
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
