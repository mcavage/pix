// check_create_exec_interpolation_test.go proves the bounded interpolation
// observation phase (check_create_exec_interpolation.go) that closes the
// final E0.7 host-evidence gap (AC-7): checkEnvironmentCreateThenExecInvocation
// still registers as exactly ONE named check (matrix_test.go's
// TestCheckNames_NonEmptyAndDerivesFromRegistry already pins CheckNames() at
// six entries), but now also observes native `${VAR}`, `${VAR:-default}`,
// and undefined-variable interpolation before it returns. These tests prove
// observeDefinedDefaultInterpolation and observeUndefinedVariableBehavior in
// isolation against an injected fake Executor — the same posture every
// other check-level test file in this package uses — plus the exact call
// order across all three fixtures the full check now drives.
package uatenvmatrix

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestEnvWithoutKey_RemovesKeyWithoutMutatingInput proves envWithoutKey
// returns a fresh slice with every entry named key removed, and never
// mutates the slice it was handed — the requirement that the interpolation
// phases never touch process env, only their own local copies.
func TestEnvWithoutKey_RemovesKeyWithoutMutatingInput(t *testing.T) {
	in := []string{"A=1", "PIX_UAT_STORY0_MISSING=leaked", "B=2"}
	inCopy := append([]string(nil), in...)

	out := envWithoutKey(in, "PIX_UAT_STORY0_MISSING")

	for _, e := range out {
		if strings.HasPrefix(e, "PIX_UAT_STORY0_MISSING=") {
			t.Fatalf("envWithoutKey did not remove the key: %#v", out)
		}
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 remaining entries, got %d: %#v", len(out), out)
	}
	for i := range in {
		if in[i] != inCopy[i] {
			t.Fatalf("envWithoutKey mutated its input slice: got %#v, want %#v", in, inCopy)
		}
	}
}

// TestEnvWithSetKey_AppendsDeterministicallyWithoutMutatingInput proves
// envWithSetKey replaces any existing entry for key and appends exactly one
// deterministic `key=value` entry, without mutating its input.
func TestEnvWithSetKey_AppendsDeterministicallyWithoutMutatingInput(t *testing.T) {
	in := []string{"A=1", "PIX_UAT_STORY0_DEFINED=stale-ambient-value", "B=2"}
	inCopy := append([]string(nil), in...)

	out := envWithSetKey(in, "PIX_UAT_STORY0_DEFINED", "known-value")

	count := 0
	for _, e := range out {
		if e == "PIX_UAT_STORY0_DEFINED=known-value" {
			count++
		}
		if e == "PIX_UAT_STORY0_DEFINED=stale-ambient-value" {
			t.Fatalf("envWithSetKey left the stale ambient value in place: %#v", out)
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one PIX_UAT_STORY0_DEFINED=known-value entry, got %d: %#v", count, out)
	}
	for i := range in {
		if in[i] != inCopy[i] {
			t.Fatalf("envWithSetKey mutated its input slice: got %#v, want %#v", in, inCopy)
		}
	}
	// Calling it again with the same inputs must produce byte-identical
	// output (deterministic), never depending on map iteration order or
	// similar.
	out2 := envWithSetKey(in, "PIX_UAT_STORY0_DEFINED", "known-value")
	if strings.Join(out, ",") != strings.Join(out2, ",") {
		t.Fatalf("envWithSetKey is not deterministic: %#v vs %#v", out, out2)
	}
}

// TestClassifyUndefinedVariableProbeOutput_RecognizesAllThreeShapes proves
// the three legitimate undefined-variable observation outcomes are each
// recognized and normalized, and that anything else is reported as an
// unrelated/inconclusive error rather than silently guessed at.
func TestClassifyUndefinedVariableProbeOutput_RecognizesAllThreeShapes(t *testing.T) {
	cases := []struct {
		name      string
		out       string
		wantWords []string
		wantErr   bool
	}{
		{"unset", "PIX_UAT_INTERP_MISSING:UNSET\n", []string{"unset"}, false},
		{"empty", "PIX_UAT_INTERP_MISSING:EMPTY\n", []string{"set-empty"}, false},
		{"literal-unexpanded", "PIX_UAT_INTERP_MISSING:VALUE=${PIX_UAT_STORY0_MISSING}\n", []string{"literal", "unexpanded"}, false},
		{"unrecognized-non-empty-value", "PIX_UAT_INTERP_MISSING:VALUE=some-other-value\n", nil, true},
		{"garbage", "not even close to the expected shape\n", nil, true},
		{"empty-string", "", nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := classifyUndefinedVariableProbeOutput(c.out)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected an unrelated/inconclusive error, got classification %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected a recognized classification, got error: %v", err)
			}
			for _, w := range c.wantWords {
				if !strings.Contains(got, w) {
					t.Errorf("classification %q does not contain expected word %q", got, w)
				}
			}
		})
	}
}

