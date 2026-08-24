// check_create_exec_test.go proves checkEnvironmentCreateThenExecInvocation's
// OWN receipt-gated cleanup wiring in isolation against an injected fake
// Executor — the same posture check_local_image_test.go and
// check_recreate_boundary_test.go use, so this unit's tests never need a
// real `sbx` binary or Run()'s full registry. matrix_test.go and
// cleanup_test.go already prove the happy path and the shared helper's own
// branches respectively; these tests prove the check-level WIRING: cleanup
// runs on both success and downstream failure, and never masks whichever
// error was already primary.
//
// This file also proves the second fresh host-backed Story 0 failure (host
// run run-20260824-082810-d9f64946): the prior check treated an
// interactive, non-terminating pi TUI's bare process exit as transport
// proof, passed pi a `--kit` flag pi does not have, and used a valued
// `--resume` (a bare picker flag). See check_create_exec.go's doc comment
// for the full corrected contract these tests pin.
package uatenvmatrix

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// createExecFakeExecutor answers create/exec/ls/rm calls independently, so
// each test can compose exactly the failure combination it needs.
type createExecFakeExecutor struct {
	createOut string
	createErr error

	// execOut/execErr answer the check's actual transport probe: a
	// non-TTY, name-based `sbx exec -i` argv-echo probe (never the
	// production `-it` shape — see buildEchoProbeArgv). execFn, when set,
	// takes precedence and lets a test simulate a hung probe honoring ctx.
	execOut string
	execErr error
	execFn  func(ctx context.Context, args []string) (string, string, error)

	// lsOut/lsErr answer every `sbx ls --json` call: both
	// pollForRunningInstance's pre-exec poll and cleanupCreatedFixture's own
	// post-exec fresh probe. lsSequence, when non-empty, answers successive
	// ls calls in order (each entry's error is always nil) — so a test can
	// prove the poll actually waits across several non-running attempts
	// before a running row shows up. Once lsSequence is exhausted, lsErr (if
	// set) answers every later ls call instead of repeating the sequence's
	// final entry, so a test can distinguish the poll's own rows from a
	// LATER cleanup fresh-probe failure that must fail independently.
	lsOut      string
	lsErr      error
	lsSequence []string
	lsCalls    int

	rmErr error
	calls [][]string
}

func (f *createExecFakeExecutor) Run(ctx context.Context, name string, args, env []string, dir string) (string, string, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	switch {
	case len(args) > 0 && args[0] == "ls":
		if len(f.lsSequence) > 0 {
			idx := f.lsCalls
			f.lsCalls++
			if idx < len(f.lsSequence) {
				return f.lsSequence[idx], "", nil
			}
			if f.lsErr != nil {
				return "", "", f.lsErr
			}
			return f.lsSequence[len(f.lsSequence)-1], "", nil
		}
		f.lsCalls++
		return f.lsOut, "", f.lsErr
	case len(args) > 1 && args[0] == "env" && args[1] == "rm":
		return "", "", f.rmErr
	case len(args) > 1 && args[0] == "env" && args[1] == "create":
		return f.createOut, "", f.createErr
	default: // name-based `sbx exec` (the argv-echo probe)
		if f.execFn != nil {
			return f.execFn(ctx, args)
		}
		return f.execOut, "", f.execErr
	}
}

// runningRowJSON renders a minimal `sbx ls --json` body containing exactly
// one schema-usable row for name at status, in the bare-array canonical
// shape findRunningRow accepts.
func runningRowJSON(name, status string) string {
	return fmt.Sprintf(`[{"name":%q,"status":%q}]`, name, status)
}

// createReceiptOut renders a create receipt that positively identifies both
// name and every declared relative kit — the shape createOutputIdentifiesKit
// requires so a fixture's generated-kit facet is never silently assumed.
func createReceiptOut(name string, kits []string) string {
	out := "created " + name + " (positively identified)"
	for _, k := range kits {
		out += " kit " + k
	}
	return out + "\n"
}

