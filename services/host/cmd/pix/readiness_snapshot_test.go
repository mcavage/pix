package main

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
	if got := axisNames(readinessAxes); !reflect.DeepEqual(got, want) {
		t.Fatalf("axis set changed.\n got: %v\nwant: %v\nAdding or removing an axis requires a recorded decision in the PRD (AC-P0-204), not just a code change.", got, want)
	}
	// The one parameterized family.
	if !mcpAxis("slack").known() {
		t.Fatal("mcp:<server> must be a known axis")
	}
	if mcpAxis("").known() {
		t.Fatal("mcp: with no server name must not be a known axis")
	}
	if Axis("invented").known() {
		t.Fatal("an axis outside the frozen set must not be known")
	}
}

// TestUnrequestedAxesAreAbsent proves the laziness contract: a builder for an
// axis nobody requested never runs, and the axis is ABSENT from the snapshot —
// never ready, never a verdict.
func TestUnrequestedAxesAreAbsent(t *testing.T) {
	ran := map[Axis]int{}
	builders := map[Axis]axisBuilder{}
	for _, a := range readinessAxes {
		a := a
		builders[a] = func() []check {
			ran[a]++
			return []check{{label: string(a), verdict: verdictReady}}
		}
	}
	s := buildSnapshot(Request{Axes: []Axis{axisSbx, axisProviders}}, builders)

	if len(ran) != 2 || ran[axisSbx] != 1 || ran[axisProviders] != 1 {
		t.Fatalf("expected exactly the two requested builders to run once, got %v", ran)
	}
	if s.Has(axisOllamaHost) {
		t.Fatal("an unrequested axis must be absent from the snapshot")
	}
	if _, _, ok := s.AxisVerdict(axisOllamaHost); ok {
		t.Fatal("an absent axis must not report a verdict")
	}
	if got := s.Axes(); !reflect.DeepEqual(got, []Axis{axisSbx, axisProviders}) {
		t.Fatalf("axes should preserve request order, got %v", got)
	}
}

// TestRequestedPromotionLivesInTheType: `requested` promotes an OPTIONAL axis
// to blocking for this invocation only, and does so in buildSnapshot — the
// command's flag handling never touches a requirement.
func TestRequestedPromotionLivesInTheType(t *testing.T) {
	builders := map[Axis]axisBuilder{
		axisModelWatcher: func() []check {
			return []check{{label: "watcher", requirement: requirementOptional, verdict: verdictTodo, evidence: "not pulled", todo: "ollama pull x"}}
		},
	}
	plain := buildSnapshot(Request{Axes: []Axis{axisModelWatcher}}, builders)
	if got := plain.ExitCode(); got != exitReady {
		t.Fatalf("a stale OPTIONAL axis must never block unrelated repair: exit %d, want 0", got)
	}
	promoted := buildSnapshot(Request{Axes: []Axis{axisModelWatcher}, Requested: []Axis{axisModelWatcher}}, builders)
	if got := promoted.ExitCode(); got != exitNotReady {
		t.Fatalf("an explicitly requested axis must block: exit %d, want 1", got)
	}
	req, _, _ := promoted.AxisVerdict(axisModelWatcher)
	if req != requirementRequested {
		t.Fatalf("promotion must be recorded on the check, got requirement %q", req)
	}
	// Core is never downgraded, and notes are never promoted.
	core := buildSnapshot(Request{Axes: []Axis{axisModelWatcher}, Requested: []Axis{axisModelWatcher}}, map[Axis]axisBuilder{
		axisModelWatcher: func() []check {
			return []check{
				{label: "core", requirement: requirementCore, verdict: verdictReady},
				{label: "note", note: true, verdict: verdictUnverifiable},
			}
		},
	})
	got, _ := core.Checks(axisModelWatcher)
	if got[0].req() != requirementCore {
		t.Fatalf("core must stay core under promotion, got %q", got[0].req())
	}
	if got[1].req() != requirementOptional {
		t.Fatalf("a note must never be promoted, got %q", got[1].req())
	}
	if core.ExitCode() != exitReady {
		t.Fatalf("a note must never affect the exit code, got %d", core.ExitCode())
	}
}

