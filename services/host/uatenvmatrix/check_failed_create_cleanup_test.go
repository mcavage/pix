// check_failed_create_cleanup_test.go proves environment_failed_create_cleanup
// (docs/design/environments.md section 9.3 / section 11, item 5) in
// isolation against an injected fake Executor and this check's own
// function — the same posture check_recreate_boundary_test.go and
// check_local_image_test.go use, so this unit's tests never need a real
// `sbx` binary or Run()'s full registry.
package uatenvmatrix

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestCheckEnvironmentFailedCreateCleanup_FailureBeforeReceiptRecordsNoRemoval
// is this unit's core red test: a native create that fails before Pix ever
// observes a positive receipt (no positively-identified instance id in the
// output) must record ZERO removal invocations — no `sbx env rm`, no `sbx
// rm` — and the check itself must succeed (the fail-closed outcome IS the
// expected, correct behavior; it is not a check failure).
func TestCheckEnvironmentFailedCreateCleanup_FailureBeforeReceiptRecordsNoRemoval(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	var recordedArgs [][]string
	executor := recordingExecutor{fn: func(args []string) (string, string, error) {
		recordedArgs = append(recordedArgs, append([]string(nil), args...))
		return "", "sbx: internal error resolving scoped secrets", errors.New("create failed")
	}}

	if err := checkEnvironmentFailedCreateCleanup(context.Background(), &lw, executor, phaseDir); err != nil {
		t.Fatalf("expected success when create fails before a positive receipt, got: %v", err)
	}

	if len(recordedArgs) != 1 {
		t.Fatalf("expected exactly 1 executor call (the failed create only), got %d: %#v", len(recordedArgs), recordedArgs)
	}
	for _, args := range recordedArgs {
		if isRemovalCommand(args) {
			t.Fatalf("recorded a removal command %#v with no positive create receipt", args)
		}
	}
}

// TestCheckEnvironmentFailedCreateCleanup_ArtifactNamesResidueAndNoCleanup
// proves the bounded artifact names the possible residue categories
// (scoped secrets, bindings, MCP registrations) and states plainly that no
// cleanup was attempted — the exact diagnostic docs/design/environments.md
// section 9.3 requires in place of guessing.
func TestCheckEnvironmentFailedCreateCleanup_ArtifactNamesResidueAndNoCleanup(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	executor := recordingExecutor{fn: func(args []string) (string, string, error) {
		return "", "sbx: internal error", errors.New("create failed")
	}}

	if err := checkEnvironmentFailedCreateCleanup(context.Background(), &lw, executor, phaseDir); err != nil {
		t.Fatalf("expected success, got: %v", err)
	}

	logged := lw.String()
	for _, want := range []string{"residue", "secret", "binding", "MCP", "no cleanup was attempted"} {
		if !strings.Contains(strings.ToLower(logged), strings.ToLower(want)) {
			t.Errorf("artifact does not mention %q: %s", want, logged)
		}
	}
}

// TestCheckEnvironmentFailedCreateCleanup_AnyRemovalCallIsCheckFailure proves
// the check's own removal guard: even if a future edit mistakenly issued a
// removal command, it must never reach the injected Executor and must fail
// the check rather than silently succeed.
func TestCheckEnvironmentFailedCreateCleanup_AnyRemovalCallIsCheckFailure(t *testing.T) {
	inner := recordingExecutor{fn: func(args []string) (string, string, error) {
		t.Fatalf("underlying executor invoked with %#v; a removal command must never be forwarded", args)
		return "", "", nil
	}}
	guarded := &noCleanupExecutor{inner: inner}

	for _, args := range [][]string{
		{"env", "rm", "-f", "pix-uatenv-fixture-failed-create"},
		{"rm", "-f", "pix-uatenv-fixture-failed-create"},
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

// TestCheckEnvironmentFailedCreateCleanup_PositiveReceiptIsNotThisUnit proves
// the success-before-failure / positive-receipt variant is explicitly out of
// scope for this check: if create DOES positively identify the fixture's
// instance, this check fails loudly rather than silently treating it as the
// before-receipt case it exists to prove.
func TestCheckEnvironmentFailedCreateCleanup_PositiveReceiptIsNotThisUnit(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	executor := recordingExecutor{fn: func(args []string) (string, string, error) {
		return "created " + failedCreateCleanupFixtureName + " (positively identified)\n", "", nil
	}}

	err := checkEnvironmentFailedCreateCleanup(context.Background(), &lw, executor, phaseDir)
	if err == nil {
		t.Fatal("expected an error when create positively identifies the instance; that is not this unit's scenario")
	}
}
