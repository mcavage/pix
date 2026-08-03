package doctor

// These exercise doctor's JSON schema and the Report RENDERER. Both stayed in
// this package -- JsonView is doctor's wire format, and the renderer takes
// doctor's own hint strings -- so the tests followed them back out of readiness.

import (
	"encoding/json"
	"strings"
	"testing"

	"pix/host/hostenv/hostenvtest"
	"pix/host/readiness"
)

// state/detail compatibility fields.
func TestDoctorJSONSchemaV2(t *testing.T) {
	r := &readiness.Report{
		Groups: []readiness.Group{{Title: "G1", Checks: []readiness.Check{
			{Label: "ok", Verdict: readiness.VerdictReady, Detail: "fine", Evidence: "probe: fine"},
			{Label: "bad", Verdict: readiness.VerdictTodo, Detail: "broken", Todo: "fix-it"},
			{Label: "unv", Verdict: readiness.VerdictUnverifiable, Detail: "cannot-check", Todo: "never-emit"},
			{Label: "den", Requirement: readiness.RequirementCore, Verdict: readiness.VerdictDenied, Detail: "org says no"},
		}}},
		Services: []string{"memory"},
	}
	v := JsonView(r, "")
	if v.SchemaVersion != 3 {
		t.Fatalf("schema_version = %d, want 3", v.SchemaVersion)
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
			t.Errorf("check %q readiness.Group = %q, want G1", c.Label, c.Group)
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
		`"schema_version":3`, `"blocking":true`, `"group":"G1"`,
		`"requirement":"core"`, `"verdict":"denied"`, `"evidence":"probe: fine"`,
		`"state":"ok"`, `"detail":"fine"`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("serialized JSON missing %s:\n%s", want, raw)
		}
	}
}

// unverifiable-only, and pass without Blocking.
func TestDoctorJSON_NonBlockingVerdicts(t *testing.T) {
	mk := func(v readiness.Verdict) *readiness.Report {
		return &readiness.Report{Groups: []readiness.Group{{Title: "g", Checks: []readiness.Check{{Label: "c", Verdict: v, Todo: "x"}}}}}
	}
	if got := JsonView(mk(readiness.VerdictTodo), ""); got.Verdict != "outstanding" || got.Blocking {
		t.Errorf("optional todo -> %+v, want outstanding/non-Blocking", got)
	}
	if got := JsonView(mk(readiness.VerdictUnverifiable), ""); got.Verdict != "unverifiable" || got.Blocking {
		t.Errorf("unverifiable -> %+v, want unverifiable/non-Blocking", got)
	}
	if got := JsonView(mk(readiness.VerdictReady), ""); got.Verdict != "pass" || got.Blocking {
		t.Errorf("ready -> %+v, want pass/non-Blocking", got)
	}
}

// it) requires sbx to actually answer, which a cold run by definition cannot.
func TestRunDoctor_NothingBlocksYet(t *testing.T) {
	f := hostenvtest.Env{Present: map[string]bool{}, Output: map[string]string{}, Ports: map[int]bool{}}
	r := RunDoctor(defaultCfg(), f.Build())
	if r.Blocking() {
		t.Error("a cold (sbx-absent) run must never block")
	}
	if len(r.Todos()) == 0 {
		t.Error("a cold run should still surface Todos")
	}
	v := JsonView(r, "")
	if v.Blocking || v.Verdict != "outstanding" {
		t.Errorf("cold run JSON = (blocking=%v, verdict=%q), want (false, outstanding)", v.Blocking, v.Verdict)
	}
	for _, g := range v.Groups {
		for _, c := range g.Checks {
			// The providers group's "model key" check is the one deliberate
			// exception (S06): it is core, but sbx-absent makes it unverifiable,
			// which never blocks — exercised directly below.
			if c.Requirement != "optional" && !(g.Title == "Inference / credentials (proxy-injected, never in the VM)" && c.Label == "model key") {
				t.Errorf("check %q readiness.Requirement = %q; only the S06 model-key readiness.Check is core", c.Label, c.Requirement)
			}
			if c.Evidence == "" && !c.Note {
				t.Errorf("check %q has empty evidence", c.Label)
			}
		}
	}
	for _, g := range v.Groups {
		if g.Title != "Inference / credentials (proxy-injected, never in the VM)" {
			continue
		}
		for _, c := range g.Checks {
			if c.Label == "model key" && c.Verdict != "unverifiable" {
				t.Errorf("cold run's model-key check readiness.Verdict = %q, want unverifiable", c.Verdict)
			}
		}
	}
}
