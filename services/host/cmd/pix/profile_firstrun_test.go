package main

import (
	"bytes"
	"pix/host/workflow/provision"
	"strings"
	"testing"
)

// provision.FirstRunFlow now only PRINTS a nudge (setup is explicit); it never handles the
// invocation and never launches anything, on any TTY state.
func TestFirstRunFlowNudgesNeverHandles(t *testing.T) {
	for _, tty := range []bool{false, true} {
		var out bytes.Buffer
		if handled := provision.FirstRunFlow(strings.NewReader(""), &out, tty); handled {
			t.Errorf("tty=%v: first run must never handle the invocation", tty)
		}
		s := out.String()
		if !strings.Contains(s, "pix setup") || !strings.Contains(s, "pix run") {
			t.Errorf("tty=%v: nudge should mention setup + run: %q", tty, s)
		}
	}
}
