package main

import (
	"fmt"
	"strings"
	"testing"

	"pi-stack/host/config"
)

// TestPlanSandboxLaunch_AbsentCreates (a): no sandbox by that name -> a full
// create, argv carries --kit like any other fresh launch.
func TestPlanSandboxLaunch_AbsentCreates(t *testing.T) {
	cfg := &config.Config{}
	o := runOpts{Workspace: ".", Name: "pi-stack-t"}
	plan := planSandboxLaunch(sbxAbsent, false, cfg, o, "0.0.99")

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
	o := runOpts{
		Workspace:   ".",
		Name:        "pi-stack-t",
		MCPEnabled:  true,
		Kits:        []string{"/flag/kit"},
		Passthrough: []string{"--resume"},
	}
	plan := planSandboxLaunch(sbxRunning, false, cfg, o, "0.0.99")

	if !plan.Reattach {
		t.Error("running sandbox with no --replace should reattach")
	}
	if plan.RmFirst {
		t.Error("a plain reattach must not rm first")
	}
	want := []string{"run", "--name", "pi-stack-t", "--", "--resume"}
	if len(plan.Args) != len(want) {
		t.Fatalf("reattach argv = %v, want %v", plan.Args, want)
	}
	for i := range want {
		if plan.Args[i] != want[i] {
			t.Fatalf("reattach argv = %v, want %v", plan.Args, want)
		}
	}
	for _, flag := range []string{"--kit", "--template", "--mcp"} {
		if contains(plan.Args, []string{flag}) {
			t.Errorf("reattach argv must not carry %s, got %v", flag, plan.Args)
		}
	}
}

