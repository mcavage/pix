// check_create_exec_test.go proves checkEnvironmentCreateThenExecInvocation's
// OWN receipt-gated cleanup wiring in isolation against an injected fake
// Executor — the same posture check_local_image_test.go and
// check_recreate_boundary_test.go use, so this unit's tests never need a
// real `sbx` binary or Run()'s full registry. matrix_test.go and
// cleanup_test.go already prove the happy path and the shared helper's own
// branches respectively; these tests prove the check-level WIRING: cleanup
// runs on both success and downstream failure, and never masks whichever
// error was already primary.
package uatenvmatrix

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// createExecFakeExecutor answers create/exec/ls/rm calls independently, so
// each test can compose exactly the failure combination it needs.
type createExecFakeExecutor struct {
	createOut string
	createErr error
	execOut   string
	execErr   error
	lsOut     string
	lsErr     error
	rmErr     error
	calls     [][]string
}

func (f *createExecFakeExecutor) Run(ctx context.Context, name string, args, env []string, dir string) (string, string, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	switch {
	case len(args) > 0 && args[0] == "ls":
		return f.lsOut, "", f.lsErr
	case len(args) > 1 && args[0] == "env" && args[1] == "rm":
		return "", "", f.rmErr
	case len(args) > 1 && args[0] == "env" && args[1] == "create":
		return f.createOut, "", f.createErr
	default: // name-based `sbx exec`
		return f.execOut, "", f.execErr
	}
}

func customAgentFixtureName(t *testing.T) string {
	t.Helper()
	return customAgentFixture().Name
}

// TestCheckEnvironmentCreateThenExecInvocation_SuccessRemovesFixture proves
// the full success path: a receipted create, a successful exec, a fresh
// probe reconfirming the same identity, then an environment-scoped removal
// — the check succeeds AND the artifact records the fixture was removed, so
// this check leaks nothing on the happy path.
func TestCheckEnvironmentCreateThenExecInvocation_SuccessRemovesFixture(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	name := customAgentFixtureName(t)
	fe := &createExecFakeExecutor{
		createOut: "created " + name + " (positively identified)\n",
		lsOut:     "created " + name + " (positively identified)\n",
	}

	if err := checkEnvironmentCreateThenExecInvocation(context.Background(), &lw, fe, phaseDir); err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if !strings.Contains(lw.String(), "cleanup: removed "+name) {
		t.Errorf("artifact does not record the fixture was removed: %s", lw.String())
	}
}

// TestCheckEnvironmentCreateThenExecInvocation_FreshProbeFailureFailsTheCheck
// proves a receipted create followed by a successful exec is NOT enough on
// its own: if the fresh post-exec probe cannot reconfirm the same identity,
// the check itself must fail (a real, undetected leak) rather than silently
// reporting success while residue lives on.
func TestCheckEnvironmentCreateThenExecInvocation_FreshProbeFailureFailsTheCheck(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	name := customAgentFixtureName(t)
	fe := &createExecFakeExecutor{
		createOut: "created " + name + " (positively identified)\n",
		lsErr:     errors.New("dial tcp: connection refused"),
	}

	err := checkEnvironmentCreateThenExecInvocation(context.Background(), &lw, fe, phaseDir)
	if err == nil {
		t.Fatal("expected a fresh-probe failure to fail the check, got nil")
	}
	if !strings.Contains(lw.String(), "residue possible") {
		t.Errorf("artifact does not record residue evidence: %s", lw.String())
	}
}

// TestCheckEnvironmentCreateThenExecInvocation_CleanupNeverMasksExecFailure
// proves the "runs on downstream failure without masking the primary error"
// requirement directly: the name-based exec fails (the primary error), and
// cleanup's own fresh probe ALSO fails to reconfirm the identity — the
// returned error must still be the ORIGINAL exec failure, with the cleanup
// evidence recorded only in the artifact, never substituted as the reported
// cause.
func TestCheckEnvironmentCreateThenExecInvocation_CleanupNeverMasksExecFailure(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	name := customAgentFixtureName(t)
	fe := &createExecFakeExecutor{
		createOut: "created " + name + " (positively identified)\n",
		execErr:   errors.New("dial tcp: connection refused (exec)"),
		lsErr:     errors.New("dial tcp: connection refused (probe)"),
	}

	err := checkEnvironmentCreateThenExecInvocation(context.Background(), &lw, fe, phaseDir)
	if err == nil {
		t.Fatal("expected the exec failure to fail the check, got nil")
	}
	if !strings.Contains(err.Error(), "name-based sbx exec") {
		t.Fatalf("expected the exec failure to remain the reported cause, got: %v", err)
	}
	if strings.Contains(err.Error(), "fresh probe") {
		t.Fatalf("cleanup's own fresh-probe failure must never replace the primary exec error, got: %v", err)
	}
	if !strings.Contains(lw.String(), "fresh probe did not reconfirm") {
		t.Errorf("artifact does not record the cleanup evidence even though it did not become the reported error: %s", lw.String())
	}
}

// TestCheckEnvironmentCreateThenExecInvocation_RemovalCommandFailureFailsTheCheck
// proves a receipted-and-reconfirmed instance whose actual removal command
// fails is reported as a check failure — a real leaked sandbox is worth
// surfacing, not silently accepted because create and exec both succeeded.
func TestCheckEnvironmentCreateThenExecInvocation_RemovalCommandFailureFailsTheCheck(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	name := customAgentFixtureName(t)
	fe := &createExecFakeExecutor{
		createOut: "created " + name + " (positively identified)\n",
		lsOut:     "created " + name + " (positively identified)\n",
		rmErr:     errors.New("sbx: exit status 1"),
	}

	err := checkEnvironmentCreateThenExecInvocation(context.Background(), &lw, fe, phaseDir)
	if err == nil {
		t.Fatal("expected the removal command failure to fail the check, got nil")
	}
	if !strings.Contains(err.Error(), "sbx env rm -f") {
		t.Fatalf("expected the removal failure to name the environment-scoped command, got: %v", err)
	}
}