// fastPollAndProbeBounds overrides the package's production poll/timeout
// bounds with fast, deterministic ones for the duration of a test, and
// restores the originals on cleanup — the injectable bounds finding #5 asks
// for, applied at the narrowest layer these tests need.
func fastPollAndProbeBounds(t *testing.T, poll pollConfig, probeTimeout time.Duration) {
	t.Helper()
	origPoll := runningRowPollConfig
	origProbe := echoProbeTimeout
	runningRowPollConfig = poll
	echoProbeTimeout = probeTimeout
	t.Cleanup(func() {
		runningRowPollConfig = origPoll
		echoProbeTimeout = origProbe
	})
}

func fakeExecutorForSuccess(t *testing.T) (*createExecFakeExecutor, string) {
	t.Helper()
	fixture := customAgentFixture()
	name := fixture.Name
	return &createExecFakeExecutor{
		createOut: createReceiptOut(name, fixture.RelativeKits),
		lsOut:     runningRowJSON(name, "running"),
		execOut:   expectedEchoProbeOutput(intendedPiInvocation(fixture)),
	}, name
}

// TestCheckEnvironmentCreateThenExecInvocation_SuccessRemovesFixture proves
// the full success path: a receipted create that identifies the declared
// kit, a poll that observes a running row, a successful argv-echo probe, a
// fresh probe reconfirming the same identity, then an environment-scoped
// removal — the check succeeds AND the artifact records the fixture was
// removed, so this check leaks nothing on the happy path.
func TestCheckEnvironmentCreateThenExecInvocation_SuccessRemovesFixture(t *testing.T) {
	fastPollAndProbeBounds(t, pollConfig{Interval: time.Millisecond, Timeout: time.Second}, time.Second)
	phaseDir := t.TempDir()
	var lw strings.Builder
	fe, name := fakeExecutorForSuccess(t)

	if err := checkEnvironmentCreateThenExecInvocation(context.Background(), &lw, fe, phaseDir); err != nil {
		t.Fatalf("expected success, got: %v (log=%s)", err, lw.String())
	}
	if !strings.Contains(lw.String(), "cleanup: removed "+name) {
		t.Errorf("artifact does not record the fixture was removed: %s", lw.String())
	}
}

// TestBuildExecArgv_ExactProductionShape pins the pure production
// expected-argv builder's exact shape: `exec -it <name> -- pi --skill ...
// --model ... --session ...`, with no pi `--kit` flag (kit is proven from
// create's own resolution, never re-asserted to pi) and no valued `--resume`
// (`--resume` is a bare picker flag that takes none; the exact resume
// target travels as `--session <id>`).
func TestBuildExecArgv_ExactProductionShape(t *testing.T) {
	fixture := customAgentFixture()
	got := buildExecArgv(fixture)
	want := []string{
		"exec", "-it", fixture.Name, "--", "pi",
		"--skill", "/opt/pix/kit/skills",
		"--skill", "/home/uat/personal-context/skills",
		"--model", "anthropic/claude-sonnet-5",
		"--session", "session-fixture-1",
	}
	if len(got) != len(want) {
		t.Fatalf("buildExecArgv length = %d, want %d\ngot:  %#v\nwant: %#v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("buildExecArgv[%d] = %q, want %q\ngot:  %#v\nwant: %#v", i, got[i], want[i], got, want)
		}
	}
	joined := strings.Join(got, " ")
	if strings.Contains(joined, "--kit") {
		t.Errorf("buildExecArgv must never pass pi a --kit flag (kit is proven from create's own resolution): %v", got)
	}
	if strings.Contains(joined, "--resume") {
		t.Errorf("buildExecArgv must never use --resume (a bare picker flag that takes no value); the exact resume target must travel as --session: %v", got)
	}
}

