package main

// phase10_dx_test.go covers the Phase-10 product/DX findings:
//
//   DX-2  status and doctor must agree on sbx-absent semantics (likely inside
//         the sandbox): neither presumes a host-install fix from absence
//         alone, and both point at running `pi-stack doctor` on the host.
//   DX-3  the aggregate model-keys TODO must be a bare, copy-pasteable
//         command with no parenthetical/alternatives baked in.
//   DX-4  a verified CORE (blocking) failure line must be visually distinct
//         from an optional failure — both render \u2717, but the core one is
//         marked "(required)".
//   DX-5  the "--verbose for full detail" hint only prints when concise mode
//         actually hid a healthy check; a cold/all-todo run collapses nothing,
//         so no hint.

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"pi-stack/host/config"
)

// --- DX-2 --------------------------------------------------------------

// TestStatusDoctorAgreeOnSbxAbsent: given the identical sbx-absent env, status
// must not claim an "install the Docker Sandboxes CLI" repair action (that
// presumes this IS the host and sbx is genuinely missing, when absence just as
// likely means running inside the sandbox, where sbx is structurally never
// there) and must share doctor's own advice: run `pi-stack doctor` on the host.
func TestStatusDoctorAgreeOnSbxAbsent(t *testing.T) {
	cfg := &config.Config{}
	env := shellEnv{
		lookPath: func(string) (string, error) { return "", fmt.Errorf("not found") },
		dial:     func(int) bool { return false },
		statFile: func(string) bool { return false },
	}

	dr := runDoctor(cfg, env)
	var dbuf bytes.Buffer
	dr.render(&dbuf, false)
	doctorOut := dbuf.String()
	if !strings.Contains(doctorOut, "run `pi-stack doctor` on the host") {
		t.Fatalf("doctor's own sbx-absent note changed shape, update this test's expectation:\n%s", doctorOut)
	}

	// status's own perspective lives in its Todos (the machine-readable list
	// --json exposes and the human render summarizes by count) — check the
	// actual guidance text there, not the human summary line (which is just a
	// generic "N outstanding" count regardless of content).
	st := gatherStatus(cfg, "default", env)
	joined := strings.Join(st.Todos, "\n")
	if strings.Contains(joined, "install the Docker Sandboxes CLI") {
		t.Errorf("status must not presume a host-install fix merely because sbx is absent (may be inside the sandbox), got: %v", st.Todos)
	}
	if !strings.Contains(joined, "pi-stack doctor` on the host") {
		t.Errorf("status must advise running doctor on the host, matching doctor's own perspective, got: %v", st.Todos)
	}
}

// TestStatusDoctorAgreeOnOneKeyGitHubOptional is finding #4: given the
// identical env (one model-provider key set, github unset), bare `pi-stack`
// (status) and `pi-stack doctor` must agree there is nothing outstanding --
// neither the unused model-key alternatives nor the missing optional github
// key may be reported as a gap by either command.
func TestStatusDoctorAgreeOnOneKeyGitHubOptional(t *testing.T) {
	cfg := &config.Config{}
	f := fakeEnv{
		present: map[string]bool{"sbx": true},
		output: map[string]string{
			"sbx secret ls": "anthropic\n",
			"sbx mcp ls":    "",
		},
		ports: map[int]bool{11435: true},
	}
	env := f.env()

	dr := runDoctor(cfg, env)
	if dr.blocking() {
		t.Error("doctor must not block with one model key present")
	}
	if len(dr.todos()) != 0 {
		t.Errorf("doctor must report zero todos (one key present, github optional), got %v", dr.todos())
	}

	st := gatherStatus(cfg, "default", env)
	if len(st.Todos) != 0 {
		t.Errorf("status must report zero todos (one key present, github optional), got %v", st.Todos)
	}

	var out bytes.Buffer
	renderStatus(cfg, "default", env, &out, false)
	if strings.Contains(out.String(), "outstanding") {
		t.Errorf("bare status must not report anything outstanding, got:\n%s", out.String())
	}
}

// --- DX-3 ----------------------------------------------------------------

// TestModelKeysAggregateTodo_BareCommand: the zero-of-three-keys TODO must be
// a single, bare, copy-pasteable command \u2014 no trailing parenthetical, no
// shell-unsafe characters ('(', ')') that would break a straight paste.
func TestModelKeysAggregateTodo_BareCommand(t *testing.T) {
	c := modelProviderAggregateCheck(0, true, true)
	if c.todo == "" {
		t.Fatal("expected a todo for zero-of-three keys")
	}
	if strings.ContainsAny(c.todo, "()") {
		t.Errorf("todo must not contain a parenthetical, got %q", c.todo)
	}
	fields := strings.Fields(c.todo)
	if len(fields) == 0 {
		t.Fatal("todo has no fields")
	}
	// The whole string must be exactly the executable command: joining the
	// fields back with single spaces must reproduce it verbatim (no embedded
	// commentary/alternatives glued on after the command).
	if strings.Join(fields, " ") != c.todo {
		t.Errorf("todo is not a bare single command, got %q", c.todo)
	}
	if !strings.HasPrefix(c.todo, "sbx secret set -g ") {
		t.Errorf("todo should be a plain `sbx secret set -g <provider>` command, got %q", c.todo)
	}
	// The alternatives/caveat (any one of the three keys is enough) still needs
	// to live SOMEWHERE \u2014 just not in the todo. It belongs in detail.
	if !strings.Contains(c.detail, "any one") {
		t.Errorf("expected the any-one-of-three caveat to live in detail, got %q", c.detail)
	}
}

