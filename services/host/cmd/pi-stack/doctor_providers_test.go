package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestProvidersGroup_OneKeyReady: any ONE of anthropic/openai/google present
// makes the core "model key" check ready \u2014 core, no todo, never blocking.
func TestProvidersGroup_OneKeyReady(t *testing.T) {
	g := providersGroup(nil, "anthropic\ngithub\n", true)
	core := g.checks[0]
	if core.label != "model key" {
		t.Fatalf("expected the core check first, got %q", core.label)
	}
	if core.req() != requirementCore {
		t.Errorf("model key requirement = %q, want core", core.req())
	}
	if core.result() != verdictReady {
		t.Errorf("model key verdict = %q, want ready", core.result())
	}
	if core.todo != "" {
		t.Errorf("a ready core check must carry no todo, got %q", core.todo)
	}
	if blockingCheck(core.req(), core.result()) {
		t.Error("a ready core check must never block")
	}
}

// TestProvidersGroup_ZeroConfirmed_OneCopyPasteableCommand: sbx reachable,
// POSITIVELY zero model keys set -> todo, core, blocking, with EXACTLY one
// copy-pasteable command (the other providers are named in evidence, not the
// command).
func TestProvidersGroup_ZeroConfirmed_OneCopyPasteableCommand(t *testing.T) {
	g := providersGroup(nil, "github\n", true) // github set, but no MODEL key
	core := g.checks[0]
	if core.req() != requirementCore {
		t.Fatalf("model key requirement = %q, want core", core.req())
	}
	if core.result() != verdictTodo {
		t.Fatalf("model key verdict = %q, want todo", core.result())
	}
	if !blockingCheck(core.req(), core.result()) {
		t.Error("a positively-confirmed-zero core check must block")
	}
	if core.todo == "" {
		t.Fatal("expected a copy-pasteable fix command")
	}
	// Exactly one command: no "&&"-free alternative list, no second `pi-stack
	// secret set` invocation baked into the command string itself.
	if strings.Count(core.todo, "pi-stack secret set") != 1 {
		t.Errorf("todo must name exactly one `pi-stack secret set` invocation, got %q", core.todo)
	}
	// Alternatives belong in evidence, never as a second command.
	if !strings.Contains(core.evidenceString(), "openai") || !strings.Contains(core.evidenceString(), "google") {
		t.Errorf("evidence should name the alternative providers, got %q", core.evidenceString())
	}
}

// TestProvidersGroup_SbxAbsentUnverifiable: sbx absent -> unverifiable, never
// denied, never blocking, and carries no todo command.
func TestProvidersGroup_SbxAbsentUnverifiable(t *testing.T) {
	g := providersGroup(nil, "", false)
	core := g.checks[0]
	if core.result() != verdictUnverifiable {
		t.Fatalf("model key verdict = %q, want unverifiable", core.result())
	}
	if core.result() == verdictDenied {
		t.Error("sbx-absent must never be classified denied")
	}
	if blockingCheck(core.req(), core.result()) {
		t.Error("an unverifiable core check must never block")
	}
	if core.todo != "" {
		t.Errorf("an unverifiable check must carry no todo, got %q", core.todo)
	}
}

// TestProvidersGroup_SecretLsFailure_Unverifiable mirrors the sbx-absent case
// for the OTHER unverifiable cause: sbx present but `sbx secret ls` errored
// (control plane down). Same tri-state sbxOK=false path as sbxModelKeyState.
func TestProvidersGroup_SecretLsFailure_Unverifiable(t *testing.T) {
	g := providersGroup(nil, "", false) // sbxOK=false covers both "absent" and "errored"
	core := g.checks[0]
	if core.result() != verdictUnverifiable {
		t.Fatalf("verdict = %q, want unverifiable", core.result())
	}
}