// TestCheckEnvironmentCreateThenExecInvocation_PollHappensBeforeExec proves
// finding #5 directly: the check waits across several non-running `sbx ls
// --json` rows before a running one appears, and the argv-echo probe is
// issued only AFTER the poll observes it — never before, and never by
// skipping the poll outright.
func TestCheckEnvironmentCreateThenExecInvocation_PollHappensBeforeExec(t *testing.T) {
	fastPollAndProbeBounds(t, pollConfig{Interval: time.Millisecond, Timeout: 2 * time.Second}, time.Second)
	phaseDir := t.TempDir()
	var lw strings.Builder
	fixture := customAgentFixture()
	name := fixture.Name
	fe := &createExecFakeExecutor{
		createOut:  createReceiptOut(name, fixture.RelativeKits),
		lsSequence: []string{runningRowJSON(name, "creating"), runningRowJSON(name, "creating"), runningRowJSON(name, "running")},
		execOut:    expectedEchoProbeOutput(intendedPiInvocation(fixture)),
	}

	if err := checkEnvironmentCreateThenExecInvocation(context.Background(), &lw, fe, phaseDir); err != nil {
		t.Fatalf("expected success once the poll observes a running row, got: %v (log=%s)", err, lw.String())
	}

	var lsCallsBeforeExec, execIndex, lsIndex int
	execIndex, lsIndex = -1, -1
	for i, call := range fe.calls {
		if len(call) > 0 && call[0] == "ls" && execIndex == -1 {
			lsCallsBeforeExec++
			lsIndex = i
		}
		if len(call) > 0 && call[0] == "exec" && execIndex == -1 {
			execIndex = i
		}
	}
	if lsCallsBeforeExec < 3 {
		t.Errorf("expected at least 3 poll `ls` calls before the argv-echo probe (2 non-running + 1 running), got %d (calls=%v)", lsCallsBeforeExec, fe.calls)
	}
	if execIndex == -1 {
		t.Fatalf("expected an `exec` call after the poll observed a running row; calls=%v", fe.calls)
	}
	if lsIndex >= execIndex {
		t.Fatalf("expected the poll's `ls` calls to precede the `exec` argv-echo probe; calls=%v", fe.calls)
	}
}

// TestCheckEnvironmentCreateThenExecInvocation_PollTimeoutNeverExecs proves
// finding #5's other half: if the poll never observes a positively
// identified running row before its bound elapses, the check must fail
// closed WITHOUT ever issuing the exec argv-echo probe, and receipt-gated
// cleanup must still evaluate (primary-error precedence: the poll timeout
// remains the reported cause).
func TestCheckEnvironmentCreateThenExecInvocation_PollTimeoutNeverExecs(t *testing.T) {
	fastPollAndProbeBounds(t, pollConfig{Interval: time.Millisecond, Timeout: 10 * time.Millisecond}, time.Second)
	phaseDir := t.TempDir()
	var lw strings.Builder
	fixture := customAgentFixture()
	name := fixture.Name
	fe := &createExecFakeExecutor{
		createOut: createReceiptOut(name, fixture.RelativeKits),
		lsOut:     runningRowJSON(name, "creating"), // never transitions to running
	}

	err := checkEnvironmentCreateThenExecInvocation(context.Background(), &lw, fe, phaseDir)
	if err == nil {
		t.Fatal("expected the poll timeout to fail the check, got nil")
	}
	if !strings.Contains(err.Error(), "poll") || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected an attributed poll-timeout error, got: %v", err)
	}
	for _, call := range fe.calls {
		if len(call) > 0 && call[0] == "exec" {
			t.Fatalf("poll timeout must never issue the exec argv-echo probe, but it did: %v", call)
		}
	}
	if !strings.Contains(lw.String(), "cleanup: evaluating") {
		t.Errorf("cleanup must still evaluate on a poll timeout: %s", lw.String())
	}
}