// TestBoundedRefusalReason_NormalizesAndTruncates proves the refusal-branch
// reason collapses whitespace, falls back to the bare error when stdout/
// stderr carry nothing, and is bounded rather than growing without limit.
func TestBoundedRefusalReason_NormalizesAndTruncates(t *testing.T) {
	reason := boundedRefusalReason("line one\n  line two  ", "", errors.New("exit status 1"))
	if !strings.Contains(reason, "line one") || !strings.Contains(reason, "line two") {
		t.Errorf("expected the normalized reason to retain both output lines, got %q", reason)
	}
	if strings.Contains(reason, "\n") {
		t.Errorf("expected whitespace to be collapsed, got %q", reason)
	}

	fallback := boundedRefusalReason("", "", errors.New("exit status 1"))
	if !strings.Contains(fallback, "exit status 1") {
		t.Errorf("expected the bare error to be used when stdout/stderr are empty, got %q", fallback)
	}

	huge := strings.Repeat("x", undefinedVariableRefusalReasonMaxLen*3)
	truncated := boundedRefusalReason(huge, "", nil)
	if len(truncated) > undefinedVariableRefusalReasonMaxLen+len(" ...(truncated)") {
		t.Errorf("expected the reason to be bounded to %d chars plus the truncation marker, got %d chars", undefinedVariableRefusalReasonMaxLen, len(truncated))
	}
	if !strings.Contains(truncated, "truncated") {
		t.Errorf("expected a truncation marker in the bounded reason, got %q", truncated)
	}
}

// interpDefinedFakeExecutor answers exactly the calls
// observeDefinedDefaultInterpolation issues, keyed on argv shape.
type interpDefinedFakeExecutor struct {
	createOut string
	createErr error
	execOut   string
	execErr   error
	execFn    func(ctx context.Context, args []string) (string, string, error)
	lsErr     error
	rmErr     error
	calls     [][]string
}

func (f *interpDefinedFakeExecutor) Run(ctx context.Context, name string, args, env []string, dir string) (string, string, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	switch {
	case len(args) > 0 && args[0] == "ls":
		if f.lsErr != nil {
			return "", "", f.lsErr
		}
		return runningRowJSON(interpDefinedDefaultFixtureName, "running"), "", nil
	case len(args) > 1 && args[0] == "env" && args[1] == "rm":
		return "", "", f.rmErr
	case len(args) > 1 && args[0] == "env" && args[1] == "create":
		return f.createOut, "", f.createErr
	default: // exec probe
		if f.execFn != nil {
			return f.execFn(ctx, args)
		}
		return f.execOut, "", f.execErr
	}
}

func fastInterpolationBounds(t *testing.T) {
	t.Helper()
	origPoll := runningRowPollConfig
	origProbe := echoProbeTimeout
	runningRowPollConfig = pollConfig{Interval: time.Millisecond, Timeout: time.Second}
	echoProbeTimeout = time.Second
	t.Cleanup(func() {
		runningRowPollConfig = origPoll
		echoProbeTimeout = origProbe
	})
}

// TestObserveDefinedDefaultInterpolation_Success proves the pinned success
// path: create succeeds, the poll observes a running row, the exec probe
// reports the EXACT known defined value and the EXACT literal default, the
// normalized behavior lines are logged, and receipt-gated cleanup removes
// the fixture.
func TestObserveDefinedDefaultInterpolation_Success(t *testing.T) {
	fastInterpolationBounds(t)
	phaseDir := t.TempDir()
	var lw strings.Builder
	fe := &interpDefinedFakeExecutor{
		createOut: "created " + interpDefinedDefaultFixtureName + " (positively identified)\n",
		execOut:   expectedInterpolationDefinedDefaultProbeOutput(),
	}

	if err := observeDefinedDefaultInterpolation(context.Background(), &lw, fe, phaseDir); err != nil {
		t.Fatalf("expected success, got: %v (log=%s)", err, lw.String())
	}

	logged := lw.String()
	if !strings.Contains(logged, `${PIX_UAT_STORY0_DEFINED} resolved to the exact known value "pix-uat-story0-defined-value"`) {
		t.Errorf("artifact does not record the exact known defined value: %s", logged)
	}
	if !strings.Contains(logged, `${PIX_UAT_STORY0_MISSING:-fallback-value} resolved to the exact literal default "fallback-value"`) {
		t.Errorf("artifact does not record the exact literal default: %s", logged)
	}
	if !strings.Contains(logged, "cleanup: removed "+interpDefinedDefaultFixtureName) {
		t.Errorf("artifact does not record the fixture was removed: %s", logged)
	}
}

