package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestBlockingCheck is the exact exit-code matrix: only a POSITIVELY VERIFIED
// core failure (verdict todo or denied) blocks. Optional anything, and any
// requirement that is merely unverifiable or ready, never does.
func TestBlockingCheck(t *testing.T) {
	cases := []struct {
		req  requirement
		v    verdict
		want bool
	}{
		{requirementCore, verdictTodo, true},
		{requirementCore, verdictDenied, true},
		{requirementCore, verdictReady, false},
		{requirementCore, verdictUnverifiable, false},
		{requirementOptional, verdictTodo, false},
		{requirementOptional, verdictDenied, false},
		{requirementOptional, verdictReady, false},
		{requirementOptional, verdictUnverifiable, false},
	}
	for _, c := range cases {
		if got := blockingCheck(c.req, c.v); got != c.want {
			t.Errorf("blockingCheck(%s,%s) = %v, want %v", c.req, c.v, got, c.want)
		}
	}
}

// TestCheckZeroValuesFailSafe: an unset requirement reads as optional (never a
// surprise exit-1) and an unset verdict reads as unverifiable (never a false
// green, never a block).
func TestCheckZeroValuesFailSafe(t *testing.T) {
	c := check{label: "x", detail: "d"}
	if c.req() != requirementOptional {
		t.Errorf("zero requirement = %q, want optional", c.req())
	}
	if c.result() != verdictUnverifiable {
		t.Errorf("zero verdict = %q, want unverifiable", c.result())
	}
	if c.state() != stateWarn {
		t.Errorf("zero-verdict state = %v, want stateWarn", c.state())
	}
	if blockingCheck(c.req(), c.result()) {
		t.Error("zero-value check must never block")
	}
	// A note makes no claim: it renders · and reads ready for the tallies.
	n := check{label: "n", detail: "annotation", note: true}
	if n.state() != stateInfo || n.result() != verdictReady {
		t.Errorf("note = (state %v, result %q), want (stateInfo, ready)", n.state(), n.result())
	}
	// Evidence falls back to detail so JSON is never blank.
	if c.evidenceString() != "d" {
		t.Errorf("evidence fallback = %q, want detail", c.evidenceString())
	}
	e := check{detail: "d", evidence: "probe: x=y"}
	if e.evidenceString() != "probe: x=y" {
		t.Errorf("explicit evidence = %q", e.evidenceString())
	}
}

// TestReportTallies_UnverifiableNeverTodo: an unverifiable check never
// surfaces its todo suggestion and never counts as outstanding, while denied
// counts as outstanding and blocks when core.
func TestReportTallies_UnverifiableNeverTodo(t *testing.T) {
	r := &report{groups: []group{{
		title: "g",
		checks: []check{
			{label: "a", verdict: verdictUnverifiable, todo: "should-not-appear"},
			{label: "b", verdict: verdictTodo, todo: "fix-b"},
			{label: "c", verdict: verdictDenied, todo: "escalate-c"},
			{label: "d", verdict: verdictReady},
			{label: "e", note: true},
		},
	}}}
	if got := strings.Join(r.todos(), ","); got != "fix-b,escalate-c" {
		t.Errorf("todos() = %q, want fix-b,escalate-c", got)
	}
	if r.outstanding() != 2 {
		t.Errorf("outstanding() = %d, want 2 (todo + denied)", r.outstanding())
	}
	if r.unverifiableCount() != 1 {
		t.Errorf("unverifiableCount() = %d, want 1", r.unverifiableCount())
	}
	if r.blocking() {
		t.Error("all-optional report must not block")
	}
	r.groups[0].checks[2].requirement = requirementCore
	if !r.blocking() {
		t.Error("a core denied must block")
	}
}

// TestRenderHeadline distinguishes the four headline states: verified core
// failure > optional outstanding > unverifiable-only > all pass.
func TestRenderHeadline(t *testing.T) {
	render := func(r *report) string {
		var buf bytes.Buffer
		r.render(&buf, false)
		return buf.String()
	}
	coreFail := &report{groups: []group{{title: "g", checks: []check{
		{label: "k", requirement: requirementCore, verdict: verdictTodo, todo: "fix"},
	}}}}
	if out := render(coreFail); !strings.Contains(out, "required core check is verified failing") {
		t.Errorf("core-failure headline missing, got:\n%s", out)
	}
	optFail := &report{groups: []group{{title: "g", checks: []check{
		{label: "k", verdict: verdictTodo, todo: "fix"},
	}}}}
	if out := render(optFail); !strings.Contains(out, "outstanding (optional, nothing blocking)") {
		t.Errorf("optional-outstanding headline missing, got:\n%s", out)
	}
	unv := &report{groups: []group{{title: "g", checks: []check{
		{label: "k", verdict: verdictUnverifiable},
	}}}}
	if out := render(unv); !strings.Contains(out, "could not be verified from here") {
		t.Errorf("unverifiable headline missing, got:\n%s", out)
	} else if strings.Contains(out, "outstanding") {
		t.Errorf("unverifiable-only must not read as outstanding, got:\n%s", out)
	}
	pass := &report{groups: []group{{title: "g", checks: []check{
		{label: "k", verdict: verdictReady},
	}}}}
	if out := render(pass); !strings.Contains(out, "all checks pass") {
		t.Errorf("all-pass headline missing, got:\n%s", out)
	}
}

