package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNoRawGogAuthLoginInProductionSource is a SOURCE guard, not a rendered-
// output guard: it scans every production .go file in this package (i.e. every
// *.go file that is NOT a _test.go file) for the raw legacy `gog auth login`
// phrase and fails the build if it ever reappears — in a string literal OR a
// comment. The one guided recovery command status/doctor/mcp ever print or
// write about is `pix gworkspace setup` (gogSetupHint); the direct legacy
// command must never regress into a TODO, a rendered label, or guidance prose
// again (that regression is exactly what S11 shipped and this guard catches
// mechanically instead of relying on review to notice it a second time).
//
// Test files are intentionally OUT OF SCOPE for this guard: they legitimately
// reference the phrase two ways that are not user guidance at all —
// (1) as banned input asserted against (e.g. `strings.Contains(out, "gog auth
// login")` guarding rendered/production output), and (2) as the real
// capability-probe argv/fixture text for the legacy `add-client+login` route,
// which the installed gog CLI genuinely exposes and gog_setup.go genuinely
// invokes (with --readonly) when that route is the one detected — that is
// implementation argv, not user-facing guidance, and is exercised in
// gog_setup_test.go's fixtures.
func TestNoRawGogAuthLoginInProductionSource(t *testing.T) {
	const banned = "gog auth login"

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir(.): %v", err)
	}
	var hits []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if name == "copy_guard_test.go" {
			continue // this guard has to name the banned phrase to look for it
		}
		b, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", name, err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, banned) {
				hits = append(hits, name+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
	}
	if len(hits) > 0 {
		t.Errorf("raw legacy `%s` guidance is banned from production source (use gogSetupHint / \"pix gworkspace setup\" instead), found:\n%s",
			banned, strings.Join(hits, "\n"))
	}
}