// TestObserveDefinedDefaultInterpolation_CreateEnvSetsDefinedAndStripsMissing
// proves requirement 5 directly against the actual env slice handed to the
// create call: the defined host var carries the exact known value, the
// missing host var is entirely absent, and the ambient process env (which
// may itself define PIX_UAT_STORY0_MISSING) is never consulted for the
// decision to strip it.
func TestObserveDefinedDefaultInterpolation_CreateEnvSetsDefinedAndStripsMissing(t *testing.T) {
	t.Setenv("PIX_UAT_STORY0_MISSING", "leaked-from-host-daemon")
	fastInterpolationBounds(t)
	phaseDir := t.TempDir()
	var lw strings.Builder
	var createEnv []string
	fe := &interpDefinedFakeExecutor{
		createOut: "created " + interpDefinedDefaultFixtureName + " (positively identified)\n",
		execOut:   expectedInterpolationDefinedDefaultProbeOutput(),
	}
	executor := recordingEnvExecutor{inner: fe, onCreate: func(env []string) { createEnv = env }}

	if err := observeDefinedDefaultInterpolation(context.Background(), &lw, executor, phaseDir); err != nil {
		t.Fatalf("expected success, got: %v (log=%s)", err, lw.String())
	}

	wantDefined := "PIX_UAT_STORY0_DEFINED=pix-uat-story0-defined-value"
	foundDefined := false
	for _, e := range createEnv {
		if e == wantDefined {
			foundDefined = true
		}
		if strings.HasPrefix(e, "PIX_UAT_STORY0_MISSING=") {
			t.Fatalf("create env still carries PIX_UAT_STORY0_MISSING despite the ambient host value: %#v", createEnv)
		}
	}
	if !foundDefined {
		t.Fatalf("create env does not carry %q: %#v", wantDefined, createEnv)
	}
}

// recordingEnvExecutor wraps an inner Executor and reports the exact env
// slice handed to the ONE `sbx env create` call it observes, via onCreate.
type recordingEnvExecutor struct {
	inner    Executor
	onCreate func(env []string)
}

func (r recordingEnvExecutor) Run(ctx context.Context, name string, args, env []string, dir string) (string, string, error) {
	if len(args) > 1 && args[0] == "env" && args[1] == "create" && r.onCreate != nil {
		r.onCreate(env)
	}
	return r.inner.Run(ctx, name, args, env, dir)
}

// TestObserveDefinedDefaultInterpolation_CreateFailureFailsHard proves this
// fixture's outcome is PINNED, never ambiguous evidence like the
// undefined-variable fixture: any create failure fails the check outright.
func TestObserveDefinedDefaultInterpolation_CreateFailureFailsHard(t *testing.T) {
	fastInterpolationBounds(t)
	phaseDir := t.TempDir()
	var lw strings.Builder
	fe := &interpDefinedFakeExecutor{createErr: errors.New("sbx: internal error")}

	err := observeDefinedDefaultInterpolation(context.Background(), &lw, fe, phaseDir)
	if err == nil {
		t.Fatal("expected a create failure to fail this phase outright, got nil")
	}
	if strings.Contains(err.Error(), "refused") {
		t.Errorf("the defined/default fixture must never be treated as ambiguous 'refused' evidence: %v", err)
	}
}

// TestObserveDefinedDefaultInterpolation_ExecProbeMismatchFails proves a
// probe response that does not carry the exact expected values fails the
// phase, never silently accepted as "close enough".
func TestObserveDefinedDefaultInterpolation_ExecProbeMismatchFails(t *testing.T) {
	fastInterpolationBounds(t)
	phaseDir := t.TempDir()
	var lw strings.Builder
	fe := &interpDefinedFakeExecutor{
		createOut: "created " + interpDefinedDefaultFixtureName + " (positively identified)\n",
		execOut:   "PIX_UAT_INTERP_DEFINED=wrong-value\nPIX_UAT_INTERP_DEFAULT=fallback-value\n",
	}

	err := observeDefinedDefaultInterpolation(context.Background(), &lw, fe, phaseDir)
	if err == nil {
		t.Fatal("expected a mismatched probe output to fail the phase, got nil")
	}
	if !strings.Contains(err.Error(), "did not report the exact expected interpolated values") {
		t.Fatalf("expected an exact-match error, got: %v", err)
	}
}

