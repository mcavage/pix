package systest

import (
	"strings"
	"testing"
)

// TestFakeRunInput_RecordsArgvNeverTheInput: Calls is a diagnostic a failing
// test prints, so it must carry the command shape and NOTHING a caller fed
// over stdin. A fixture that wants to assert the input asserts it in its own
// RunInputFn, where the value is deliberate rather than ambient.
func TestFakeRunInput_RecordsArgvNeverTheInput(t *testing.T) {
	const val = "sk-never-recorded"
	var gotInput string
	f := &Fake{RunInputFn: func(input, name string, args ...string) (string, error) {
		gotInput = input
		return "", nil
	}}
	if _, err := f.RunInput(val, "sbx", "secret", "set", "-f", "--sandbox", "pix-demo", "anthropic"); err != nil {
		t.Fatalf("RunInput: %v", err)
	}
	if gotInput != val {
		t.Errorf("RunInputFn saw input %q, want %q", gotInput, val)
	}
	if !f.Ran("sbx secret set -f --sandbox pix-demo anthropic") {
		t.Errorf("the argv was not recorded, Calls = %v", f.Calls)
	}
	if strings.Contains(strings.Join(f.Calls, "\n"), val) {
		t.Errorf("the input leaked into Calls: %v", f.Calls)
	}
}

// TestFakeRunInput_FallsBackToRun keeps the many fixtures that wire only
// RunFn working: the fallback substitutes a WIRED seam for an unwired one
// (exactly what RunTimed already does), and the input is dropped rather than
// smuggled into the argv the fixture sees.
func TestFakeRunInput_FallsBackToRun(t *testing.T) {
	var gotArgs []string
	f := &Fake{RunFn: func(name string, args ...string) (string, error) {
		gotArgs = append([]string{name}, args...)
		return "ok", nil
	}}
	out, err := f.RunInput("sk-dropped", "sbx", "secret", "set", "anthropic")
	if err != nil || out != "ok" {
		t.Fatalf("RunInput = (%q, %v), want (\"ok\", nil)", out, err)
	}
	if strings.Contains(strings.Join(gotArgs, " "), "sk-dropped") {
		t.Errorf("the input became an argv element: %v", gotArgs)
	}
	if !f.Ran("sbx secret set anthropic") {
		t.Errorf("the argv was not recorded, Calls = %v", f.Calls)
	}
}

// TestFakeRunInput_UnwiredRefusesLoudly: with neither seam wired, the fake
// refuses by name rather than answering silently.
func TestFakeRunInput_UnwiredRefusesLoudly(t *testing.T) {
	f := &Fake{}
	if _, err := f.RunInput("x", "sbx", "secret", "set"); err == nil {
		t.Fatal("an unwired RunInput must refuse")
	} else if !strings.Contains(err.Error(), "RunInput") {
		t.Errorf("the refusal must name the method, got: %v", err)
	}
}
