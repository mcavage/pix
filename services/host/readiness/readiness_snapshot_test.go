package readiness

import (
	"reflect"
	"testing"
)

// TestAxisSetIsFrozen pins the readiness axis set. It is deliberately a
// hand-written literal: adding an axis is a product decision recorded in the
// PRD (§6.3 AC-P0-204), not a commit. If this test fails, the change is either
// wrong or needs the PRD updated first.
func TestAxisSetIsFrozen(t *testing.T) {
	want := []string{
		"gworkspace",
		"model.bridge",
		"model.embed",
		"model.watcher",
		"ollama.host",
		"ollama.sandbox",
		"pack",
		"providers",
		"sbx",
		"secrets",
		"service.knowledge",
		"service.memory",
	}
	if got := AxisNames(AllAxes); !reflect.DeepEqual(got, want) {
		t.Fatalf("axis set changed.\n got: %v\nwant: %v\nAdding or removing an axis requires a recorded decision in the PRD (AC-P0-204), not just a code change.", got, want)
	}
	// The one parameterized family.
	if !MCPAxis("slack").Known() {
		t.Fatal("mcp:<server> must be a known axis")
	}
	if MCPAxis("").Known() {
		t.Fatal("mcp: with no server name must not be a known axis")
	}
	if Axis("invented").Known() {
		t.Fatal("an axis outside the frozen set must not be known")
	}
}

// TestUnrequestedAxesAreAbsent proves the laziness contract: a builder for an
// axis nobody requested never runs, and the axis is ABSENT from the snapshot —
// never ready, never a Verdict.
func TestUnrequestedAxesAreAbsent(t *testing.T) {
	ran := map[Axis]int{}
	builders := map[Axis]AxisBuilder{}
	for _, a := range AllAxes {
		a := a
		builders[a] = func() []Check {
			ran[a]++
			return []Check{{Label: string(a), Verdict: VerdictReady}}
		}
	}
	s := Build(Request{Axes: []Axis{AxisSbx, AxisProviders}}, builders)

	if len(ran) != 2 || ran[AxisSbx] != 1 || ran[AxisProviders] != 1 {
		t.Fatalf("expected exactly the two requested builders to run once, got %v", ran)
	}
	if s.Has(AxisOllamaHost) {
		t.Fatal("an unrequested axis must be absent from the snapshot")
	}
	if _, _, ok := s.AxisVerdict(AxisOllamaHost); ok {
		t.Fatal("an absent axis must not report a verdict")
	}
	if got := s.Axes(); !reflect.DeepEqual(got, []Axis{AxisSbx, AxisProviders}) {
		t.Fatalf("axes should preserve request order, got %v", got)
	}
}

// TestRequestedPromotionLivesInTheType: `requested` promotes an OPTIONAL axis
// to Blocking for this invocation only, and does so in Build — the
// command's flag handling never touches a Requirement.
func TestRequestedPromotionLivesInTheType(t *testing.T) {
	builders := map[Axis]AxisBuilder{
		AxisModelWatcher: func() []Check {
			return []Check{{Label: "watcher", Requirement: RequirementOptional, Verdict: VerdictTodo, Evidence: "not pulled", Todo: "ollama pull x"}}
		},
	}
	plain := Build(Request{Axes: []Axis{AxisModelWatcher}}, builders)
	if got := plain.ExitCode(); got != ExitReady {
		t.Fatalf("a stale OPTIONAL axis must never block unrelated repair: exit %d, want 0", got)
	}
	promoted := Build(Request{Axes: []Axis{AxisModelWatcher}, Requested: []Axis{AxisModelWatcher}}, builders)
	if got := promoted.ExitCode(); got != ExitNotReady {
		t.Fatalf("an explicitly requested axis must block: exit %d, want 1", got)
	}
	req, _, _ := promoted.AxisVerdict(AxisModelWatcher)
	if req != RequirementRequested {
		t.Fatalf("promotion must be recorded on the check, got requirement %q", req)
	}
	// Core is never downgraded, and notes are never promoted.
	core := Build(Request{Axes: []Axis{AxisModelWatcher}, Requested: []Axis{AxisModelWatcher}}, map[Axis]AxisBuilder{
		AxisModelWatcher: func() []Check {
			return []Check{
				{Label: "core", Requirement: RequirementCore, Verdict: VerdictReady},
				{Label: "note", Note: true, Verdict: VerdictUnverifiable},
			}
		},
	})
	got, _ := core.Checks(AxisModelWatcher)
	if got[0].Req() != RequirementCore {
		t.Fatalf("core must stay core under promotion, got %q", got[0].Req())
	}
	if got[1].Req() != RequirementOptional {
		t.Fatalf("a note must never be promoted, got %q", got[1].Req())
	}
	if core.ExitCode() != ExitReady {
		t.Fatalf("a note must never affect the exit code, got %d", core.ExitCode())
	}
}