// TestObserveDefinedDefaultInterpolation_ProbeTimeoutIsBoundedAndAttributed
// proves the exec probe reuses the SAME bounded echoProbeTimeout the
// primary fixture's probe uses, and reports an attributed timeout error
// rather than hanging.
func TestObserveDefinedDefaultInterpolation_ProbeTimeoutIsBoundedAndAttributed(t *testing.T) {
	origPoll := runningRowPollConfig
	origProbe := echoProbeTimeout
	runningRowPollConfig = pollConfig{Interval: time.Millisecond, Timeout: time.Second}
	echoProbeTimeout = 10 * time.Millisecond
	t.Cleanup(func() {
		runningRowPollConfig = origPoll
		echoProbeTimeout = origProbe
	})
	phaseDir := t.TempDir()
	var lw strings.Builder
	fe := &interpDefinedFakeExecutor{
		createOut: "created " + interpDefinedDefaultFixtureName + " (positively identified)\n",
		execFn: func(ctx context.Context, args []string) (string, string, error) {
			<-ctx.Done()
			return "", "", ctx.Err()
		},
	}

	start := time.Now()
	err := observeDefinedDefaultInterpolation(context.Background(), &lw, fe, phaseDir)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected the hung probe to fail the phase, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected an attributed timeout error, got: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the error to wrap context.DeadlineExceeded, got: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("expected the probe to be bounded to a few ms, took %s", elapsed)
	}
}

// interpMissingFakeExecutor answers exactly the calls
// observeUndefinedVariableBehavior issues, keyed on argv shape.
type interpMissingFakeExecutor struct {
	createOut string
	createErr error
	execOut   string
	execErr   error
	execFn    func(ctx context.Context, args []string) (string, string, error)
	lsErr     error
	rmErr     error
	calls     [][]string
}

func (f *interpMissingFakeExecutor) Run(ctx context.Context, name string, args, env []string, dir string) (string, string, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	switch {
	case len(args) > 0 && args[0] == "ls":
		if f.lsErr != nil {
			return "", "", f.lsErr
		}
		return runningRowJSON(interpMissingFixtureName, "running"), "", nil
	case len(args) > 1 && args[0] == "env" && args[1] == "rm":
		return "", "", f.rmErr
	case len(args) > 1 && args[0] == "env" && args[1] == "create":
		return f.createOut, "", f.createErr
	default: // exec probe
		if f.execFn != nil {
			return f.execFn(ctx, args)
		}
		return f.execOut, "", f.execErr
	}
}

// TestObserveUndefinedVariableBehavior_RefusalBranchLogsAndIssuesNoRemoval
// proves the first legitimate outcome: a create refusal before any positive
// receipt logs a normalized "refused: <bounded reason>" line and issues
// ZERO removal calls (cleanupCreatedFixture's own no-receipt branch), and
// the check itself succeeds (an accurate "refused" observation is not a
// check failure).
func TestObserveUndefinedVariableBehavior_RefusalBranchLogsAndIssuesNoRemoval(t *testing.T) {
	fastInterpolationBounds(t)
	phaseDir := t.TempDir()
	var lw strings.Builder
	fe := &interpMissingFakeExecutor{
		createErr: errors.New("exit status 1"),
		createOut: "",
	}

	if err := observeUndefinedVariableBehavior(context.Background(), &lw, fe, phaseDir); err != nil {
		t.Fatalf("expected the refusal branch to succeed, got: %v", err)
	}

	logged := lw.String()
	if !strings.Contains(logged, "undefined variable behavior: refused:") {
		t.Errorf("artifact does not record the normalized refusal line: %s", logged)
	}
	for _, call := range fe.calls {
		if len(call) > 1 && call[0] == "env" && call[1] == "rm" {
			t.Fatalf("refusal branch must issue NO removal call, got: %v", call)
		}
	}
}

