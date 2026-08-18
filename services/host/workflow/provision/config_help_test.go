package provision

import (
	"strings"
	"testing"
)

// TestConfigKeysHelp_NoDeletedKnowledgeSurface: the built-in knowledge daemon
// (:11436) and every code path that lazily started it were deleted outright
// (W2 U03A, AGENTS.md's go-plugin+Suture section), not merely retired from
// the CLI. `host.autoserve`'s help line used to list "run/memory/knowledge"
// as the three surfaces that lazily start the services daemon; only run and
// memory do that today (service.EnsureUp's only two call sites). This is an
// anti-drift pin so a stale "knowledge" mention can't creep back into the
// LIVE `pix config --help` text.
func TestConfigKeysHelp_NoDeletedKnowledgeSurface(t *testing.T) {
	// The pack-embedded knowledge/ dir (packinfo.KnowledgeDir) is a real, still-
	// mounted, inert facet, so "knowledge" in the pack <path> line is legitimate
	// and must stay. Only the retired DAEMON's mention (autoserve lazily
	// starting a "knowledge" surface, which no longer exists) is the drift this
	// test guards against.
	for _, line := range strings.Split(ConfigKeysHelp, "\n") {
		if strings.Contains(line, "knowledge") && !strings.Contains(line, "pack <path>") {
			t.Errorf("ConfigKeysHelp mentions the deleted knowledge service in a live help line: %q", line)
		}
	}
}

// TestMemoryCaptureSummaryIsAsymmetric pins the DX fix: the two modes do not
// take effect the same way, and the confirmation line must not flatten that.
// Turning capture OFF is a HOST-side refusal that binds already-running
// sandboxes; turning it ON only reaches a sandbox launched after the change,
// and even then only if a watcher model is actually reachable — which this
// command does not verify, so it must name a check rather than report
// success.
func TestMemoryCaptureSummaryIsAsymmetric(t *testing.T) {
	off := memoryCaptureSummary("explicit")
	if !strings.Contains(off, "immediately") || !strings.Contains(off, "already-running") {
		t.Errorf("explicit summary must say the host refusal is immediate for running sandboxes, got: %q", off)
	}
	if strings.Contains(off, "NEW sandboxes only") {
		t.Errorf("explicit summary must NOT claim new-sandboxes-only, got: %q", off)
	}

	on := memoryCaptureSummary("experimental-auto")
	if !strings.Contains(on, "NEW sandboxes only") {
		t.Errorf("experimental-auto summary must say it reaches new sandboxes only, got: %q", on)
	}
	if !strings.Contains(on, "watcher model") || !strings.Contains(on, "ollama list") {
		t.Errorf("experimental-auto summary must warn that a watcher model is required and name the verification command, got: %q", on)
	}
	// Success words a probe would have to earn (AGENTS.md invariant 12).
	// "ready" is checked as a whole word only: "already-running" legitimately
	// contains it.
	for _, claim := range []string{"enabled", " ready", "verified", "working"} {
		if strings.Contains(on, claim) {
			t.Errorf("experimental-auto summary must not claim %q: nothing here probed the watcher, got: %q", claim, on)
		}
	}
}
