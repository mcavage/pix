package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/cli"
)

// reset_cmd_test.go proves M2 (security re-review): pix reset has no
// --keep-memory/--keep-sandboxes/--force flags left to ignore, --yes is the
// ONLY noninteractive confirmation escape, and an interactive reset shows
// the exact operations and defaults to No before anything mutates. Every
// test drives `dispatch` (the real entry point), sbx forced ABSENT via an
// empty PATH, so a run that reaches past the confirmation gate is provably
// attempting the real sweep step, not a stub.

// resetTestHome makes a PIX_HOME with one marker file, so a test can prove
// "untouched" (the file is still readable at its original path) versus
// "moved aside" (the whole directory is gone, renamed to a .bak-<ts>
// sibling) without depending on ResetHome's own internals.
func resetTestHome(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "pixhome")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "AGENTS.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

func resetTestDeps(t *testing.T, home string) (*cli.Deps, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	t.Setenv("PATH", t.TempDir()) // sbx forced absent: a sweep that runs anyway is provably attempted, never silently skipped
	t.Setenv("PIX_HOME", home)
	var out, errb bytes.Buffer
	return &cli.Deps{Out: &out, Err: &errb}, &out, &errb
}

func requireHomeUntouched(t *testing.T, home string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(home, "AGENTS.md")); err != nil {
		t.Fatalf("PIX_HOME must be untouched by a refusal: %v", err)
	}
	matches, _ := filepath.Glob(home + ".bak-*")
	if len(matches) != 0 {
		t.Fatalf("a refusal must never rename PIX_HOME aside; found backup(s): %v", matches)
	}
}

// TestResetCmd_NoIgnoredFlags proves the flags M2 names are actually GONE
// from the struct, not merely undocumented: kong must reject them the same
// way it rejects any other unknown flag.
func TestResetCmd_NoIgnoredFlags(t *testing.T) {
	for _, flag := range []string{"--keep-memory", "--keep-sandboxes", "--force"} {
		home := resetTestHome(t)
		d, _, errb := resetTestDeps(t, home)
		d.Interactive = false
		d.In = strings.NewReader("")

		code := dispatch([]string{"reset", flag}, d)
		if code == 0 {
			t.Errorf("dispatch(%q) exit = 0, want nonzero (an unknown flag)", flag)
		}
		if !strings.Contains(errb.String(), "unknown flag") {
			t.Errorf("dispatch(%q) stderr = %q, want an unknown-flag refusal", flag, errb.String())
		}
		requireHomeUntouched(t, home)
	}
}

// TestResetCmd_NonInteractiveWithoutYes_RefusesBeforeAnyBillOrMutation is
// the existing noninteractive escape, still required: no --yes on a
// non-interactive terminal refuses immediately, before printing the
// operations bill (a script capturing stdout should see nothing) and before
// anything is swept or renamed.
func TestResetCmd_NonInteractiveWithoutYes_RefusesBeforeAnyBillOrMutation(t *testing.T) {
	home := resetTestHome(t)
	d, out, errb := resetTestDeps(t, home)
	d.Interactive = false

	code := dispatch([]string{"reset"}, d)

	if code == 0 {
		t.Fatalf("dispatch exit = 0, want nonzero (stdout=%q stderr=%q)", out.String(), errb.String())
	}
	if !strings.Contains(errb.String(), "non-interactive") || !strings.Contains(errb.String(), "--yes") {
		t.Fatalf("stderr = %q, want the fail-closed --yes refusal", errb.String())
	}
	if strings.Contains(out.String(), "pix reset will:") {
		t.Fatal("a non-interactive refusal must never print the operations bill")
	}
	requireHomeUntouched(t, home)
}

