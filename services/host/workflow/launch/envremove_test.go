package launch

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/cli"
)

// writeEffectiveFixture writes a minimal, valid v1 effective document
// naming `name`, mirroring what E2.1's RenderEffective would have produced
// (envinfo/render_test.go's own fixtures) — hand-authored bytes, not a
// second renderer this test would then be testing against itself.
func writeEffectiveFixture(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, "effective.sbxenv.yaml")
	body := "schemaVersion: \"1\"\nname: " + name + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestPlanEnvRemoveSeam_F8UsesEffectivePathWhenNameMatches is F8 at
// the launch-integration seam: an effective document whose own `name:`
// equals the recorded pix-* instance plans `env rm -f <effectivePath>`.
func TestPlanEnvRemoveSeam_F8UsesEffectivePathWhenNameMatches(t *testing.T) {
	dir := t.TempDir()
	const instance = "pix-work-abc12345"
	path := writeEffectiveFixture(t, dir, instance)

	plan, err := PlanEnvRemoveSeam(path, instance, true)
	if err != nil {
		t.Fatalf("PlanEnvRemoveSeam: %v", err)
	}
	want := []string{"env", "rm", "-f", path}
	if len(plan.Argv) != len(want) {
		t.Fatalf("Argv = %v, want %v", plan.Argv, want)
	}
	for i := range want {
		if plan.Argv[i] != want[i] {
			t.Fatalf("Argv = %v, want %v", plan.Argv, want)
		}
	}
	if plan.Report != "" {
		t.Fatalf("Report = %q, want empty on the successful primary path", plan.Report)
	}
}

// TestPlanEnvRemoveSeam_RefusesNonPixScopedName proves the non-pix-*
// case refuses with exit code 2 (a cli.UsageError) and plans no argv.
func TestPlanEnvRemoveSeam_RefusesNonPixScopedName(t *testing.T) {
	dir := t.TempDir()
	path := writeEffectiveFixture(t, dir, "not-pix-scoped-env")

	plan, err := PlanEnvRemoveSeam(path, "pix-uatenv-fixture-rm-scope", true)
	if err == nil {
		t.Fatalf("PlanEnvRemoveSeam(non pix-* effective name) = nil error, want a refusal")
	}
	if plan.Argv != nil {
		t.Fatalf("plan.Argv = %v, want nil on refusal", plan.Argv)
	}
	if code := cli.ExitCode(err); code != 2 {
		t.Fatalf("cli.ExitCode(%v) = %d, want 2", err, code)
	}
	var usage cli.UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("err = %v, want a cli.UsageError", err)
	}
}

// TestPlanEnvRemoveSeam_RefusesInstanceMismatch proves the
// pix-*-scoped-but-mismatched case also refuses with exit code 2.
func TestPlanEnvRemoveSeam_RefusesInstanceMismatch(t *testing.T) {
	dir := t.TempDir()
	path := writeEffectiveFixture(t, dir, "pix-uatenv-fixture-rm-scope-mismatch")

	plan, err := PlanEnvRemoveSeam(path, "pix-uatenv-fixture-rm-scope", true)
	if err == nil {
		t.Fatalf("PlanEnvRemoveSeam(mismatched instance) = nil error, want a refusal")
	}
	if plan.Argv != nil {
		t.Fatalf("plan.Argv = %v, want nil on refusal", plan.Argv)
	}
	if code := cli.ExitCode(err); code != 2 {
		t.Fatalf("cli.ExitCode(%v) = %d, want 2", err, code)
	}
}

// TestPlanEnvRemoveSeam_FallsBackWhenEffectiveFileAbsent is §10.3's
// "pre-migration sandbox or a hard crash that lost state" case: no file at
// effectivePath falls back to the existing name-based planner and reports
// that environment-scoped secret cleanup could not run.
func TestPlanEnvRemoveSeam_FallsBackWhenEffectiveFileAbsent(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "effective.sbxenv.yaml") // never written
	const instance = "pix-work-abc12345"

	plan, err := PlanEnvRemoveSeam(missing, instance, true)
	if err != nil {
		t.Fatalf("PlanEnvRemoveSeam: %v", err)
	}
	want := []string{"rm", "-f", instance}
	if len(plan.Argv) != len(want) {
		t.Fatalf("Argv = %v, want %v", plan.Argv, want)
	}
	for i := range want {
		if plan.Argv[i] != want[i] {
			t.Fatalf("Argv = %v, want %v", plan.Argv, want)
		}
	}
	if !strings.Contains(plan.Report, "environment-scoped secret cleanup could not run") {
		t.Fatalf("Report = %q, want it to explicitly say environment-scoped secret cleanup could not run", plan.Report)
	}
	if !strings.Contains(plan.Report, instance) {
		t.Fatalf("Report = %q, want it to name the recorded instance %q", plan.Report, instance)
	}
}