// TestObserveUndefinedVariableBehavior_SuccessBranchClassifiesAndCleansUp
// proves the second legitimate outcome across all three classification
// shapes: a positive receipt, a running poll, a classified exec probe, the
// normalized behavior line, and receipt-gated cleanup that actually removes
// the fixture.
func TestObserveUndefinedVariableBehavior_SuccessBranchClassifiesAndCleansUp(t *testing.T) {
	cases := []struct {
		name      string
		execOut   string
		wantWords []string
	}{
		{"unset", undefinedVariableProbeUnsetToken + "\n", []string{"unset"}},
		{"empty", undefinedVariableProbeEmptyToken + "\n", []string{"set-empty"}},
		{"literal-unexpanded", undefinedVariableProbeValuePrefix + undefinedVariableLiteralUnexpanded + "\n", []string{"literal", "unexpanded"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fastInterpolationBounds(t)
			phaseDir := t.TempDir()
			var lw strings.Builder
			fe := &interpMissingFakeExecutor{
				createOut: "created " + interpMissingFixtureName + " (positively identified)\n",
				execOut:   c.execOut,
			}

			if err := observeUndefinedVariableBehavior(context.Background(), &lw, fe, phaseDir); err != nil {
				t.Fatalf("expected success, got: %v (log=%s)", err, lw.String())
			}

			logged := lw.String()
			if !strings.Contains(logged, "undefined variable behavior:") || strings.Contains(logged, "refused:") {
				t.Errorf("artifact does not record a normalized SUCCESS classification line: %s", logged)
			}
			for _, w := range c.wantWords {
				if !strings.Contains(logged, w) {
					t.Errorf("artifact classification does not contain %q: %s", w, logged)
				}
			}
			if !strings.Contains(logged, "cleanup: removed "+interpMissingFixtureName) {
				t.Errorf("artifact does not record the fixture was removed: %s", logged)
			}
		})
	}
}

// TestObserveUndefinedVariableBehavior_InconclusiveClassificationFails
// proves the third case: a create success whose exec probe output cannot be
// classified into any of the three legitimate shapes fails the check
// outright, per this file's own "any unrelated/inconclusive outcome fails"
// contract.
func TestObserveUndefinedVariableBehavior_InconclusiveClassificationFails(t *testing.T) {
	fastInterpolationBounds(t)
	phaseDir := t.TempDir()
	var lw strings.Builder
	fe := &interpMissingFakeExecutor{
		createOut: "created " + interpMissingFixtureName + " (positively identified)\n",
		execOut:   "nonsense output the classifier has never seen\n",
	}

	err := observeUndefinedVariableBehavior(context.Background(), &lw, fe, phaseDir)
	if err == nil {
		t.Fatal("expected an unclassifiable probe output to fail the check, got nil")
	}
	if !strings.Contains(err.Error(), "unrelated/inconclusive") {
		t.Fatalf("expected an unrelated/inconclusive error, got: %v", err)
	}
}

// TestObserveUndefinedVariableBehavior_CreateSucceedsButUnidentifiedFails
// proves a create call that neither refuses (err == nil) nor positively
// identifies the fixture's name is treated as unrelated/inconclusive, never
// silently accepted as either legitimate outcome.
func TestObserveUndefinedVariableBehavior_CreateSucceedsButUnidentifiedFails(t *testing.T) {
	fastInterpolationBounds(t)
	phaseDir := t.TempDir()
	var lw strings.Builder
	fe := &interpMissingFakeExecutor{createOut: "accepted\n"}

	err := observeUndefinedVariableBehavior(context.Background(), &lw, fe, phaseDir)
	if err == nil {
		t.Fatal("expected an unidentified create success to fail the check, got nil")
	}
	if !strings.Contains(err.Error(), "unrelated/inconclusive") {
		t.Fatalf("expected an unrelated/inconclusive error, got: %v", err)
	}
}

// TestObserveUndefinedVariableBehavior_ProbeTimeoutFails proves the success
// branch's own exec probe is ALSO bounded by the SAME echoProbeTimeout, and
// fails with an attributed timeout rather than hanging.
func TestObserveUndefinedVariableBehavior_ProbeTimeoutFails(t *testing.T) {
	origPoll := runningRowPollConfig
	origProbe := echoProbeTimeout
	runningRowPollConfig = pollConfig{Interval: time.Millisecond, Timeout: time.Second}
	echoProbeTimeout = 10 * time.Millisecond
	t.Cleanup(func() {
		runningRowPollConfig = origPoll
		echoProbeTimeout = origProbe
	})
	phaseDir := t.TempDir()
	var lw strings.Builder
	fe := &interpMissingFakeExecutor{
		createOut: "created " + interpMissingFixtureName + " (positively identified)\n",
		execFn: func(ctx context.Context, args []string) (string, string, error) {
			<-ctx.Done()
			return "", "", ctx.Err()
		},
	}

	err := observeUndefinedVariableBehavior(context.Background(), &lw, fe, phaseDir)
	if err == nil {
		t.Fatal("expected the hung probe to fail the phase, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected an attributed timeout error, got: %v", err)
	}
}

