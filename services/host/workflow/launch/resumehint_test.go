package launch

import (
	"bytes"
	"strings"
	"testing"
)

// TestReportTeardownKeepsRoutineRemovalQuiet: pi prints the one exact,
// host-runnable resume command before it exits. A normal automatic removal is
// expected lifecycle cleanup, so the host launcher must not append a second
// line. Kept and failed teardown decisions still need to be visible.
func TestReportTeardownKeepsRoutineRemovalQuiet(t *testing.T) {
	var removed, kept bytes.Buffer
	reportTeardown(&removed, TeardownResult{Verdict: TeardownRemoved, Detail: "removed x"}, t.TempDir())
	reportTeardown(&kept, TeardownResult{Verdict: TeardownKeptBusy, Detail: "still referenced", Sandbox: "x"}, t.TempDir())
	if removed.Len() != 0 {
		t.Errorf("routine removal must stay silent, got %q", removed.String())
	}
	if !strings.Contains(kept.String(), "kept x: still referenced") {
		t.Errorf("a kept sandbox must remain visible, got %q", kept.String())
	}
}
