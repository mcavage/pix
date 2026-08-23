// check_rm_scope_refusal_test.go proves environment_rm_scope_refusal
// (docs/design/environments.md section 10.3 / section 11, item 7) in
// isolation against an injected fake Executor and this check's own
// function — the same posture check_failed_create_cleanup_test.go and
// check_recreate_boundary_test.go use, so this unit's tests never need a
// real `sbx` binary or Run()'s full registry.
//
// Story 0 has no production sandbox.PlanEnvRemove yet (Story 2 wires launch
// and removal). These tests exercise this package's OWN typed policy
// function, planEnvRemoveRefusal — a literal, package-owned encoding of the
// upstream contract section 10.3 describes for the future production code,
// never a claim that a real `sbx` binary enforces Pix scope itself.
package uatenvmatrix

import (
	"context"
	"strings"
	"testing"
)

// TestPlanEnvRemoveRefusal_NonPixEffectiveNameRefused is this unit's first
// red test: an effective name outside `pix-*` scope must be refused before
// any removal call is even considered.
func TestPlanEnvRemoveRefusal_NonPixEffectiveNameRefused(t *testing.T) {
	err := planEnvRemoveRefusal(rmScopeRefusalNonPixEffectiveName, rmScopeRefusalFixtureName)
	if err == nil {
		t.Fatalf("expected refusal for non-pix-* effective name %q, got nil", rmScopeRefusalNonPixEffectiveName)
	}
	if !strings.Contains(err.Error(), "pix-*") {
		t.Errorf("refusal error does not name the pix-* scope requirement: %v", err)
	}
}

// TestPlanEnvRemoveRefusal_MismatchedEffectiveNameRefused is this unit's
// second red test: an effective name that IS pix-* scoped but does not equal
// the recorded instance name must also be refused.
func TestPlanEnvRemoveRefusal_MismatchedEffectiveNameRefused(t *testing.T) {
	if !strings.HasPrefix(rmScopeRefusalMismatchedEffectiveName, "pix-") {
		t.Fatalf("test fixture bug: %q must itself be pix-* scoped to isolate the mismatch case", rmScopeRefusalMismatchedEffectiveName)
	}
	err := planEnvRemoveRefusal(rmScopeRefusalMismatchedEffectiveName, rmScopeRefusalFixtureName)
	if err == nil {
		t.Fatalf("expected refusal for effective name %q mismatched against recorded instance %q, got nil", rmScopeRefusalMismatchedEffectiveName, rmScopeRefusalFixtureName)
	}
	if !strings.Contains(err.Error(), rmScopeRefusalMismatchedEffectiveName) || !strings.Contains(err.Error(), rmScopeRefusalFixtureName) {
		t.Errorf("refusal error does not name both the effective name and the recorded instance: %v", err)
	}
}

// TestPlanEnvRemoveRefusal_MatchingPixNameAllowed proves the policy function
// is a real proof, not an unconditional refusal: a pix-* effective name that
// equals the recorded instance name passes.
func TestPlanEnvRemoveRefusal_MatchingPixNameAllowed(t *testing.T) {
	if err := planEnvRemoveRefusal(rmScopeRefusalFixtureName, rmScopeRefusalFixtureName); err != nil {
		t.Fatalf("expected no refusal when effective name equals the recorded pix-* instance, got: %v", err)
	}
}

// TestCheckEnvironmentRmScopeRefusal_RefusesBothCasesWithNoExecutorCalls
// proves the check itself succeeds (both refusals ARE the expected, correct
// outcome) while never issuing any command through the injected Executor —
// this check proves a pure safety-policy contract, not a native sbx
// invocation, so zero executor calls is the correct shape.
func TestCheckEnvironmentRmScopeRefusal_RefusesBothCasesWithNoExecutorCalls(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	executor := recordingExecutor{fn: func(args []string) (string, string, error) {
		t.Fatalf("executor invoked with %#v; environment_rm_scope_refusal must never issue a real command", args)
		return "", "", nil
	}}

	if err := checkEnvironmentRmScopeRefusal(context.Background(), &lw, executor, phaseDir); err != nil {
		t.Fatalf("expected success when both refusal cases are observed, got: %v", err)
	}
}

// TestCheckEnvironmentRmScopeRefusal_ArtifactNamesBothCasesAndNoRemovalArgv
// proves the bounded artifact names both refused cases (scope and mismatch)
// by their effective names, and states plainly that no removal argv was
// ever issued — the exact evidence the acceptance criteria require.
func TestCheckEnvironmentRmScopeRefusal_ArtifactNamesBothCasesAndNoRemovalArgv(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	executor := recordingExecutor{fn: func(args []string) (string, string, error) {
		t.Fatalf("executor invoked with %#v", args)
		return "", "", nil
	}}

	if err := checkEnvironmentRmScopeRefusal(context.Background(), &lw, executor, phaseDir); err != nil {
		t.Fatalf("expected success, got: %v", err)
	}

	logged := lw.String()
	for _, want := range []string{
		"scope",
		"mismatch",
		rmScopeRefusalNonPixEffectiveName,
		rmScopeRefusalMismatchedEffectiveName,
		rmScopeRefusalFixtureName,
		"no removal argv",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("artifact does not mention %q: %s", want, logged)
		}
	}
	for _, forbidden := range []string{"env rm", "sbx rm", "\"rm\""} {
		if strings.Contains(logged, forbidden) {
			t.Errorf("artifact records a removal argv fragment %q, want none: %s", forbidden, logged)
		}
	}
}

// TestCheckEnvironmentRmScopeRefusal_AnyRemovalCallIsCheckFailure proves this
// check's own removal guard: even if a future edit mistakenly issued a
// removal command for either refused case, it must never reach the injected
// Executor and must fail rather than silently succeed — the same
// belt-and-braces enforcement check_failed_create_cleanup.go's
// noCleanupExecutor gives that check.
func TestCheckEnvironmentRmScopeRefusal_AnyRemovalCallIsCheckFailure(t *testing.T) {
	inner := recordingExecutor{fn: func(args []string) (string, string, error) {
		t.Fatalf("underlying executor invoked with %#v; a removal command must never be forwarded", args)
		return "", "", nil
	}}
	guarded := &rmScopeRefusalGuardExecutor{inner: inner}

	for _, args := range [][]string{
		{"env", "rm", "-f", rmScopeRefusalNonPixEffectiveName},
		{"rm", "-f", rmScopeRefusalMismatchedEffectiveName},
	} {
		_, _, err := guarded.Run(context.Background(), "sbx", args, nil, "")
		if err == nil {
			t.Fatalf("expected an error for removal command %#v, got nil", args)
		}
		if !guarded.attemptedRemoval {
			t.Fatalf("expected attemptedRemoval to be set after removal command %#v", args)
		}
		guarded.attemptedRemoval = false
	}
}