// TestPlanSandboxLaunch_StoppedReattaches (c): a STOPPED sandbox with no
// --replace reattaches with the same shape as a running one.
func TestPlanSandboxLaunch_StoppedReattaches(t *testing.T) {
	cfg := &config.Config{}
	o := runOpts{Workspace: ".", Name: "pi-stack-t"}
	plan := planSandboxLaunch(sbxStopped, false, cfg, o, "0.0.99")

	if !plan.Reattach {
		t.Error("stopped sandbox with no --replace should reattach")
	}
	if plan.RmFirst {
		t.Error("a plain reattach must not rm first")
	}
	want := []string{"run", "--name", "pi-stack-t"}
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
	o := runOpts{Workspace: ".", Name: "pi-stack-t", Replace: true}

	for _, state := range []sbxState{sbxRunning, sbxStopped} {
		plan := planSandboxLaunch(state, true, cfg, o, "0.0.99")
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
	o := runOpts{Workspace: ".", Name: "pi-stack-t", Replace: true}
	plan := planSandboxLaunch(sbxAbsent, true, cfg, o, "0.0.99")

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

// TestPlanSandboxLaunch_UnknownCreates: an indeterminate `sbx ls` (unknown)
// degrades to the same behavior as absent — attempt a create rather than
// refusing to launch on a probe failure.
func TestPlanSandboxLaunch_UnknownCreates(t *testing.T) {
	cfg := &config.Config{}
	o := runOpts{Workspace: ".", Name: "pi-stack-t"}
	plan := planSandboxLaunch(sbxUnknown, false, cfg, o, "0.0.99")
	if plan.Reattach {
		t.Error("unknown state must not reattach")
	}
	if !contains(plan.Args, []string{"--kit"}) {
		t.Errorf("unknown state should still create, got %v", plan.Args)
	}
}

// TestBuildReattachArgs_NoPassthrough: with no passthrough there is no trailing
// `--`.
func TestBuildReattachArgs_NoPassthrough(t *testing.T) {
	args := buildReattachArgs(runOpts{Name: "pi-stack-t"})
	want := "run --name pi-stack-t"
	if strings.Join(args, " ") != want {
		t.Errorf("buildReattachArgs = %v, want %q", args, want)
	}
}

// TestBuildReattachArgs_ForwardsModel (review finding 1, HIGH): a resolved
// o.Model (set directly by --model, or by --intent resolving into it in
// run.go) MUST reach pi on a re-attach exactly like it does on create —
// --model is a pi RUNTIME arg, not a create-only sbx flag.
func TestBuildReattachArgs_ForwardsModel(t *testing.T) {
	args := buildReattachArgs(runOpts{Name: "pi-stack-t", Model: "openai/gpt-5.6-sol"})
	want := []string{"run", "--name", "pi-stack-t", "--", "--model", "openai/gpt-5.6-sol"}
	if len(args) != len(want) {
		t.Fatalf("buildReattachArgs = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("buildReattachArgs = %v, want %v", args, want)
		}
	}
}

// TestBuildReattachArgs_ModelAndPassthrough: --model precedes the user's own
// passthrough after `--`, mirroring buildSbxArgs' own ordering.
func TestBuildReattachArgs_ModelAndPassthrough(t *testing.T) {
	args := buildReattachArgs(runOpts{Name: "pi-stack-t", Model: "anthropic/claude-sonnet-5", Passthrough: []string{"--resume"}})
	want := []string{"run", "--name", "pi-stack-t", "--", "--model", "anthropic/claude-sonnet-5", "--resume"}
	if len(args) != len(want) {
		t.Fatalf("buildReattachArgs = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("buildReattachArgs = %v, want %v", args, want)
		}
	}
}

// TestBuildReattachArgs_PassthroughOnly_NoModel: with o.Model empty, only the
// passthrough forwards — no stray --model.
func TestBuildReattachArgs_PassthroughOnly_NoModel(t *testing.T) {
	args := buildReattachArgs(runOpts{Name: "pi-stack-t", Passthrough: []string{"--resume"}})
	want := []string{"run", "--name", "pi-stack-t", "--", "--resume"}
	if len(args) != len(want) {
		t.Fatalf("buildReattachArgs = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("buildReattachArgs = %v, want %v", args, want)
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
	args := buildReattachArgs(runOpts{Name: "pi-stack-t"})
	if contains(args, []string{"--"}) {
		t.Errorf("no trailing -- expected with neither model nor passthrough, got %v", args)
	}
}

// TestApplyReplaceRm_FailurePropagatesAndBlocksCreate (review finding 2,
// MEDIUM): when `sbx rm -f` fails, applyReplaceRm must return a non-nil error
// naming the sandbox — run.go's caller then os.Exit(1)s WITHOUT ever building
// or execing the create argv. We mirror that control flow here (a `created`
// flag that only flips on err==nil) to prove the failure blocks the create
// step, and assert exactly one rm attempt was made (no retry, no fallback).
func TestApplyReplaceRm_FailurePropagatesAndBlocksCreate(t *testing.T) {
	var recorded [][]string
	env := shellEnv{run: func(cmd string, args ...string) (string, error) {
		recorded = append(recorded, append([]string{cmd}, args...))
		return "Error: cannot remove sandbox", fmt.Errorf("exit status 1")
	}}
	plan := runLaunchPlan{RmFirst: true, Args: []string{"run", "pi-stack", "."}}

	err := applyReplaceRm(env, plan, "pi-stack-t")
	if err == nil {
		t.Fatal("expected an error when `sbx rm -f` fails")
	}
	if !strings.Contains(err.Error(), "pi-stack-t") {
		t.Errorf("error should name the sandbox, got: %v", err)
	}

	// Mirror run.go's control flow: it only proceeds to exec the create argv when
	// applyReplaceRm returns nil.
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
	env := shellEnv{run: func(cmd string, args ...string) (string, error) { return "", nil }}
	plan := runLaunchPlan{RmFirst: true}
	if err := applyReplaceRm(env, plan, "pi-stack-t"); err != nil {
		t.Errorf("expected nil error on a successful rm, got %v", err)
	}
}

// TestApplyReplaceRm_NoOpWhenNotNeeded: RmFirst=false (a plain reattach or a
// create with nothing to remove) never calls env.run at all.
func TestApplyReplaceRm_NoOpWhenNotNeeded(t *testing.T) {
	called := false
	env := shellEnv{run: func(cmd string, args ...string) (string, error) { called = true; return "", nil }}
	if err := applyReplaceRm(env, runLaunchPlan{RmFirst: false}, "pi-stack-t"); err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
	if called {
		t.Error("applyReplaceRm must not call env.run when RmFirst is false")
	}
}

// TestWillCreate_MatchesPlanSandboxLaunchReattachDecision (review finding 3,
// MEDIUM): willCreate is the single source of truth run.go uses to decide
// whether to resolve create-only inputs (checkout/--dev/kit) BEFORE the
// state probe's create-vs-reattach-vs-replace decision is even known. It must
// stay in lockstep with planSandboxLaunch's own branching: false exactly when
// planSandboxLaunch would produce a plain re-attach.
func TestWillCreate_MatchesPlanSandboxLaunchReattachDecision(t *testing.T) {
	cfg := &config.Config{}
	o := runOpts{Workspace: ".", Name: "pi-stack-t"}
	for _, tc := range []struct {
		state   sbxState
		replace bool
		want    bool
	}{
		{sbxAbsent, false, true},
		{sbxUnknown, false, true},
		{sbxRunning, false, false},
		{sbxStopped, false, false},
		{sbxRunning, true, true},
		{sbxStopped, true, true},
		{sbxAbsent, true, true},
	} {
		got := willCreate(tc.state, tc.replace)
		if got != tc.want {
			t.Errorf("willCreate(%v, %v) = %v, want %v", tc.state, tc.replace, got, tc.want)
		}
		plan := planSandboxLaunch(tc.state, tc.replace, cfg, o, "0.0.99")
		if got == plan.Reattach {
			t.Errorf("willCreate(%v, %v) = %v must be the inverse of plan.Reattach = %v", tc.state, tc.replace, got, plan.Reattach)
		}
	}
}

// TestRunDevResolution_SkippedOnReattach (review finding 3, MEDIUM): the
// bug was that --dev/checkout resolution ran BEFORE the state probe, so
// `pi-stack run --name existing --dev` with no resolvable checkout would exit
// on the checkout error instead of re-attaching (--dev is create/replace-only).
// This test exercises the actual decision predicate run.go now gates the
// resolveRepoRoot() call behind: for a state that reattaches, willCreate must
// be false so run.go's `if willCreate(state, o.Replace) { ...resolveRepoRoot...
// }` branch is skipped entirely — a missing checkout is never even asked
// about, let alone allowed to fail the launch.
func TestRunDevResolution_SkippedOnReattach(t *testing.T) {
	for _, state := range []sbxState{sbxRunning, sbxStopped} {
		if willCreate(state, false) {
			t.Fatalf("state=%v: willCreate must be false so --dev/checkout resolution (which needs a real repo"+
				" checkout) is skipped on a plain re-attach", state)
		}
	}
	// And the create/replace paths DO need it resolved.
	for _, state := range []sbxState{sbxAbsent, sbxUnknown} {
		if !willCreate(state, false) {
			t.Fatalf("state=%v: willCreate must be true so --dev/checkout resolution runs for a fresh create", state)
		}
	}
	if !willCreate(sbxRunning, true) {
		t.Fatal("--replace on a running sandbox must still resolve --dev/checkout (it recreates)")
	}
}

// TestParseRunArgs_Replace: --replace parses as a bare boolean flag.
func TestParseRunArgs_Replace(t *testing.T) {
	o, err := parseRunArgs([]string{"--replace"})
	if err != nil {
		t.Fatalf("parseRunArgs(--replace) error: %v", err)
	}
	if !o.Replace {
		t.Error("expected Replace=true")
	}
}