// TestResetCmd_Interactive_DefaultNoRefusesAndMutatesNothing proves the
// exact default-No shape M2 requires: an interactive terminal that answers
// anything other than "y" (a bare newline, the default) refuses, prints
// the confirmation as declined, and PIX_HOME is completely untouched — no
// sweep attempt, no container call, no rename.
func TestResetCmd_Interactive_DefaultNoRefusesAndMutatesNothing(t *testing.T) {
	home := resetTestHome(t)
	d, out, _ := resetTestDeps(t, home)
	d.Interactive = true
	d.In = strings.NewReader("\n") // bare Enter: default is No

	code := dispatch([]string{"reset"}, d)

	if code == 0 {
		t.Fatalf("dispatch exit = 0, want nonzero; stdout=%q", out.String())
	}
	if !strings.Contains(out.String(), "not confirmed") {
		t.Fatalf("stdout = %q, want the explicit not-confirmed refusal", out.String())
	}
	// The sweep step (rmAllSandboxes -> launch.Rm -> sbx absent) would have
	// printed its own "sbx not on PATH" refusal had it run at all; a decline
	// must never reach it.
	if strings.Contains(out.String(), "sweep") || strings.Contains(out.String(), "sbx not on PATH") {
		t.Fatalf("declining still reached the sweep step; stdout=%q", out.String())
	}
	requireHomeUntouched(t, home)
}

// TestResetCmd_Interactive_ShowsExactOperationsBeforePrompting proves the
// bill itself: a user must see the three fixed operations (sandboxes,
// then the memory container, then the PIX_HOME rename), in that order,
// before ever being asked to confirm.
func TestResetCmd_Interactive_ShowsExactOperationsBeforePrompting(t *testing.T) {
	home := resetTestHome(t)
	d, out, _ := resetTestDeps(t, home)
	d.Interactive = true
	d.In = strings.NewReader("n\n")

	_ = dispatch([]string{"reset"}, d)

	got := out.String()
	promptAt := strings.Index(got, "Proceed?")
	if promptAt < 0 {
		t.Fatalf("stdout = %q, want a Proceed? confirmation", got)
	}
	bill := got[:promptAt]
	wantInOrder := []string{"pix-* sandbox", "pix-memory", home}
	last := -1
	for _, want := range wantInOrder {
		i := strings.Index(bill, want)
		if i < 0 {
			t.Fatalf("bill (before the prompt) did not mention %q; bill=%q", want, bill)
		}
		if i < last {
			t.Fatalf("bill printed %q out of the fixed order; bill=%q", want, bill)
		}
		last = i
	}
}

// TestResetCmd_Interactive_AcceptProceedsPastTheGate proves the confirm
// gate is not a dead end for a "y": accepting moves on to the REAL reset
// sequence (ResetHome's own fixed order — sweep sandboxes first), never a
// second silent check. sbx is absent, so the sweep step itself refuses;
// what matters here is that refusal is reached ONLY after an explicit "y",
// proving accept -> mutation-attempt ordering rather than the gate being a
// no-op.
func TestResetCmd_Interactive_AcceptProceedsPastTheGate(t *testing.T) {
	home := resetTestHome(t)
	d, out, errb := resetTestDeps(t, home)
	d.Interactive = true
	d.In = strings.NewReader("y\n")

	code := dispatch([]string{"reset"}, d)

	if code == 0 {
		t.Fatalf("dispatch exit = 0, want nonzero (sbx is absent); stdout=%q stderr=%q", out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "Proceed?") {
		t.Fatalf("accept path must still have shown the prompt; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "sbx not on PATH") {
		t.Fatalf("an explicit \"y\" did not reach the real sweep step; stdout=%q stderr=%q", out.String(), errb.String())
	}
	// The sweep failure must have stopped ResetHome before the rename step:
	// PIX_HOME is untouched (a real reset would only fail earlier, never
	// leave PIX_HOME half-renamed).
	requireHomeUntouched(t, home)
}

// TestResetCmd_Yes_SkipsPromptButNotTheOperations proves --yes is ONLY the
// prompt escape: it still reaches the real sweep step (sbx absent refuses
// there), it just never shows the bill or asks.
func TestResetCmd_Yes_SkipsPromptButNotTheOperations(t *testing.T) {
	home := resetTestHome(t)
	d, out, errb := resetTestDeps(t, home)
	d.Interactive = false
	d.In = strings.NewReader("")

	code := dispatch([]string{"reset", "--yes"}, d)

	if code == 0 {
		t.Fatalf("dispatch exit = 0, want nonzero (sbx is absent); stdout=%q stderr=%q", out.String(), errb.String())
	}
	if strings.Contains(out.String(), "pix reset will:") || strings.Contains(out.String(), "Proceed?") {
		t.Fatalf("--yes must skip the bill/prompt entirely; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "sbx not on PATH") {
		t.Fatalf("--yes did not reach the real sweep step; stderr=%q", errb.String())
	}
	requireHomeUntouched(t, home)
}