// TestPlanEnvRemoveSeam_FallbackNonForcedUsesPlanRemove proves the
// fallback honors force=false by using sandbox.PlanRemove's own argv shape
// (no -f), the same non-force/force split PlanRemove/PlanForceRemove
// already offer every other caller in this package.
func TestPlanEnvRemoveSeam_FallbackNonForcedUsesPlanRemove(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "effective.sbxenv.yaml")
	const instance = "pix-work-abc12345"

	plan, err := PlanEnvRemoveSeam(missing, instance, false)
	if err != nil {
		t.Fatalf("PlanEnvRemoveSeam: %v", err)
	}
	want := []string{"rm", instance}
	if len(plan.Argv) != len(want) || plan.Argv[0] != want[0] || plan.Argv[1] != want[1] {
		t.Fatalf("Argv = %v, want %v", plan.Argv, want)
	}
}

// TestPlanEnvRemoveSeam_EmptyEffectivePathAlwaysFallsBack proves an
// empty effectivePath (no environment launch recorded one at all) is
// treated exactly like an absent file: fallback, with the same report.
func TestPlanEnvRemoveSeam_EmptyEffectivePathAlwaysFallsBack(t *testing.T) {
	const instance = "pix-work-abc12345"
	plan, err := PlanEnvRemoveSeam("", instance, true)
	if err != nil {
		t.Fatalf("PlanEnvRemoveSeam: %v", err)
	}
	if !strings.Contains(plan.Report, "environment-scoped secret cleanup could not run") {
		t.Fatalf("Report = %q, want the fallback cleanup-not-run note", plan.Report)
	}
}

// TestPlanEnvRemoveSeam_NeverAppendsPruneBindings is the argv-matrix
// test at the launch-integration seam: across every branch this function
// can take (success, both refusals, fallback with and without force, and
// an empty path), no composed argv ever mentions pruning. A3's nonclaim
// stays exactly what it was; this only proves the narrower, checkable
// half — this seam never composes an argv that would ask sbx to prune.
func TestPlanEnvRemoveSeam_NeverAppendsPruneBindings(t *testing.T) {
	assertNoPrune := func(t *testing.T, argv []string) {
		t.Helper()
		for _, a := range argv {
			if a == "--prune-bindings" || strings.Contains(a, "prune") {
				t.Fatalf("argv %v mentions pruning; must never plan a binding prune", argv)
			}
		}
	}

	dir := t.TempDir()
	const instance = "pix-argvmatrix-11111111"
	matchedPath := writeEffectiveFixture(t, dir, instance)

	if plan, err := PlanEnvRemoveSeam(matchedPath, instance, true); err == nil {
		assertNoPrune(t, plan.Argv)
	} else {
		t.Fatalf("primary path: %v", err)
	}

	if plan, err := PlanEnvRemoveSeam(filepath.Join(dir, "missing.sbxenv.yaml"), instance, true); err == nil {
		assertNoPrune(t, plan.Argv)
	} else {
		t.Fatalf("fallback (force): %v", err)
	}

	if plan, err := PlanEnvRemoveSeam(filepath.Join(dir, "missing.sbxenv.yaml"), instance, false); err == nil {
		assertNoPrune(t, plan.Argv)
	} else {
		t.Fatalf("fallback (non-force): %v", err)
	}

	if plan, err := PlanEnvRemoveSeam("", instance, true); err == nil {
		assertNoPrune(t, plan.Argv)
	} else {
		t.Fatalf("empty path fallback: %v", err)
	}

	// Both refusal branches compose no argv at all — nothing to scan.
	nonPixPath := writeEffectiveFixture(t, dir, "not-pix-scoped")
	if plan, err := PlanEnvRemoveSeam(nonPixPath, instance, true); err == nil {
		t.Fatalf("expected a refusal, got argv %v", plan.Argv)
	} else if plan.Argv != nil {
		t.Fatalf("plan.Argv = %v on refusal, want nil", plan.Argv)
	}
}
