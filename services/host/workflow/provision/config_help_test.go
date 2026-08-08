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