// TestCheckEnvironmentCreateThenExecInvocation_ProbeUsesNonTTYExecAndExactArgv
// proves finding #4 directly: the actual transport proof is a non-TTY
// `sbx exec -i <name> --` (never `-it`), and a probe response missing or
// mangling even one intended argv facet fails the check outright rather
// than being treated as a benign partial match.
func TestCheckEnvironmentCreateThenExecInvocation_ProbeUsesNonTTYExecAndExactArgv(t *testing.T) {
	fastPollAndProbeBounds(t, pollConfig{Interval: time.Millisecond, Timeout: time.Second}, time.Second)
	phaseDir := t.TempDir()
	var lw strings.Builder
	fixture := customAgentFixture()
	name := fixture.Name
	intended := intendedPiInvocation(fixture)
	mangled := append([]string(nil), intended...)
	mangled[len(mangled)-1] = "session-DIFFERENT" // mangle the --session value
	fe := &createExecFakeExecutor{
		createOut: createReceiptOut(name, fixture.RelativeKits),
		lsOut:     runningRowJSON(name, "running"),
		execOut:   expectedEchoProbeOutput(mangled),
	}

	err := checkEnvironmentCreateThenExecInvocation(context.Background(), &lw, fe, phaseDir)
	if err == nil {
		t.Fatal("expected a mangled echoed facet to fail the check, got nil")
	}
	if !strings.Contains(err.Error(), "did not echo the exact intended pi invocation") {
		t.Fatalf("expected an exact-echo mismatch error, got: %v", err)
	}

	var execArgs []string
	for _, call := range fe.calls {
		if len(call) > 0 && call[0] == "exec" {
			execArgs = call
			break
		}
	}
	if execArgs == nil {
		t.Fatalf("expected an `exec` call, got calls=%v", fe.calls)
	}
	if execArgs[1] != "-i" {
		t.Fatalf("expected the actual transport probe to use non-TTY `exec -i`, got flag %q in %v", execArgs[1], execArgs)
	}
	for _, arg := range execArgs {
		if arg == "-it" {
			t.Fatalf("the actual transport probe must never use `-it` (that shape is production's, pinned separately by buildExecArgv, and hangs under a pty-less daemon): %v", execArgs)
		}
	}
}

// TestCheckEnvironmentCreateThenExecInvocation_HungProbeIsBoundedAndAttributed
// proves the probe is bounded by a context timeout and reports an
// attributed error on timeout, rather than hanging the check the way the
// original interactive TUI invocation did (`ERROR: inspect exec: context
// deadline exceeded` with no attribution to a bounded probe design at all).
func TestCheckEnvironmentCreateThenExecInvocation_HungProbeIsBoundedAndAttributed(t *testing.T) {
	fastPollAndProbeBounds(t, pollConfig{Interval: time.Millisecond, Timeout: time.Second}, 10*time.Millisecond)
	phaseDir := t.TempDir()
	var lw strings.Builder
	fixture := customAgentFixture()
	name := fixture.Name
	fe := &createExecFakeExecutor{
		createOut: createReceiptOut(name, fixture.RelativeKits),
		lsOut:     runningRowJSON(name, "running"),
		execFn: func(ctx context.Context, args []string) (string, string, error) {
			<-ctx.Done()
			return "", "", ctx.Err()
		},
	}

	start := time.Now()
	err := checkEnvironmentCreateThenExecInvocation(context.Background(), &lw, fe, phaseDir)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected the hung probe to fail the check, got nil")
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
	if !strings.Contains(lw.String(), "cleanup: evaluating") {
		t.Errorf("cleanup must still evaluate after a bounded probe timeout: %s", lw.String())
	}
}

// TestCheckEnvironmentCreateThenExecInvocation_CreateOutputMustIdentifyKit
// proves finding #2: a create receipt that positively identifies the
// fixture's name but never mentions its declared kit must fail the check —
// the generated-kit facet must never be silently dropped, since pi itself
// is never told --kit and create's own resolution is the ONLY place this
// check can observe that fact.
func TestCheckEnvironmentCreateThenExecInvocation_CreateOutputMustIdentifyKit(t *testing.T) {
	fastPollAndProbeBounds(t, pollConfig{Interval: time.Millisecond, Timeout: time.Second}, time.Second)
	phaseDir := t.TempDir()
	var lw strings.Builder
	fixture := customAgentFixture()
	name := fixture.Name
	fe := &createExecFakeExecutor{
		createOut: "created " + name + " (positively identified)\n", // no kit facet
	}

	err := checkEnvironmentCreateThenExecInvocation(context.Background(), &lw, fe, phaseDir)
	if err == nil {
		t.Fatal("expected a create receipt missing the kit facet to fail the check, got nil")
	}
	if !strings.Contains(err.Error(), "kit") {
		t.Fatalf("expected an error naming the missing kit facet, got: %v", err)
	}
}