// --- DX-4 ----------------------------------------------------------------

// TestRender_CoreBlockingCheckMarkedRequired: a verified CORE failure (model
// keys, zero of three) renders with an explicit "(required)" marker so it
// reads as distinct from an optional \u2717 with no such hierarchy.
func TestRender_CoreBlockingCheckMarkedRequired(t *testing.T) {
	f := fakeEnv{
		present: map[string]bool{"sbx": true},
		output: map[string]string{
			"sbx secret ls": "github\n", // zero of anthropic/openai/google
			"sbx mcp ls":    "",
		},
		ports: map[int]bool{11435: true},
	}
	r := runDoctor(defaultCfg(), f.env())
	var buf bytes.Buffer
	r.render(&buf, true) // verbose: every line renders, including the core failure
	out := buf.String()
	if !strings.Contains(out, "✗ model keys (required)") {
		t.Errorf("expected the blocking core check marked (required), got:\n%s", out)
	}
}

// TestRender_OptionalFailurePlainNoRequiredMarker: an optional (non-core)
// verified failure (ollama installed, daemon down) must NOT get the
// "(required)" marker \u2014 only a verified CORE failure does.
func TestRender_OptionalFailurePlainNoRequiredMarker(t *testing.T) {
	cfg := defaultCfg()
	f := fakeEnv{
		present: map[string]bool{"sbx": true, "ollama": true},
		output: map[string]string{
			"sbx secret ls": "anthropic\nopenai\ngoogle\ngithub\n",
			"sbx mcp ls":    "",
		},
		ports: map[int]bool{11435: true}, // 11434 (ollama daemon) NOT open -> down
	}
	r := runDoctor(cfg, f.env())
	var buf bytes.Buffer
	r.render(&buf, true)
	out := buf.String()
	if !strings.Contains(out, "✗ ollama") {
		t.Fatalf("expected a verified ollama failure line, got:\n%s", out)
	}
	if strings.Contains(out, "ollama") && strings.Contains(out, "ollama (required)") {
		t.Errorf("optional ollama failure must not carry the (required) marker, got:\n%s", out)
	}
	for _, ln := range strings.Split(out, "\n") {
		if strings.Contains(ln, "✗ ollama") && strings.Contains(ln, "(required)") {
			t.Errorf("ollama's ✗ line must stay plain (it's optional), got line: %q", ln)
		}
	}
}

// --- DX-5 ----------------------------------------------------------------

// TestRender_NoVerboseHintWhenNothingCollapsed: when every check in the
// report is already shown (nothing healthy got hidden by concise mode \u2014 a
// cold/all-failing run), the "--verbose for full detail" hint must not print;
// there is nothing extra --verbose would reveal.
func TestRender_NoVerboseHintWhenNothingCollapsed(t *testing.T) {
	cfg := defaultCfg()
	f := fakeEnv{
		present: map[string]bool{}, // sbx absent entirely -> nothing verified healthy
		output:  map[string]string{},
		ports:   map[int]bool{}, // memory down too
	}
	r := runDoctor(cfg, f.env())
	var buf bytes.Buffer
	r.render(&buf, false) // concise
	out := buf.String()
	if strings.Contains(out, "--verbose") {
		t.Errorf("no healthy check was hidden here, so the --verbose hint should not print, got:\n%s", out)
	}
}

// TestRender_VerboseHintWhenCollapsed: when a healthy check WAS hidden by
// concise mode, the hint must print.
func TestRender_VerboseHintWhenCollapsed(t *testing.T) {
	f := gogConfirmed(fakeEnv{
		present: map[string]bool{"sbx": true, "ollama": true},
		output: map[string]string{
			"sbx secret ls": "anthropic\nopenai\ngoogle\ngithub\n",
			"ollama list":   "NAME\ngemma4:latest\nnomic-embed-text:latest\n",
		},
		ports: map[int]bool{11435: true, 11434: true},
	})
	r := runDoctor(defaultCfg(), f.env())
	var buf bytes.Buffer
	r.render(&buf, false) // concise: providers group is all-healthy -> collapses
	out := buf.String()
	if !strings.Contains(out, "--verbose") {
		t.Errorf("a healthy check was hidden here, expected the --verbose hint, got:\n%s", out)
	}
}

// TestRender_ConciseIdenticalWhenColdVsCollapsed_NoFalseHint is the DX-5
// identical/no-hint-on-cold-output regression: rendering the SAME cold
// (nothing healthy) report twice concise must be byte-identical and neither
// carries the hint.
func TestRender_ConciseIdenticalWhenColdVsCollapsed_NoFalseHint(t *testing.T) {
	cfg := defaultCfg()
	f := fakeEnv{present: map[string]bool{}, output: map[string]string{}, ports: map[int]bool{}}
	r1 := runDoctor(cfg, f.env())
	r2 := runDoctor(cfg, f.env())
	var b1, b2 bytes.Buffer
	r1.render(&b1, false)
	r2.render(&b2, false)
	if b1.String() != b2.String() {
		t.Fatalf("expected identical concise output for identical cold input:\n%s\n---\n%s", b1.String(), b2.String())
	}
	if strings.Contains(b1.String(), "--verbose") {
		t.Errorf("cold output must not carry the --verbose hint, got:\n%s", b1.String())
	}
}