// TestRenderConciseVsVerbose: the default collapses verified-ready detail to a
// per-group summary (with the --verbose hint); --verbose shows every check and
// drops the hint. Non-ready checks and notes always show.
func TestRenderConciseVsVerbose(t *testing.T) {
	r := &report{groups: []group{
		{title: "healthy group", checks: []check{
			{label: "h1", verdict: verdictReady, detail: "ready-detail-1"},
			{label: "h2", verdict: verdictReady, detail: "ready-detail-2"},
		}},
		{title: "mixed group", checks: []check{
			{label: "ok", verdict: verdictReady, detail: "ready-detail-3"},
			{label: "bad", verdict: verdictTodo, detail: "broken", todo: "fix-it"},
			{label: "ctx", note: true, detail: "annotation-line"},
			{label: "unv", verdict: verdictUnverifiable, detail: "cannot-check"},
		}},
	}}
	var concise bytes.Buffer
	r.render(&concise, false)
	out := concise.String()
	for _, hidden := range []string{"ready-detail-1", "ready-detail-2", "ready-detail-3"} {
		if strings.Contains(out, hidden) {
			t.Errorf("concise output must collapse ready detail %q:\n%s", hidden, out)
		}
	}
	if !strings.Contains(out, "✓ all 2 checks ready") {
		t.Errorf("concise output missing the per-group ready summary:\n%s", out)
	}
	for _, shown := range []string{"broken", "annotation-line", "cannot-check", "TODO: fix-it"} {
		if !strings.Contains(out, shown) {
			t.Errorf("concise output must keep %q:\n%s", shown, out)
		}
	}
	if !strings.Contains(out, "pi-stack doctor --verbose") {
		t.Errorf("concise output that hid detail must hint at --verbose:\n%s", out)
	}

	var verbose bytes.Buffer
	r.render(&verbose, true)
	vout := verbose.String()
	for _, shown := range []string{"ready-detail-1", "ready-detail-2", "ready-detail-3", "broken", "annotation-line", "cannot-check"} {
		if !strings.Contains(vout, shown) {
			t.Errorf("verbose output must show %q:\n%s", shown, vout)
		}
	}
	if strings.Contains(vout, "pi-stack doctor --verbose") {
		t.Errorf("verbose output must not hint at --verbose:\n%s", vout)
	}
}

// TestRenderConcise_NoHintWhenNothingHidden: a cold/all-todo run shows every
// check already, so the --verbose hint must not print.
func TestRenderConcise_NoHintWhenNothingHidden(t *testing.T) {
	r := &report{groups: []group{{title: "g", checks: []check{
		{label: "bad", verdict: verdictTodo, detail: "broken", todo: "fix-it"},
	}}}}
	var buf bytes.Buffer
	r.render(&buf, false)
	if strings.Contains(buf.String(), "--verbose") {
		t.Errorf("nothing was collapsed; no hint expected:\n%s", buf.String())
	}
}

// TestRenderGlyphs: verdict is authoritative for the glyph — ⚠ for
// unverifiable, ✗ for verified todo/denied, ✓ ready, · note.
func TestRenderGlyphs(t *testing.T) {
	for want, s := range map[string]checkState{
		"✓": stateOK, "✗": stateTODO, "·": stateInfo, "⚠": stateWarn,
	} {
		if got := glyph(s); got != want {
			t.Errorf("glyph(%v) = %q, want %q", s, got, want)
		}
	}
	if (check{verdict: verdictDenied}).state() != stateTODO {
		t.Error("denied must render the verified-failure glyph class")
	}
}