// TestExitCodeMatrix covers every (requirement, verdict) combination and the
// precedence rule: a verified failure (1) outranks an unverifiable (3), which
// outranks ready (0). 2 is usage-only and never derived from a snapshot.
func TestExitCodeMatrix(t *testing.T) {
	mk := func(r requirement, v verdict) check {
		return check{label: string(r) + "/" + string(v), requirement: r, verdict: v}
	}
	cases := []struct {
		name       string
		checks     []check
		want       int
		wantSuppr  int
		requestOne bool
	}{
		{name: "empty snapshot", checks: nil, want: exitReady, wantSuppr: exitReady},
		{name: "all ready", checks: []check{mk(requirementCore, verdictReady), mk(requirementOptional, verdictReady)}, want: exitReady, wantSuppr: exitReady},
		{name: "core todo", checks: []check{mk(requirementCore, verdictTodo)}, want: exitNotReady, wantSuppr: exitNotReady},
		{name: "core denied", checks: []check{mk(requirementCore, verdictDenied)}, want: exitNotReady, wantSuppr: exitNotReady},
		{name: "core unverifiable", checks: []check{mk(requirementCore, verdictUnverifiable)}, want: exitUnverifiable, wantSuppr: exitReady},
		{name: "requested todo", checks: []check{mk(requirementRequested, verdictTodo)}, want: exitNotReady, wantSuppr: exitNotReady},
		{name: "requested unverifiable", checks: []check{mk(requirementRequested, verdictUnverifiable)}, want: exitUnverifiable, wantSuppr: exitReady},
		{name: "optional todo", checks: []check{mk(requirementOptional, verdictTodo)}, want: exitReady, wantSuppr: exitReady},
		{name: "optional denied", checks: []check{mk(requirementOptional, verdictDenied)}, want: exitReady, wantSuppr: exitReady},
		{name: "optional unverifiable", checks: []check{mk(requirementOptional, verdictUnverifiable)}, want: exitReady, wantSuppr: exitReady},
		{name: "unset requirement and verdict reads optional+unverifiable", checks: []check{{label: "zero"}}, want: exitReady, wantSuppr: exitReady},
		{name: "unset requirement on a core-looking failure never blocks", checks: []check{{label: "zero", verdict: verdictTodo}}, want: exitReady, wantSuppr: exitReady},
		{name: "verified failure outranks unverifiable", checks: []check{mk(requirementCore, verdictUnverifiable), mk(requirementCore, verdictTodo)}, want: exitNotReady, wantSuppr: exitNotReady},
		{name: "unverifiable outranks ready", checks: []check{mk(requirementCore, verdictReady), mk(requirementCore, verdictUnverifiable)}, want: exitUnverifiable, wantSuppr: exitReady},
		{name: "note never counts", checks: []check{{label: "n", note: true, requirement: requirementCore, verdict: verdictTodo}}, want: exitReady, wantSuppr: exitReady},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checks := tc.checks
			s := buildSnapshot(Request{Axes: []Axis{axisSbx}}, map[Axis]axisBuilder{
				axisSbx: func() []check { return checks },
			})
			if got := s.ExitCode(); got != tc.want {
				t.Fatalf("ExitCode() = %d, want %d", got, tc.want)
			}
			if got := s.ExitCodeSuppressingUnverifiable(); got != tc.wantSuppr {
				t.Fatalf("ExitCodeSuppressingUnverifiable() = %d, want %d", got, tc.wantSuppr)
			}
			if s.ExitCode() == exitUsage {
				t.Fatal("2 is a usage code and must never be derived from a snapshot")
			}
		})
	}
}

// TestAxisVerdictReportsTheWorstCheck: an axis with several checks reports the
// worst one, with the strongest requirement seen.
func TestAxisVerdictReportsTheWorstCheck(t *testing.T) {
	s := buildSnapshot(Request{Axes: []Axis{axisServiceMemory}}, map[Axis]axisBuilder{
		axisServiceMemory: func() []check {
			return []check{
				{label: "port", requirement: requirementOptional, verdict: verdictReady},
				{label: "identity", requirement: requirementCore, verdict: verdictTodo},
				{label: "note", note: true, verdict: verdictReady},
			}
		},
	})
	req, v, ok := s.AxisVerdict(axisServiceMemory)
	if !ok || req != requirementCore || v != verdictTodo {
		t.Fatalf("AxisVerdict = (%q, %q, %v), want (core, todo, true)", req, v, ok)
	}
	if s.Outstanding() != 1 || s.UnverifiableCount() != 0 {
		t.Fatalf("tallies wrong: outstanding=%d unverifiable=%d", s.Outstanding(), s.UnverifiableCount())
	}
}

// TestSnapshotStampsAxisAndDuration: every check carries its axis, so a
// renderer never has to re-derive it from a group title.
func TestSnapshotStampsAxisAndDuration(t *testing.T) {
	s := buildSnapshot(Request{Axes: []Axis{axisPack}}, map[Axis]axisBuilder{
		axisPack: func() []check { return []check{{label: "pack", verdict: verdictReady}} },
	})
	got, _ := s.Checks(axisPack)
	if got[0].axisOf() != axisPack {
		t.Fatalf("check axis = %q, want %q", got[0].axisOf(), axisPack)
	}
	if got[0].duration < 0 {
		t.Fatal("duration must be measured")
	}
}