// TestProvidersGroup_AlternateMissingNotOutstanding: once one provider is
// confirmed present, the still-missing alternates are informational only \u2014
// they never count as outstanding.
func TestProvidersGroup_AlternateMissingNotOutstanding(t *testing.T) {
	// anthropic set, openai/google/github unset. The baked overlord default
	// (run_intent -> openai/gpt-5.6-sol) makes the run_intent check advise
	// repointing run_intent, but that is a NOTE (never outstanding): a wrong
	// session-model vendor is a config fix, not a blocking gap, and the core
	// "at least one key" gate is satisfied.
	g := providersGroup(nil, "anthropic\n", true)
	r := &report{groups: []group{g}}
	if got := r.outstanding(); got != 0 {
		t.Errorf("outstanding = %d, want 0 (missing alternates + the run_intent advisory are informational)", got)
	}
	for _, c := range g.checks[1:] {
		if !c.note {
			t.Errorf("per-provider check %q must be a note (informational), got %+v", c.label, c)
		}
	}
	// The one todo present is the session-model advisory (set the missing session
	// provider's key), not a blocking item: anthropic is set (core satisfied), but
	// the baked overlord -> openai needs an openai key.
	if td := r.todos(); len(td) != 1 || !strings.Contains(td[0], "OPENAI_API_KEY") {
		t.Errorf("todos = %v, want exactly the session-model key advisory (OPENAI_API_KEY)", td)
	}
}

// TestProvidersGroup_GithubOptional_NoOutstanding: github absent is
// not-configured/info \u2014 never outstanding \u2014 even when zero MODEL keys are
// set (the model-key core todo is the only outstanding item).
func TestProvidersGroup_GithubOptional_NoOutstanding(t *testing.T) {
	g := providersGroup(nil, "", true) // sbx reachable, nothing set at all
	r := &report{groups: []group{g}}
	if got := r.outstanding(); got != 1 {
		t.Errorf("outstanding = %d, want 1 (only the core model-key todo)", got)
	}
	var github check
	found := false
	for _, c := range g.checks {
		if c.label == "github" {
			github, found = c, true
		}
	}
	if !found {
		t.Fatal("expected a github info line")
	}
	if !github.note {
		t.Error("github must be an informational note, never a second requirement")
	}
	if strings.Contains(github.detail, "set") && !strings.Contains(github.detail, "not configured") {
		t.Errorf("github detail = %q", github.detail)
	}
}

// TestProvidersGroup_GithubProbeFailure_Unverifiable: sbx absent/errored ->
// github's info line reads unverifiable (via detail text), not a false claim.
func TestProvidersGroup_GithubProbeFailure_Unverifiable(t *testing.T) {
	g := providersGroup(nil, "", false)
	var github check
	for _, c := range g.checks {
		if c.label == "github" {
			github = c
		}
	}
	if !github.note {
		t.Fatal("github must remain a note even when unverifiable")
	}
	if !strings.Contains(github.detail, "cannot verify") {
		t.Errorf("github detail = %q, want a cannot-verify note", github.detail)
	}
}

// TestProvidersGroup_JSON: exact JSON contract for the core check and the
// per-provider info lines.
func TestProvidersGroup_JSON(t *testing.T) {
	r := &report{groups: []group{providersGroup(nil, "anthropic\n", true)}}
	v := r.jsonView("")
	if v.Blocking {
		t.Error("one confirmed key must not block")
	}
	byLabel := map[string]doctorCheckJSON{}
	for _, c := range v.Groups[0].Checks {
		byLabel[c.Label] = c
	}
	core := byLabel["model key"]
	if core.Requirement != "core" || core.Verdict != "ready" || core.Todo != "" {
		t.Errorf("model key JSON = %+v", core)
	}
	// openai is a not-configured informational note: it stays a note (never
	// blocks/counts as outstanding) but its VERDICT must be truthful —
	// unverifiable, not a blanket ready, since "not configured" is not a
	// verified-working claim (DX JSON finding 2).
	openai := byLabel["openai"]
	if !openai.Note || openai.Verdict != "unverifiable" || openai.Todo != "" || openai.State != "info" {
		t.Errorf("openai JSON (note) = %+v", openai)
	}
	if openai.Detail != "not configured" {
		t.Errorf("openai detail = %q, want not configured", openai.Detail)
	}
}

// TestProvidersGroup_JSON_ZeroConfirmed: the blocked-JSON shape when zero keys
// are confirmed.
func TestProvidersGroup_JSON_ZeroConfirmed(t *testing.T) {
	r := &report{groups: []group{providersGroup(nil, "", true)}}
	v := r.jsonView("")
	if !v.Blocking || v.Verdict != "blocked" {
		t.Errorf("zero confirmed -> (blocking=%v, verdict=%q), want (true, blocked)", v.Blocking, v.Verdict)
	}
	core := v.Groups[0].Checks[0]
	if core.Todo == "" {
		t.Error("expected the JSON todo field populated for the confirmed-zero core check")
	}
}