// TestExitCodeMatrix covers every (Requirement, Verdict) combination and the
// precedence rule: a verified failure (1) outranks an unverifiable (3), which
// outranks ready (0). 2 is usage-only and never derived from a snapshot.
func TestExitCodeMatrix(t *testing.T) {
	mk := func(r Requirement, v Verdict) Check {
		return Check{Label: string(r) + "/" + string(v), Requirement: r, Verdict: v}
	}
	cases := []struct {
		name       string
		checks     []Check
		want       int
		wantSuppr  int
		requestOne bool
	}{
		{name: "empty snapshot", checks: nil, want: ExitReady, wantSuppr: ExitReady},
		{name: "all ready", checks: []Check{mk(RequirementCore, VerdictReady), mk(RequirementOptional, VerdictReady)}, want: ExitReady, wantSuppr: ExitReady},
		{name: "core todo", checks: []Check{mk(RequirementCore, VerdictTodo)}, want: ExitNotReady, wantSuppr: ExitNotReady},
		{name: "core denied", checks: []Check{mk(RequirementCore, VerdictDenied)}, want: ExitNotReady, wantSuppr: ExitNotReady},
		{name: "core unverifiable", checks: []Check{mk(RequirementCore, VerdictUnverifiable)}, want: ExitUnverifiable, wantSuppr: ExitReady},
		{name: "requested todo", checks: []Check{mk(RequirementRequested, VerdictTodo)}, want: ExitNotReady, wantSuppr: ExitNotReady},
		{name: "requested unverifiable", checks: []Check{mk(RequirementRequested, VerdictUnverifiable)}, want: ExitUnverifiable, wantSuppr: ExitReady},
		{name: "optional todo", checks: []Check{mk(RequirementOptional, VerdictTodo)}, want: ExitReady, wantSuppr: ExitReady},
		{name: "optional denied", checks: []Check{mk(RequirementOptional, VerdictDenied)}, want: ExitReady, wantSuppr: ExitReady},
		{name: "optional unverifiable", checks: []Check{mk(RequirementOptional, VerdictUnverifiable)}, want: ExitReady, wantSuppr: ExitReady},
		{name: "unset requirement and verdict reads optional+unverifiable", checks: []Check{{Label: "zero"}}, want: ExitReady, wantSuppr: ExitReady},
		{name: "unset requirement on a core-looking failure never blocks", checks: []Check{{Label: "zero", Verdict: VerdictTodo}}, want: ExitReady, wantSuppr: ExitReady},
		{name: "verified failure outranks unverifiable", checks: []Check{mk(RequirementCore, VerdictUnverifiable), mk(RequirementCore, VerdictTodo)}, want: ExitNotReady, wantSuppr: ExitNotReady},
		{name: "unverifiable outranks ready", checks: []Check{mk(RequirementCore, VerdictReady), mk(RequirementCore, VerdictUnverifiable)}, want: ExitUnverifiable, wantSuppr: ExitReady},
		{name: "note never counts", checks: []Check{{Label: "n", Note: true, Requirement: RequirementCore, Verdict: VerdictTodo}}, want: ExitReady, wantSuppr: ExitReady},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checks := tc.checks
			s := Build(Request{Axes: []Axis{AxisSbx}}, map[Axis]AxisBuilder{
				AxisSbx: func() []Check { return checks },
			})
			if got := s.ExitCode(); got != tc.want {
				t.Fatalf("ExitCode() = %d, want %d", got, tc.want)
			}
			if got := s.ExitCodeSuppressingUnverifiable(); got != tc.wantSuppr {
				t.Fatalf("ExitCodeSuppressingUnverifiable() = %d, want %d", got, tc.wantSuppr)
			}
			if s.ExitCode() == ExitUsage {
				t.Fatal("2 is a usage code and must never be derived from a snapshot")
			}
		})
	}
}

// TestAxisVerdictReportsTheWorstCheck: an axis with several checks reports the
// worst one, with the strongest Requirement seen.
func TestAxisVerdictReportsTheWorstCheck(t *testing.T) {
	s := Build(Request{Axes: []Axis{AxisServiceMemory}}, map[Axis]AxisBuilder{
		AxisServiceMemory: func() []Check {
			return []Check{
				{Label: "port", Requirement: RequirementOptional, Verdict: VerdictReady},
				{Label: "identity", Requirement: RequirementCore, Verdict: VerdictTodo},
				{Label: "note", Note: true, Verdict: VerdictReady},
			}
		},
	})
	req, v, ok := s.AxisVerdict(AxisServiceMemory)
	if !ok || req != RequirementCore || v != VerdictTodo {
		t.Fatalf("AxisVerdict = (%q, %q, %v), want (core, todo, true)", req, v, ok)
	}
	if s.Outstanding() != 1 || s.UnverifiableCount() != 0 {
		t.Fatalf("tallies wrong: outstanding=%d unverifiable=%d", s.Outstanding(), s.UnverifiableCount())
	}
}

// TestSnapshotStampsAxisAndDuration: every Check carries its axis, so a
// renderer never has to re-derive it from a Group title.
func TestSnapshotStampsAxisAndDuration(t *testing.T) {
	s := Build(Request{Axes: []Axis{AxisPack}}, map[Axis]AxisBuilder{
		AxisPack: func() []Check { return []Check{{Label: "pack", Verdict: VerdictReady}} },
	})
	got, _ := s.Checks(AxisPack)
	if got[0].AxisOf() != AxisPack {
		t.Fatalf("check axis = %q, want %q", got[0].AxisOf(), AxisPack)
	}
	if got[0].Duration < 0 {
		t.Fatal("duration must be measured")
	}
}
