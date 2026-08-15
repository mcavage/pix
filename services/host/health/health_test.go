package health

import (
	"context"
	"strings"
	"testing"
	"time"
)

// staticProbe is not a mock of a boundary: it asserts a Result directly, which
// is what a probe IS at this layer. The boundary-crossing probes are exercised
// against real executables and real listeners in probes_test.go.
type staticProbe struct {
	name     string
	required bool
	res      Result
	block    time.Duration
}

func (p staticProbe) Name() string   { return p.name }
func (p staticProbe) Required() bool { return p.required }

func (p staticProbe) Check(ctx context.Context) Result {
	if p.block > 0 {
		select {
		case <-time.After(p.block):
		case <-ctx.Done():
			return Result{Name: p.name, Status: StatusUnknown, Detail: "cancelled"}
		}
	}
	r := p.res
	r.Name = p.name
	return r
}

func find(t *testing.T, s Snapshot, name string) Result {
	t.Helper()
	for _, r := range s.Results {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no result named %q in %v", name, s.Names())
	return Result{}
}

// TestUnknownIsNeitherReadyNorAbsent is the load-bearing invariant of the whole
// model: "I could not check" must never collapse into either "it works" or
// "it is missing".
func TestUnknownIsNeitherReadyNorAbsent(t *testing.T) {
	r := Result{Name: "sbx", Status: StatusUnknown, Required: true}
	if r.OK() {
		t.Error("an unknown result must never report OK")
	}
	if r.Missing() {
		t.Error("an unknown result must never report Missing")
	}
	if r.Blocking() {
		t.Error("an unknown result must never block, even when required")
	}
	// The zero value is the fail-safe direction: unset reads unknown, never ready.
	var zero Result
	if zero.Effective() != StatusUnknown {
		t.Errorf("zero Result status = %q, want unknown", zero.Effective())
	}
	if zero.OK() {
		t.Error("the zero Result must never read as ready")
	}
}

// TestUnknownAloneIsNotFailure: a snapshot of nothing but unknowns is not ready
// (nothing was proven) but it does not fail the process either.
func TestUnknownAloneIsNotFailure(t *testing.T) {
	s := Run(context.Background(), time.Second,
		staticProbe{name: "sbx", required: true, res: Result{Status: StatusUnknown, Detail: "sbx not reachable from here"}},
		staticProbe{name: "pack", res: Result{Status: StatusUnknown}},
	)
	if s.Ready() {
		t.Error("unknown must not count as ready")
	}
	if got := s.ExitCode(); got != ExitOK {
		t.Errorf("unknown-only snapshot exit = %d, want %d", got, ExitOK)
	}
	if len(s.Blocking()) != 0 {
		t.Errorf("unknown-only snapshot must have no blocking results, got %v", s.Blocking())
	}
	if len(s.Unknown()) != 2 {
		t.Errorf("Unknown() = %d, want 2", len(s.Unknown()))
	}
}

// TestOnlyVerifiedRequiredFailureExits1 pins the exit contract: a verified
// absence of a REQUIRED probe is the only thing that fails.
func TestOnlyVerifiedRequiredFailureExits1(t *testing.T) {
	cases := []struct {
		name     string
		status   Status
		required bool
		want     int
	}{
		{"required absent", StatusAbsent, true, ExitNotReady},
		{"required denied", StatusDenied, true, ExitNotReady},
		{"required unknown", StatusUnknown, true, ExitOK},
		{"optional absent", StatusAbsent, false, ExitOK},
		{"optional denied", StatusDenied, false, ExitOK},
		{"required ready", StatusReady, true, ExitOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := Run(context.Background(), time.Second,
				staticProbe{name: "p", required: tc.required, res: Result{Status: tc.status, Fix: "pix fix it"}})
			if got := s.ExitCode(); got != tc.want {
				t.Errorf("exit = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestFixesOnlyFromVerifiedGaps: an unknown or ready result may never emit a
// copy-pasteable repair command — that is how a green report ends up printing
// a TODO under it.
func TestFixesOnlyFromVerifiedGaps(t *testing.T) {
	s := Run(context.Background(), time.Second,
		staticProbe{name: "ready", required: true, res: Result{Status: StatusReady, Fix: "pix never-print-me"}},
		staticProbe{name: "unknown", required: true, res: Result{Status: StatusUnknown, Fix: "pix never-print-me-either"}},
		staticProbe{name: "absent", required: true, res: Result{Status: StatusAbsent, Fix: "pix serve install"}},
		staticProbe{name: "dupe", required: true, res: Result{Status: StatusAbsent, Fix: "pix serve install"}},
	)
	fixes := s.Fixes()
	if len(fixes) != 1 || fixes[0] != "pix serve install" {
		t.Fatalf("Fixes() = %v, want exactly [pix serve install]", fixes)
	}
	if got := find(t, s, "ready").Fix; got != "" {
		t.Errorf("a ready result kept a fix: %q", got)
	}
	if got := find(t, s, "unknown").Fix; got != "" {
		t.Errorf("an unknown result kept a fix: %q", got)
	}
}

// TestRunBoundsEveryProbe: one wedged probe must not wedge the command. The
// probe is cancelled at the budget and reported unknown — never absent.
func TestRunBoundsEveryProbe(t *testing.T) {
	start := time.Now()
	s := Run(context.Background(), 60*time.Millisecond,
		staticProbe{name: "wedged", required: true, block: 10 * time.Second},
		staticProbe{name: "fast", required: true, res: Result{Status: StatusReady}},
	)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Run did not bound the wedged probe: took %s", elapsed)
	}
	w := find(t, s, "wedged")
	if w.Effective() != StatusUnknown {
		t.Errorf("wedged probe = %q, want unknown", w.Effective())
	}
	if s.ExitCode() != ExitOK {
		t.Errorf("a timed-out probe must not fail the process, exit = %d", s.ExitCode())
	}
	if find(t, s, "fast").Effective() != StatusReady {
		t.Error("a wedged probe must not poison its neighbours")
	}
}

// TestRunHonoursCallerCancellation: the caller's context still wins even when
// it is shorter than the per-probe budget.
func TestRunHonoursCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	s := Run(ctx, 10*time.Second, staticProbe{name: "wedged", required: true, block: 5 * time.Second})
	if find(t, s, "wedged").Effective() != StatusUnknown {
		t.Error("a cancelled probe must report unknown")
	}
}

// TestRunPreservesOrderAndPanicIsUnknown: probe order is the render order, and
// a probe that panics degrades to unknown instead of taking the command down.
func TestRunPreservesOrderAndPanicIsUnknown(t *testing.T) {
	s := Run(context.Background(), time.Second,
		staticProbe{name: "a", res: Result{Status: StatusReady}},
		panicProbe{},
		staticProbe{name: "c", res: Result{Status: StatusReady}},
	)
	if got := strings.Join(s.Names(), ","); got != "a,boom,c" {
		t.Errorf("order = %s, want a,boom,c", got)
	}
	if find(t, s, "boom").Effective() != StatusUnknown {
		t.Error("a panicking probe must report unknown, not crash the command")
	}
}

type panicProbe struct{}

func (panicProbe) Name() string                 { return "boom" }
func (panicProbe) Required() bool               { return true }
func (panicProbe) Check(context.Context) Result { panic("probe exploded") }

// TestReadyRequiresEveryRequiredProbeProven: an optional gap does not make the
// host un-ready, an unproven required one does.
func TestReadyRequiresEveryRequiredProbeProven(t *testing.T) {
	ok := Run(context.Background(), time.Second,
		staticProbe{name: "core", required: true, res: Result{Status: StatusReady}},
		staticProbe{name: "opt", res: Result{Status: StatusAbsent, Fix: "pix pack use <path>"}},
	)
	if !ok.Ready() {
		t.Error("an optional gap must not make the host un-ready")
	}
	no := Run(context.Background(), time.Second,
		staticProbe{name: "core", required: true, res: Result{Status: StatusUnknown}})
	if no.Ready() {
		t.Error("an unproven required probe must not report ready")
	}
}

func TestRenderStatusIsShortAndFixFree(t *testing.T) {
	s := Run(context.Background(), time.Second,
		staticProbe{name: "sbx", required: true, res: Result{Status: StatusReady, Detail: "1.2.3"}},
		staticProbe{name: "memory", required: true, res: Result{Status: StatusAbsent, Detail: "unit failed", Fix: "pix serve restart"}},
		staticProbe{name: "pack", res: Result{Status: StatusUnknown, Detail: "unreadable"}},
	)
	var b strings.Builder
	RenderStatus(&b, s)
	out := b.String()
	if n := len(strings.Split(strings.TrimSpace(out), "\n")); n > 5 {
		t.Errorf("status is meant to be short, got %d lines:\n%s", n, out)
	}
	if strings.Contains(out, "pix serve restart") {
		t.Errorf("status must not print repair commands (that is doctor's job):\n%s", out)
	}
	for _, want := range []string{"sbx", "memory", "pack"} {
		if !strings.Contains(out, want) {
			t.Errorf("status omitted %q:\n%s", want, out)
		}
	}
}

// TestStatusOffIsNeitherGapNorSuccess pins the core of the fifth status:
// verified, optional, intentionally not configured is its own answer, not a
// gap laundered into ready and not a gap laundered into missing.
func TestStatusOffIsNeitherGapNorSuccess(t *testing.T) {
	r := Result{Name: "pack", Status: StatusOff, Required: false}
	if r.Effective() != StatusOff {
		t.Fatalf("Effective() = %q, want %q — off must not collapse into unknown", r.Effective(), StatusOff)
	}
	if r.OK() {
		t.Error("an off result must never report OK: nothing was verified working")
	}
	if r.Missing() {
		t.Error("an off result must never report Missing: there is nothing to repair")
	}
	if r.Blocking() {
		t.Error("an off result must never block, even if (wrongly) marked required")
	}
	if Glyph(StatusOff) == "?" {
		t.Error("Glyph(StatusOff) must not render as the unknown glyph")
	}
}

// TestSnapshotNeverSeesOffAsAGapOrAFix: Gaps, OptionalGaps, Blocking and Fixes
// all filter on Missing(), which off never satisfies — but this pins the
// snapshot-level guarantee directly, so a future change to any one of those
// filters is caught here even if it does not touch Missing() itself.
func TestSnapshotNeverSeesOffAsAGapOrAFix(t *testing.T) {
	s := Run(context.Background(), time.Second,
		staticProbe{name: "core", required: true, res: Result{Status: StatusReady}},
		staticProbe{name: "pack", required: false, res: Result{Status: StatusOff, Detail: "no active pack"}},
	)
	if len(s.Gaps()) != 0 {
		t.Errorf("Gaps() = %v, want none: an off result is not a gap", s.Gaps())
	}
	if len(s.OptionalGaps()) != 0 {
		t.Errorf("OptionalGaps() = %v, want none", s.OptionalGaps())
	}
	if len(s.Blocking()) != 0 {
		t.Errorf("Blocking() = %v, want none", s.Blocking())
	}
	if len(s.Fixes()) != 0 {
		t.Errorf("Fixes() = %v, want none: an off row carries no repair", s.Fixes())
	}
	if !s.Ready() {
		t.Error("an optional off result must not make the host un-ready")
	}
	if s.ExitCode() != ExitOK {
		t.Errorf("exit = %d, want %d", s.ExitCode(), ExitOK)
	}
}

// TestRunCatchesARequiredProbeReportingOff pins the eligibility invariant at
// the runner: StatusOff is reserved for OPTIONAL probes ("a required capability
// cannot also be an invitation to skip"), and Run must not trust a probe that
// violates it — it degrades to unknown, the same fail-safe treatment a panic
// gets, rather than silently letting a required check disappear from both the
// gap list and the ready list.
func TestRunCatchesARequiredProbeReportingOff(t *testing.T) {
	s := Run(context.Background(), time.Second,
		staticProbe{name: "buggy", required: true, res: Result{Status: StatusOff, Fix: "should never be seen"}},
	)
	r := find(t, s, "buggy")
	if r.Effective() != StatusUnknown {
		t.Errorf("a required probe reporting off = %q, want unknown (the eligibility rule was violated)", r.Effective())
	}
	if r.Fix != "" {
		t.Errorf("the downgraded result must carry no fix, got %q", r.Fix)
	}
	if s.ExitCode() != ExitOK {
		t.Errorf("exit = %d, want %d — the eligibility violation degrades to unknown, which never fails the process", s.ExitCode(), ExitOK)
	}
}

// TestRenderStatus_OffProducesNoIssueLine is the bug this whole change fixes,
// pinned at the render layer: a host with an optional, unconfigured capability
// (pack) reports zero gaps and prints no "N issue(s). Run pix doctor" line —
// before StatusOff existed this same snapshot printed exactly that line and
// listed pack under "missing".
func TestRenderStatus_OffProducesNoIssueLine(t *testing.T) {
	s := Run(context.Background(), time.Second,
		staticProbe{name: "sbx", required: true, res: Result{Status: StatusReady, Detail: "1.2.3"}},
		staticProbe{name: "pack", res: Result{Status: StatusOff, Detail: "no active pack"}},
	)
	if len(s.Gaps()) != 0 {
		t.Fatalf("Gaps() = %v, want none", s.Gaps())
	}
	var b strings.Builder
	RenderStatus(&b, s)
	out := b.String()
	if strings.Contains(out, "issue") {
		t.Errorf("an off-only host must print no issue line:\n%s", out)
	}
	if strings.Contains(out, "missing") {
		t.Errorf("pack must not be filed under missing:\n%s", out)
	}
}

func TestRenderDoctorPrintsExactFixes(t *testing.T) {
	s := Run(context.Background(), time.Second,
		staticProbe{name: "memory", required: true, res: Result{Status: StatusAbsent, Detail: "unit failed", Fix: "pix serve restart", Evidence: "state=failed"}},
		staticProbe{name: "sbx", required: true, res: Result{Status: StatusUnknown, Detail: "probe timed out", Fix: "never printed"}},
	)
	var b strings.Builder
	RenderDoctor(&b, s)
	out := b.String()
	if !strings.Contains(out, "pix serve restart") {
		t.Errorf("doctor must print the exact fix:\n%s", out)
	}
	if strings.Contains(out, "never printed") {
		t.Errorf("doctor must not print a fix for an unknown:\n%s", out)
	}
	if !strings.Contains(out, "state=failed") {
		t.Errorf("doctor must show the evidence behind a verdict:\n%s", out)
	}
	if !strings.Contains(out, "probe timed out") {
		t.Errorf("doctor must say WHY an axis is unknown:\n%s", out)
	}
}

// TestRenderDoctorPrintsHintIndentedNeverInFixBlock: an off row's Hint is the
// invitation text ("here is what turning this on would give you"), and it must
// read as annotation under the row, never as a repair — the "Fix:" block is
// reserved for verified gaps, and a Hint sitting there would claim pack is
// broken when it is only unconfigured.
func TestRenderDoctorPrintsHintIndentedNeverInFixBlock(t *testing.T) {
	s := Run(context.Background(), time.Second,
		staticProbe{name: "pack", res: Result{Status: StatusOff, Detail: "no active pack",
			Hint: "a pack carries skills, knowledge, MCP servers and config — pix pack use <path|owner/repo>"}},
	)
	var b strings.Builder
	RenderDoctor(&b, s)
	out := b.String()
	if !strings.Contains(out, "a pack carries skills") {
		t.Errorf("doctor must render the Hint:\n%s", out)
	}
	if strings.Contains(out, "Fix:") {
		t.Errorf("an off-only snapshot has no verified gap, so there must be no Fix: block:\n%s", out)
	}
	if len(s.Fixes()) != 0 {
		t.Errorf("Fixes() = %v, want none", s.Fixes())
	}
}
