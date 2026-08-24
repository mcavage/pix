// check_recreate_boundary_test.go proves environment_recreate_boundary
// (docs/design/environments.md section 11, item 4 / section 10.2) in
// isolation against an injected fake Executor and this check's own function
// — the same posture check_local_image_test.go and isolation_test.go use, so
// this unit's tests never need a real `sbx` binary or Run()'s full registry.
package uatenvmatrix

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// recreateBoundaryFakeExecutor answers every `sbx env create` call
// deterministically, keyed only on whether the fixture bytes it was handed
// carry the drifted facet marker — exactly the seam this check requires: no
// real sbx binary is ever invoked under go test, and the fake reads the same
// file bytes a real sbx binary would read from the path it was given.
type recreateBoundaryFakeExecutor struct {
	// driftedErr is returned for the second (post-mutation) create call.
	// nil simulates upstream silently accepting the drifted declaration —
	// the exact hazard this check exists to catch.
	driftedErr error
}

func (f recreateBoundaryFakeExecutor) Run(ctx context.Context, name string, args, env []string, dir string) (string, string, error) {
	// The fresh-probe and removal calls cleanupCreatedFixture issues after
	// this check's own create calls are routed by ARGV SHAPE, never by
	// re-sniffing the fixture file's current (possibly drifted) content: the
	// same fixturePath is reused for both the baseline and drifted create
	// calls, so by the time cleanup runs the file on disk holds whichever
	// content was written last — exactly like a real `sbx env rm -f <path>`
	// call, which resolves the environment's identity from registered state,
	// never by re-parsing the file's current bytes.
	if len(args) > 0 && args[0] == "ls" {
		return "created " + recreateBoundaryFixtureName + " (positively identified)\n", "", nil
	}
	if len(args) > 1 && args[0] == "env" && args[1] == "rm" {
		return "removed\n", "", nil
	}
	fixturePath := args[len(args)-1]
	content := mustReadFile(fixturePath)
	if strings.Contains(content, "memory: 60g") {
		if f.driftedErr != nil {
			return "", "environment declaration changed since creation", f.driftedErr
		}
		return "created " + recreateBoundaryFixtureName + " (positively identified)\n", "", nil
	}
	return "created " + recreateBoundaryFixtureName + " (positively identified)\n", "", nil
}

func mustReadFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func TestCheckEnvironmentRecreateBoundary_SilentReuseAcceptedFails(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	executor := recreateBoundaryFakeExecutor{driftedErr: nil}

	err := checkEnvironmentRecreateBoundary(context.Background(), &lw, executor, phaseDir)
	if err == nil {
		t.Fatal("expected an error when the mutated fixture is accepted/reused without refusal, got nil")
	}
	if !strings.Contains(err.Error(), recreateBoundaryMutatedFacet) {
		t.Errorf("error does not name the mutated facet %q: %v", recreateBoundaryMutatedFacet, err)
	}
	wantCmd := "pix rm " + recreateBoundaryFixtureName + " && pix run --env " + recreateBoundaryEnvName
	if !strings.Contains(err.Error(), wantCmd) {
		t.Errorf("error does not name the exact recreate command %q: %v", wantCmd, err)
	}
}

func TestCheckEnvironmentRecreateBoundary_RefusalLogsFacetAndRecreateCommand(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	executor := recreateBoundaryFakeExecutor{driftedErr: errors.New("sbx: environment declaration drifted since creation")}

	err := checkEnvironmentRecreateBoundary(context.Background(), &lw, executor, phaseDir)
	if err != nil {
		t.Fatalf("expected success when the drifted create is refused, got: %v", err)
	}
	logged := lw.String()
	if !strings.Contains(logged, recreateBoundaryMutatedFacet) {
		t.Errorf("artifact does not name the mutated facet %q: %s", recreateBoundaryMutatedFacet, logged)
	}
	wantCmd := "pix rm " + recreateBoundaryFixtureName + " && pix run --env " + recreateBoundaryEnvName
	if !strings.Contains(logged, wantCmd) {
		t.Errorf("artifact does not name the exact recreate command %q: %s", wantCmd, logged)
	}
}

func TestCheckEnvironmentRecreateBoundary_BaselineFailureFailsClosed(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	calls := 0
	executor := recordingExecutor{fn: func(args []string) (string, string, error) {
		calls++
		return "", "no such command", errors.New("baseline create failed")
	}}

	err := checkEnvironmentRecreateBoundary(context.Background(), &lw, executor, phaseDir)
	if err == nil {
		t.Fatal("expected an error when the baseline create fails, got nil")
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 executor call when the baseline create fails, got %d", calls)
	}
}

func TestCheckEnvironmentRecreateBoundary_BaselineUnidentifiedFails(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	calls := 0
	executor := recordingExecutor{fn: func(args []string) (string, string, error) {
		calls++
		return "accepted\n", "", nil
	}}

	err := checkEnvironmentRecreateBoundary(context.Background(), &lw, executor, phaseDir)
	if err == nil {
		t.Fatal("expected an error when the baseline create never reports the expected instance name")
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 executor call when the baseline instance is never positively identified, got %d", calls)
	}
}

func TestCheckEnvironmentRecreateBoundary_NeverPassesUserAuthoredName(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	var recordedArgs [][]string
	executor := recordingExecutor{fn: func(args []string) (string, string, error) {
		recordedArgs = append(recordedArgs, append([]string(nil), args...))
		return "created " + recreateBoundaryFixtureName + " (positively identified)\n", "", errors.New("refused: drifted")
	}}

	if err := checkEnvironmentRecreateBoundary(context.Background(), &lw, executor, phaseDir); err != nil {
		// The second call is expected to error (refusal path); only the
		// argv shape is under test here.
		_ = err
	}
	for _, args := range recordedArgs {
		for _, a := range args {
			if a == "--name" {
				t.Fatalf("create call passed a user-authored --name flag: %#v", args)
			}
		}
	}
}

// recordingExecutor is a minimal Executor whose behavior is entirely
// delegated to fn, keyed on argv only — used by the tests above that do not
// need to inspect fixture bytes.
type recordingExecutor struct {
	fn func(args []string) (string, string, error)
}

func (e recordingExecutor) Run(ctx context.Context, name string, args, env []string, dir string) (string, string, error) {
	return e.fn(args)
}