// TestDoctorJSONSchemaV2 asserts the exact v2 contract: schema_version 2, the
// top-level blocking flag, and per-check group/label/requirement/verdict/
// evidence/todo alongside the retained v1 state/detail compatibility fields.
func TestDoctorJSONSchemaV2(t *testing.T) {
	r := &report{
		groups: []group{{title: "G1", checks: []check{
			{label: "ok", verdict: verdictReady, detail: "fine", evidence: "probe: fine"},
			{label: "bad", verdict: verdictTodo, detail: "broken", todo: "fix-it"},
			{label: "unv", verdict: verdictUnverifiable, detail: "cannot-check", todo: "never-emit"},
			{label: "den", requirement: requirementCore, verdict: verdictDenied, detail: "org says no"},
		}}},
		services: []string{"memory"},
	}
	v := r.jsonView("")
	if v.SchemaVersion != 2 {
		t.Fatalf("schema_version = %d, want 2", v.SchemaVersion)
	}
	if !v.Blocking || v.Verdict != "blocked" {
		t.Errorf("core denied -> (blocking=%v, verdict=%q), want (true, blocked)", v.Blocking, v.Verdict)
	}
	if len(v.Groups) != 1 || len(v.Groups[0].Checks) != 4 {
		t.Fatalf("unexpected groups shape: %+v", v.Groups)
	}
	byLabel := map[string]doctorCheckJSON{}
	for _, c := range v.Groups[0].Checks {
		byLabel[c.Label] = c
		if c.Group != "G1" {
			t.Errorf("check %q group = %q, want G1", c.Label, c.Group)
		}
	}
	ok := byLabel["ok"]
	if ok.Requirement != "optional" || ok.Verdict != "ready" || ok.Evidence != "probe: fine" || ok.State != "ok" || ok.Detail != "fine" {
		t.Errorf("ok check = %+v", ok)
	}
	bad := byLabel["bad"]
	if bad.Verdict != "todo" || bad.Todo != "fix-it" || bad.State != "todo" || bad.Evidence != "broken" {
		t.Errorf("bad check = %+v", bad)
	}
	unv := byLabel["unv"]
	if unv.Verdict != "unverifiable" || unv.Todo != "" || unv.State != "warn" {
		t.Errorf("unverifiable check must carry no todo and state warn, got %+v", unv)
	}
	den := byLabel["den"]
	if den.Requirement != "core" || den.Verdict != "denied" || den.State != "todo" {
		t.Errorf("denied check = %+v", den)
	}

	// The serialized bytes literally carry the new field names.
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{
		`"schema_version":2`, `"blocking":true`, `"group":"G1"`,
		`"requirement":"core"`, `"verdict":"denied"`, `"evidence":"probe: fine"`,
		`"state":"ok"`, `"detail":"fine"`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("serialized JSON missing %s:\n%s", want, raw)
		}
	}
}

// TestDoctorJSON_NonBlockingVerdicts: verdict distinguishes outstanding,
// unverifiable-only, and pass without blocking.
func TestDoctorJSON_NonBlockingVerdicts(t *testing.T) {
	mk := func(v verdict) *report {
		return &report{groups: []group{{title: "g", checks: []check{{label: "c", verdict: v, todo: "x"}}}}}
	}
	if got := mk(verdictTodo).jsonView(""); got.Verdict != "outstanding" || got.Blocking {
		t.Errorf("optional todo -> %+v, want outstanding/non-blocking", got)
	}
	if got := mk(verdictUnverifiable).jsonView(""); got.Verdict != "unverifiable" || got.Blocking {
		t.Errorf("unverifiable -> %+v, want unverifiable/non-blocking", got)
	}
	if got := mk(verdictReady).jsonView(""); got.Verdict != "pass" || got.Blocking {
		t.Errorf("ready -> %+v, want pass/non-blocking", got)
	}
}

// TestRunDoctor_NothingBlocksYet pins the S04 compatibility contract for a
// COLD run (sbx absent, nothing installed): it never blocks (exit stays 0, as
// before). S04 kept every check optional; S06 adds the providers group's ONE
// core check ("model key"), but with sbx absent that check degrades to
// unverifiable — positively-confirmed-zero (the only thing that would block
// it) requires sbx to actually answer, which a cold run by definition cannot.
func TestRunDoctor_NothingBlocksYet(t *testing.T) {
	f := fakeEnv{present: map[string]bool{}, output: map[string]string{}, ports: map[int]bool{}}
	r := runDoctor(defaultCfg(), f.env())
	if r.blocking() {
		t.Error("a cold (sbx-absent) run must never block")
	}
	if len(r.todos()) == 0 {
		t.Error("a cold run should still surface todos")
	}
	v := r.jsonView("")
	if v.Blocking || v.Verdict != "outstanding" {
		t.Errorf("cold run JSON = (blocking=%v, verdict=%q), want (false, outstanding)", v.Blocking, v.Verdict)
	}
	for _, g := range v.Groups {
		for _, c := range g.Checks {
			// The providers group's "model key" check is the one deliberate
			// exception (S06): it is core, but sbx-absent makes it unverifiable,
			// which never blocks — exercised directly below.
			if c.Requirement != "optional" && !(g.Title == "Providers / keys (proxy-injected, never in the VM)" && c.Label == "model key") {
				t.Errorf("check %q requirement = %q; only the S06 model-key check is core", c.Label, c.Requirement)
			}
			if c.Evidence == "" && !c.Note {
				t.Errorf("check %q has empty evidence", c.Label)
			}
		}
	}
	for _, g := range v.Groups {
		if g.Title != "Providers / keys (proxy-injected, never in the VM)" {
			continue
		}
		for _, c := range g.Checks {
			if c.Label == "model key" && c.Verdict != "unverifiable" {
				t.Errorf("cold run's model-key check verdict = %q, want unverifiable", c.Verdict)
			}
		}
	}
}