// TestCheckEnvironmentCreateThenExecInvocation_FreshProbeFailureFailsTheCheck
// proves a receipted create followed by a successful poll and argv-echo
// probe is NOT enough on its own: if the fresh post-exec probe cannot
// reconfirm the same identity, the check itself must fail (a real,
// undetected leak) rather than silently reporting success while residue
// lives on.
func TestCheckEnvironmentCreateThenExecInvocation_FreshProbeFailureFailsTheCheck(t *testing.T) {
	fastPollAndProbeBounds(t, pollConfig{Interval: time.Millisecond, Timeout: time.Second}, time.Second)
	phaseDir := t.TempDir()
	var lw strings.Builder
	fixture := customAgentFixture()
	name := fixture.Name
	fe := &createExecFakeExecutor{
		createOut: createReceiptOut(name, fixture.RelativeKits),
		// The poll needs one running row to proceed; cleanup's OWN fresh
		// probe must fail independently of the poll's. lsSequence supplies
		// the poll's single running row, then errors on every later `ls`
		// call (cleanup's fresh probe) via lsErr, since lsSequence never
		// itself carries a per-entry error.
		lsSequence: []string{runningRowJSON(name, "running")},
		lsErr:      errors.New("dial tcp: connection refused"),
		execOut:    expectedEchoProbeOutput(intendedPiInvocation(fixture)),
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
// requirement directly: the argv-echo probe fails (the primary error), and
// cleanup's own fresh probe ALSO fails to reconfirm the identity — the
// returned error must still be the ORIGINAL probe failure, with the cleanup
// evidence recorded only in the artifact, never substituted as the reported
// cause.
func TestCheckEnvironmentCreateThenExecInvocation_CleanupNeverMasksExecFailure(t *testing.T) {
	fastPollAndProbeBounds(t, pollConfig{Interval: time.Millisecond, Timeout: time.Second}, time.Second)
	phaseDir := t.TempDir()
	var lw strings.Builder
	fixture := customAgentFixture()
	name := fixture.Name
	fe := &createExecFakeExecutor{
		createOut:  createReceiptOut(name, fixture.RelativeKits),
		lsSequence: []string{runningRowJSON(name, "running")},
		lsErr:      errors.New("dial tcp: connection refused (probe)"),
		execErr:    errors.New("dial tcp: connection refused (exec)"),
	}

	err := checkEnvironmentCreateThenExecInvocation(context.Background(), &lw, fe, phaseDir)
	if err == nil {
		t.Fatal("expected the exec failure to fail the check, got nil")
	}
	if !strings.Contains(err.Error(), "name-based sbx exec argv-echo probe") {
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
// surfacing, not silently accepted because create and the probe both
// succeeded.
func TestCheckEnvironmentCreateThenExecInvocation_RemovalCommandFailureFailsTheCheck(t *testing.T) {
	fastPollAndProbeBounds(t, pollConfig{Interval: time.Millisecond, Timeout: time.Second}, time.Second)
	phaseDir := t.TempDir()
	var lw strings.Builder
	fe, _ := fakeExecutorForSuccess(t)
	fe.rmErr = errors.New("sbx: exit status 1")

	err := checkEnvironmentCreateThenExecInvocation(context.Background(), &lw, fe, phaseDir)
	if err == nil {
		t.Fatal("expected the removal command failure to fail the check, got nil")
	}
	if !strings.Contains(err.Error(), "sbx env rm -f") {
		t.Fatalf("expected the removal failure to name the environment-scoped command, got: %v", err)
	}
}
