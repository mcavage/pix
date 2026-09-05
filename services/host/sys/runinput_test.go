package sys

import (
	"strings"
	"testing"
)

// runinput_test.go pins the ONE seam that exists so a value never reaches a
// process table: RunInput writes it to the child's STDIN, and the argv the
// child (and every `ps` on the host) sees carries nothing but flags and
// names.

// TestRealRunInput_ValueTravelsOnStdinNotArgv is the property itself, proven
// against a REAL child process that prints its own argv and its own stdin
// separately. A value visible in the argv half would be visible in `ps`.
func TestRealRunInput_ValueTravelsOnStdinNotArgv(t *testing.T) {
	const val = "sk-never-in-argv"
	out, err := Real{}.RunInput(val,
		"sh", "-c", `for a in "$@"; do printf 'argv:%s\n' "$a"; done; printf 'stdin:'; cat`,
		"sh", "--sandbox", "pix-demo", "anthropic")
	if err != nil {
		t.Fatalf("RunInput: %v (out=%q)", err, out)
	}
	if !strings.Contains(out, "stdin:"+val) {
		t.Errorf("the value never reached the child's stdin, got:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "argv:") && strings.Contains(line, val) {
			t.Errorf("the value reached the child's argv: %q", line)
		}
	}
}

// TestRealRunInput_ReturnsCombinedOutputAndError: a failing child's output is
// still captured, exactly as Run's contract promises, so a caller can report
// (and redact) sbx's own complaint.
func TestRealRunInput_ReturnsCombinedOutputAndError(t *testing.T) {
	out, err := Real{}.RunInput("ignored", "sh", "-c", "echo boom >&2; exit 7")
	if err == nil {
		t.Fatal("want the child's nonzero exit reported as an error")
	}
	if !strings.Contains(out, "boom") {
		t.Errorf("want the child's stderr captured, got %q", out)
	}
}

// TestRealIsStillACompleteSystem: RunInput joins the Exec interface, so the
// production implementation must satisfy it (a compile-time claim worth
// making explicitly here too).
func TestRealIsStillACompleteSystem(t *testing.T) {
	var _ System = Real{}
	var _ Exec = Real{}
}