// TestProvidersGroup_RenderConcise: concise mode collapses the ready per-key
// notes/checks but still shows the group is all-ready when a key is present.
func TestProvidersGroup_RenderConcise(t *testing.T) {
	r := &report{groups: []group{providersGroup(nil, "anthropic\nopenai\ngoogle\ngithub\n", true)}}
	var buf bytes.Buffer
	r.render(&buf, false)
	out := buf.String()
	if !strings.Contains(out, "Providers / keys") {
		t.Errorf("expected the providers group title, got:\n%s", out)
	}
	if strings.Contains(out, "TODO:") {
		t.Errorf("a satisfied core check should render no TODO, got:\n%s", out)
	}
}

// TestProvidersGroup_RenderVerbose: verbose mode shows every per-provider
// info line, not just the collapsed core summary.
func TestProvidersGroup_RenderVerbose(t *testing.T) {
	r := &report{groups: []group{providersGroup(nil, "anthropic\n", true)}}
	var buf bytes.Buffer
	r.render(&buf, true)
	out := buf.String()
	for _, want := range []string{"anthropic", "openai", "google", "github", "model key"} {
		if !strings.Contains(out, want) {
			t.Errorf("verbose render missing %q, got:\n%s", want, out)
		}
	}
}

// TestProvidersGroup_RenderZeroConfirmed_OneTodoLine: the concise render must
// surface exactly the one fix command, in the TODO section.
func TestProvidersGroup_RenderZeroConfirmed_OneTodoLine(t *testing.T) {
	r := &report{groups: []group{providersGroup(nil, "", true)}}
	var buf bytes.Buffer
	r.render(&buf, false)
	out := buf.String()
	if strings.Count(out, "TODO: pi-stack secret set") != 1 {
		t.Errorf("expected exactly one provider TODO line, got:\n%s", out)
	}
}

// sbxSecretLsScript writes a fake `sbx` on the given dir's PATH that answers
// ONLY `secret ls` (with the given output/exit code) and no-ops everything
// else (0, empty), so a real subprocess run of runDoctorCmd can exercise the
// exact tri-state sbxModelKeyState/probeSbxSecrets share, without a real sbx
// install.
func sbxSecretLsScript(t *testing.T, dir, output string, exitCode int) {
	t.Helper()
	body := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"secret\" ] && [ \"$2\" = \"ls\" ]; then\n  printf '%%s' '%s'\n  exit %d\nfi\nexit 0\n", output, exitCode)
	if err := os.WriteFile(filepath.Join(dir, "sbx"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestDoctorCmd_RealExitCodes exercises runDoctorCmd's ACTUAL process exit
// code end to end (real os.Exit, real defaultShellEnv, a real fake `sbx` on
// PATH) \u2014 not just the in-process report/blocking() unit tests above.
func TestDoctorCmd_RealExitCodes(t *testing.T) {
	cases := []struct {
		name     string
		argv     []string
		sbxOut   string
		sbxExit  int
		noSbx    bool
		wantExit int
	}{
		{name: "one_key_present", sbxOut: "anthropic\n", sbxExit: 0, wantExit: 0},
		{name: "zero_keys_confirmed", sbxOut: "", sbxExit: 0, wantExit: 1},
		{name: "usage_error", argv: []string{"--bogus"}, wantExit: 2},
		{name: "probe_failed", sbxExit: 7, wantExit: 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if os.Getenv("PI_STACK_DOCTOR_HELPER") == tc.name {
				runDoctorCmd(tc.argv)
				return
			}
			dir := t.TempDir()
			if !tc.noSbx {
				sbxSecretLsScript(t, dir, tc.sbxOut, tc.sbxExit)
			}
			cmd := exec.Command(os.Args[0], "-test.run", "TestDoctorCmd_RealExitCodes/"+tc.name)
			cmd.Env = append(os.Environ(),
				"PI_STACK_DOCTOR_HELPER="+tc.name,
				"PATH="+dir,
				"PI_STACK_CONFIG="+filepath.Join(dir, "config.toml"), // absent file -> defaults
				"HOME="+dir,
			)
			out, err := cmd.CombinedOutput()
			exit := 0
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				exit = ee.ExitCode()
			} else if err != nil {
				t.Fatalf("unexpected run error: %v\noutput:\n%s", err, out)
			}
			if exit != tc.wantExit {
				t.Errorf("%s: exit = %d, want %d\noutput:\n%s", tc.name, exit, tc.wantExit, out)
			}
		})
	}
}