// TestObserveUndefinedVariableBehavior_PollTimeoutFails proves a create
// success that never reaches a positively identified running row fails the
// check — a poll timeout is infrastructure breakage, never one of the two
// legitimate observation outcomes.
func TestObserveUndefinedVariableBehavior_PollTimeoutFails(t *testing.T) {
	origPoll := runningRowPollConfig
	origProbe := echoProbeTimeout
	runningRowPollConfig = pollConfig{Interval: time.Millisecond, Timeout: 10 * time.Millisecond}
	echoProbeTimeout = time.Second
	t.Cleanup(func() {
		runningRowPollConfig = origPoll
		echoProbeTimeout = origProbe
	})
	phaseDir := t.TempDir()
	var lw strings.Builder
	fe := &interpMissingFakeExecutor{
		createOut: "created " + interpMissingFixtureName + " (positively identified)\n",
		lsErr:     errors.New("dial tcp: connection refused"),
	}

	err := observeUndefinedVariableBehavior(context.Background(), &lw, fe, phaseDir)
	if err == nil {
		t.Fatal("expected a poll timeout to fail the check, got nil")
	}
	if !strings.Contains(err.Error(), "poll") || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected an attributed poll-timeout error, got: %v", err)
	}
}

// TestCheckEnvironmentCreateThenExecInvocation_FullExactCallOrder pins the
// exact call order across ALL THREE fixtures the check now drives: the
// primary create/exec proof, THEN the defined/default interpolation phase
// (create, poll, exec, its own cleanup probe+remove), THEN the
// undefined-variable phase (create, poll, exec, its own cleanup
// probe+remove), and ONLY THEN the primary fixture's own deferred cleanup
// (probe+remove) — 16 calls total, matching matrix_test.go's own pinned
// count for this check.
func TestCheckEnvironmentCreateThenExecInvocation_FullExactCallOrder(t *testing.T) {
	fastPollAndProbeBounds(t, pollConfig{Interval: time.Millisecond, Timeout: time.Second}, time.Second)
	phaseDir := t.TempDir()
	var lw strings.Builder
	fe, _ := fakeExecutorForSuccess(t)

	if err := checkEnvironmentCreateThenExecInvocation(context.Background(), &lw, fe, phaseDir); err != nil {
		t.Fatalf("expected success, got: %v (log=%s)", err, lw.String())
	}

	if len(fe.calls) != 16 {
		t.Fatalf("expected exactly 16 executor calls across all three fixtures, got %d: %#v", len(fe.calls), fe.calls)
	}

	shape := func(i int) string {
		c := fe.calls[i]
		if len(c) == 0 {
			return ""
		}
		if len(c) > 1 && c[0] == "env" {
			return strings.Join(c[:2], " ")
		}
		return c[0]
	}
	wantShapes := []string{
		"", "env create", "ls", "exec", // primary: version(no shape check), create, poll, exec
		"env create", "ls", "exec", "ls", "env rm", // phase 2: create, poll, exec, cleanup-probe, remove
		"env create", "ls", "exec", "ls", "env rm", // phase 3: create, poll, exec, cleanup-probe, remove
		"ls", "env rm", // primary's own deferred cleanup: probe, remove
	}
	if len(wantShapes) != 16 {
		t.Fatalf("test bug: wantShapes has %d entries, want 16", len(wantShapes))
	}
	for i, want := range wantShapes {
		if want == "" {
			continue // index 0 is the version probe; already asserted elsewhere
		}
		if got := shape(i); got != want {
			t.Errorf("call[%d] shape = %q, want %q (full calls=%#v)", i, got, want, fe.calls)
		}
	}

	// The second and third fixtures' create calls must name the exact
	// interpolation fixtures, in order.
	if !strings.HasSuffix(fe.calls[4][2], "interp-defined-default.sbxenv.yaml") {
		t.Errorf("call[4] did not create the defined/default fixture: %#v", fe.calls[4])
	}
	if !strings.HasSuffix(fe.calls[9][2], "interp-missing.sbxenv.yaml") {
		t.Errorf("call[9] did not create the undefined-variable fixture: %#v", fe.calls[9])
	}
}
