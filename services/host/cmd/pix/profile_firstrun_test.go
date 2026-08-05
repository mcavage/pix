package main

import (
	"bytes"
	"pix/host/workflow/provision"
	"strings"
	"testing"
)

// provision.FirstRunFlow only PRINTS a nudge (setup is explicit): it never
// handles the invocation, and it asks nothing — there is no TTY branch left to
// take, which is why it no longer takes a reader or a tty flag.
func TestFirstRunFlowNudgesNeverHandles(t *testing.T) {
	var out bytes.Buffer
	if handled := provision.FirstRunFlow(&out); handled {
		t.Error("first run must never handle the invocation")
	}
	if s := out.String(); !strings.Contains(s, "pix setup") || !strings.Contains(s, "pix run") {
		t.Errorf("nudge should mention setup + run: %q", s)
	}
}
