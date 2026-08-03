package main

// These exercise the Report RENDERER, which stays in this package because it
// embeds doctor's own advice strings. readiness owns the model; how a surface
// words its hints is that surface's business.

import (
	"bytes"
	"pix/host/readiness"
	"pix/host/workflow/doctor"
	"strings"
	"testing"
)

// failure > optional outstanding > unverifiable-only > all pass.
func TestRenderHeadline(t *testing.T) {
	render := func(r *readiness.Report) string {
		var buf bytes.Buffer
		r.Render(&buf, false, doctor.Hints())
		return buf.String()
	}
	coreFail := &readiness.Report{Groups: []readiness.Group{{Title: "g", Checks: []readiness.Check{
		{Label: "k", Requirement: readiness.RequirementCore, Verdict: readiness.VerdictTodo, Todo: "fix"},
	}}}}
	if out := render(coreFail); !strings.Contains(out, "required core check is verified failing") {
		t.Errorf("core-failure headline missing, got:\n%s", out)
	}
	optFail := &readiness.Report{Groups: []readiness.Group{{Title: "g", Checks: []readiness.Check{
		{Label: "k", Verdict: readiness.VerdictTodo, Todo: "fix"},
	}}}}
	if out := render(optFail); !strings.Contains(out, "outstanding (optional, nothing blocking)") {
		t.Errorf("optional-outstanding headline missing, got:\n%s", out)
	}
	unv := &readiness.Report{Groups: []readiness.Group{{Title: "g", Checks: []readiness.Check{
		{Label: "k", Verdict: readiness.VerdictUnverifiable},
	}}}}
	if out := render(unv); !strings.Contains(out, "could not be verified from here") {
		t.Errorf("unverifiable headline missing, got:\n%s", out)
	} else if strings.Contains(out, "outstanding") {
		t.Errorf("unverifiable-only must not read as outstanding, got:\n%s", out)
	}
	pass := &readiness.Report{Groups: []readiness.Group{{Title: "g", Checks: []readiness.Check{
		{Label: "k", Verdict: readiness.VerdictReady},
	}}}}
	if out := render(pass); !strings.Contains(out, "all checks pass") {
		t.Errorf("all-pass headline missing, got:\n%s", out)
	}
}

// drops the hint. Non-ready checks and notes always show.
func TestRenderConciseVsVerbose(t *testing.T) {
	r := &readiness.Report{Groups: []readiness.Group{
		{Title: "healthy group", Checks: []readiness.Check{
			{Label: "h1", Verdict: readiness.VerdictReady, Detail: "ready-detail-1"},
			{Label: "h2", Verdict: readiness.VerdictReady, Detail: "ready-detail-2"},
		}},
		{Title: "mixed group", Checks: []readiness.Check{
			{Label: "ok", Verdict: readiness.VerdictReady, Detail: "ready-detail-3"},
			{Label: "bad", Verdict: readiness.VerdictTodo, Detail: "broken", Todo: "fix-it"},
			{Label: "ctx", Note: true, Detail: "annotation-line"},
			{Label: "unv", Verdict: readiness.VerdictUnverifiable, Detail: "cannot-Check"},
		}},
	}}
	var concise bytes.Buffer
	r.Render(&concise, false, doctor.Hints())
	out := concise.String()
	for _, hidden := range []string{"ready-detail-1", "ready-detail-2", "ready-detail-3"} {
		if strings.Contains(out, hidden) {
			t.Errorf("concise output must collapse ready detail %q:\n%s", hidden, out)
		}
	}
	if !strings.Contains(out, "✓ all 2 checks ready") {
		t.Errorf("concise output missing the per-Group ready summary:\n%s", out)
	}
	for _, shown := range []string{"broken", "annotation-line", "cannot-Check", "TODO: fix-it"} {
		if !strings.Contains(out, shown) {
			t.Errorf("concise output must keep %q:\n%s", shown, out)
		}
	}
	if !strings.Contains(out, "pix doctor --verbose") {
		t.Errorf("concise output that hid detail must hint at --verbose:\n%s", out)
	}

	var verbose bytes.Buffer
	r.Render(&verbose, true, doctor.Hints())
	vout := verbose.String()
	for _, shown := range []string{"ready-detail-1", "ready-detail-2", "ready-detail-3", "broken", "annotation-line", "cannot-Check"} {
		if !strings.Contains(vout, shown) {
			t.Errorf("verbose output must show %q:\n%s", shown, vout)
		}
	}
	if strings.Contains(vout, "pix doctor --verbose") {
		t.Errorf("verbose output must not hint at --verbose:\n%s", vout)
	}
}

// Check already, so the --verbose hint must not print.
func TestRenderConcise_NoHintWhenNothingHidden(t *testing.T) {
	r := &readiness.Report{Groups: []readiness.Group{{Title: "g", Checks: []readiness.Check{
		{Label: "bad", Verdict: readiness.VerdictTodo, Detail: "broken", Todo: "fix-it"},
	}}}}
	var buf bytes.Buffer
	r.Render(&buf, false, doctor.Hints())
	if strings.Contains(buf.String(), "--verbose") {
		t.Errorf("nothing was collapsed; no hint expected:\n%s", buf.String())
	}
}

// Glyph(c) rather than a Requirement-blind glyph(c.State()).
func TestDoctorRender_OptionalTodoRendersWarnGlyph(t *testing.T) {
	r := &readiness.Report{Groups: []readiness.Group{{Title: "g", Checks: []readiness.Check{
		{Label: "opt", Detail: "needs setup", Todo: "fix-it", Verdict: readiness.VerdictTodo, Requirement: readiness.RequirementOptional},
	}}}}
	var buf bytes.Buffer
	r.Render(&buf, true, doctor.Hints())
	if !strings.Contains(buf.String(), "⚠ opt") {
		t.Errorf("an optional verified-todo row must render ⚠, got:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "✗ opt") {
		t.Errorf("an optional verified-todo row must NOT render the core ✗, got:\n%s", buf.String())
	}
}
