package readiness

import (
	"testing"
)

// TestBlockingCheck is the exact exit-code matrix: only a POSITIVELY VERIFIED
// core failure (Verdict todo or denied) blocks. Optional anything, and any
// Requirement that is merely unverifiable or ready, never does.
func TestBlockingCheck(t *testing.T) {
	cases := []struct {
		req  Requirement
		v    Verdict
		want bool
	}{
		{RequirementCore, VerdictTodo, true},
		{RequirementCore, VerdictDenied, true},
		{RequirementCore, VerdictReady, false},
		{RequirementCore, VerdictUnverifiable, false},
		{RequirementOptional, VerdictTodo, false},
		{RequirementOptional, VerdictDenied, false},
		{RequirementOptional, VerdictReady, false},
		{RequirementOptional, VerdictUnverifiable, false},
	}
	for _, c := range cases {
		if got := BlockingCheck(c.req, c.v); got != c.want {
			t.Errorf("BlockingCheck(%s,%s) = %v, want %v", c.req, c.v, got, c.want)
		}
	}
}

// TestCheckZeroValuesFailSafe: an unset Requirement reads as optional (never a
// surprise exit-1) and an unset Verdict reads as unverifiable (never a false
// green, never a block).
func TestCheckZeroValuesFailSafe(t *testing.T) {
	c := Check{Label: "x", Detail: "d"}
	if c.Req() != RequirementOptional {
		t.Errorf("zero requirement = %q, want optional", c.Req())
	}
	if c.Result() != VerdictUnverifiable {
		t.Errorf("zero verdict = %q, want unverifiable", c.Result())
	}
	if c.State() != StateWarn {
		t.Errorf("zero-Verdict state = %v, want StateWarn", c.State())
	}
	if BlockingCheck(c.Req(), c.Result()) {
		t.Error("zero-value check must never block")
	}
	// A note ALWAYS renders · regardless of its Verdict (state() special-cases
	// note), but result() reads its Verdict truthfully like any other Check: an
	// unset Verdict on a note still reads unverifiable (fail-safe), NOT a
	// blanket ready — result() must not override an explicit/absent Verdict
	// merely because note is set.
	n := Check{Label: "n", Detail: "annotation", Note: true}
	if n.State() != StateInfo || n.Result() != VerdictUnverifiable {
		t.Errorf("note with unset verdict = (state %v, result %q), want (StateInfo, unverifiable)", n.State(), n.Result())
	}
	// A note with an EXPLICIT truthful Verdict carries it through unchanged.
	readyNote := Check{Label: "n2", Detail: "set", Note: true, Verdict: VerdictReady}
	if readyNote.State() != StateInfo || readyNote.Result() != VerdictReady {
		t.Errorf("note with explicit VerdictReady = (state %v, result %q), want (StateInfo, ready)", readyNote.State(), readyNote.Result())
	}
	// Evidence falls back to detail so JSON is never blank.
	if c.EvidenceString() != "d" {
		t.Errorf("evidence fallback = %q, want detail", c.EvidenceString())
	}
	e := Check{Detail: "d", Evidence: "probe: x=y"}
	if e.EvidenceString() != "probe: x=y" {
		t.Errorf("explicit evidence = %q", e.EvidenceString())
	}
}

// TestRenderHeadline distinguishes the four headline states: verified core
// TestRenderConciseVsVerbose: the default collapses verified-ready detail to a
// per-Group summary (with the --verbose hint); --verbose shows every Check and
// TestRenderConcise_NoHintWhenNothingHidden: a cold/all-todo run shows every
// TestRenderGlyphs: Verdict is authoritative for the glyph, in the ONE shared
// vocabulary (readiness_render.go) — ✓ ready, ✗ verified CORE todo/denied,
// ⚠ verified OPTIONAL todo/denied, ? unverifiable ("can't check from here"),
// · note. doctor's row renderer must go through Glyph(c) (which reads
// the Check's OWN Requirement + note), never a bare state-only mapping that
// hardcodes a single Requirement — that bug rendered every optional TODO as
// the core ✗ instead of the optional ⚠.
func TestRenderGlyphs(t *testing.T) {
	cases := []struct {
		Name string
		c    Check
		want string
	}{
		{"ready", Check{Verdict: VerdictReady}, "✓"},
		{"core todo", Check{Verdict: VerdictTodo, Requirement: RequirementCore}, "✗"},
		{"optional todo", Check{Verdict: VerdictTodo, Requirement: RequirementOptional}, "⚠"},
		{"unset requirement todo (defaults optional)", Check{Verdict: VerdictTodo}, "⚠"},
		{"unverifiable", Check{Verdict: VerdictUnverifiable}, "?"},
		{"note", Check{Verdict: VerdictTodo, Requirement: RequirementCore, Note: true}, "·"},
	}
	for _, tc := range cases {
		if got := Glyph(tc.c); got != tc.want {
			t.Errorf("%s: glyph = %q, want %q", tc.Name, got, tc.want)
		}
	}
	if (Check{Verdict: VerdictDenied}).State() != StateTODO {
		t.Error("denied must render the verified-failure glyph class")
	}
}

// TestDoctorRender_OptionalTodoRendersWarnGlyph: an end-to-end Check through
// Report.render (not just Glyph directly) — an OPTIONAL verified todo row
// must print ⚠, never the core ✗, confirming the render loop itself calls
// TestDoctorJSONSchemaV2 asserts the exact v2 contract fields still hold at
// the current schema version (now 3, which is purely additive over v2: the
// `checks`/`exit` fields): the top-level Blocking flag, and per-Check
// Group/label/Requirement/Verdict/evidence/todo alongside the retained v1
// TestDoctorJSON_NonBlockingVerdicts: Verdict distinguishes Outstanding,
// TestRunDoctor_NothingBlocksYet pins the S04 compatibility contract for a
// COLD run (sbx absent, nothing installed): it never blocks (exit stays 0, as
// before). S04 kept every Check optional; S06 adds the providers Group's ONE
// core Check ("model key"), but with sbx absent that Check degrades to
// unverifiable — positively-confirmed-zero (the only thing that would block
