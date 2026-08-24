// sbxversion_test.go proves probeSbxVersion's own contract in isolation
// against createExecFakeExecutor (check_create_exec_test.go), independent
// of the whole checkEnvironmentCreateThenExecInvocation flow:
// TestCheckEnvironmentCreateThenExecInvocation_VersionProbeRunsFirstAndLogsObservedVersion
// and its sibling in check_create_exec_test.go prove the wiring; these
// tests prove the probe's own retry/fail-closed decisions.
package uatenvmatrix

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestProbeSbxVersion_PrimarySuccess proves the common case: `sbx --version`
// succeeds and the recognizable version is extracted and logged.
func TestProbeSbxVersion_PrimarySuccess(t *testing.T) {
	var lw strings.Builder
	fe := &createExecFakeExecutor{versionOut: "sbx version 0.39\n"}

	got, err := probeSbxVersion(context.Background(), &lw, fe, nil, t.TempDir())
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if got != "0.39" {
		t.Fatalf("got version %q, want %q", got, "0.39")
	}
	if len(fe.calls) != 1 || fe.calls[0][0] != "--version" {
		t.Fatalf("expected exactly one `sbx --version` call, got calls=%v", fe.calls)
	}
	if !strings.Contains(lw.String(), "observed sbx version: 0.39") {
		t.Errorf("expected the normalized observed-version line, got log=%s", lw.String())
	}
}

// TestProbeSbxVersion_UnknownFlagFallsBackToVersionSubcommand proves the
// ONE known alternate grammar: a primary refused specifically as an
// unknown flag falls back to `sbx version`, and the returned value comes
// from that fallback attempt.
func TestProbeSbxVersion_UnknownFlagFallsBackToVersionSubcommand(t *testing.T) {
	var lw strings.Builder
	fe := &createExecFakeExecutor{
		versionErrOut:      "Error: unknown flag: --version\n",
		versionErr:         errors.New("exit status 1"),
		versionFallbackOut: "sbx version 0.39\n",
	}

	got, err := probeSbxVersion(context.Background(), &lw, fe, nil, t.TempDir())
	if err != nil {
		t.Fatalf("expected the fallback to succeed, got: %v", err)
	}
	if got != "0.39" {
		t.Fatalf("got version %q, want %q", got, "0.39")
	}
	if len(fe.calls) != 2 || fe.calls[0][0] != "--version" || fe.calls[1][0] != "version" {
		t.Fatalf("expected primary `--version` then fallback `version`, got calls=%v", fe.calls)
	}
	if !strings.Contains(lw.String(), "falling back") {
		t.Errorf("expected the log to record the fallback decision, got log=%s", lw.String())
	}
}

// TestProbeSbxVersion_UnrelatedPrimaryErrorNeverFallsBack proves the
// deliberately narrow gate: a primary failure that is NOT an unknown-flag
// refusal (auth, policy, a generic non-zero exit) must never trigger the
// fallback attempt.
func TestProbeSbxVersion_UnrelatedPrimaryErrorNeverFallsBack(t *testing.T) {
	var lw strings.Builder
	fe := &createExecFakeExecutor{
		versionErrOut:      "Error: permission denied\n",
		versionErr:         errors.New("exit status 1"),
		versionFallbackOut: "sbx version 0.39\n", // must never be reached
	}

	_, err := probeSbxVersion(context.Background(), &lw, fe, nil, t.TempDir())
	if err == nil {
		t.Fatal("expected an unrelated primary failure to fail the probe, got nil")
	}
	if len(fe.calls) != 1 {
		t.Fatalf("expected NO fallback attempt for an unrelated primary error, got calls=%v", fe.calls)
	}
	if strings.Contains(err.Error(), "fallback") {
		t.Fatalf("an unrelated primary error must never be reported as a fallback failure, got: %v", err)
	}
}

// TestProbeSbxVersion_FallbackErrorFailsClosed proves a failing fallback
// attempt fails the probe closed rather than reporting a stale or partial
// version.
func TestProbeSbxVersion_FallbackErrorFailsClosed(t *testing.T) {
	var lw strings.Builder
	fe := &createExecFakeExecutor{
		versionErrOut:      "Error: unknown flag: --version\n",
		versionErr:         errors.New("exit status 1"),
		versionFallbackErr: errors.New("exit status 1"),
	}

	_, err := probeSbxVersion(context.Background(), &lw, fe, nil, t.TempDir())
	if err == nil {
		t.Fatal("expected a fallback failure to fail closed, got nil")
	}
	if len(fe.calls) != 2 {
		t.Fatalf("expected exactly one fallback attempt, got calls=%v", fe.calls)
	}
}

// TestProbeSbxVersion_EmptyPrimaryOutputFailsClosed proves a primary
// attempt that "succeeds" (no error) but carries no recognizable version
// text fails closed rather than being accepted as ready.
func TestProbeSbxVersion_EmptyPrimaryOutputFailsClosed(t *testing.T) {
	var lw strings.Builder
	fe := &createExecFakeExecutor{} // versionOut "" and versionErr nil

	_, err := probeSbxVersion(context.Background(), &lw, fe, nil, t.TempDir())
	if err == nil {
		t.Fatal("expected empty primary version output to fail closed, got nil")
	}
	if len(fe.calls) != 1 {
		t.Fatalf("expected no fallback attempt when the primary itself did not error, got calls=%v", fe.calls)
	}
}

// TestProbeSbxVersion_EmptyFallbackOutputFailsClosed proves the same
// fail-closed posture for the fallback attempt: a successful `sbx version`
// call with no recognizable version text still fails the probe.
func TestProbeSbxVersion_EmptyFallbackOutputFailsClosed(t *testing.T) {
	var lw strings.Builder
	fe := &createExecFakeExecutor{
		versionErrOut:      "Error: unknown flag: --version\n",
		versionErr:         errors.New("exit status 1"),
		versionFallbackOut: "", // empty, no error
	}

	_, err := probeSbxVersion(context.Background(), &lw, fe, nil, t.TempDir())
	if err == nil {
		t.Fatal("expected empty fallback version output to fail closed, got nil")
	}
}

// TestIsSbxVersionUnknownFlag_NarrowScope pins the narrow detection surface
// directly: only recognized parser vocabulary for "this flag does not
// exist" counts, never a generic "error"/"invalid" substring.
func TestIsSbxVersionUnknownFlag_NarrowScope(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   bool
	}{
		{"cobra unknown flag", "Error: unknown flag: --version\n", true},
		{"cobra unknown shorthand", "Error: unknown shorthand flag: 'v' in -version\n", true},
		{"stdlib flag package", "flag provided but not defined: -version\n", true},
		{"no such flag", "no such flag -version\n", true},
		{"permission denied", "Error: permission denied\n", false},
		{"generic invalid", "Error: invalid configuration\n", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSbxVersionUnknownFlag(tc.output); got != tc.want {
				t.Errorf("isSbxVersionUnknownFlag(%q) = %v, want %v", tc.output, got, tc.want)
			}
		})
	}
}
