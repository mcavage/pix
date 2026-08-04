// retired_test.go — the retirement contract for the launcher's CLI surface.
//
// A retired surface is not a hidden surface: typing it must say so in a form a
// human AND a script can act on (PIX_RETIRED, exit 2, the exact replacement),
// and it must be inert — no config written, no daemon started, no sandbox
// touched. These tests pin the table, the message, and the fact that the
// retired names are gone from every discovery path (knownVerbs, help, usage).
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// manifestEntry mirrors corpus.RetirementEntry's fields this test needs. It is
// re-declared rather than imported so cmd/pix (L4) keeps no dependency on the
// test-only corpus package.
type manifestEntry struct {
	Granularity string `json:"granularity"`
	Verb        string `json:"verb"`
	Flag        string `json:"flag"`
	Status      string `json:"status"`
	Replacement string `json:"replacement"`
}

func loadRetirementManifest(t *testing.T) []manifestEntry {
	t.Helper()
	path := filepath.Join("corpus", "retirement.jsonl")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []manifestEntry
	for i, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e manifestEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("%s line %d: %v", path, i+1, err)
		}
		if e.Status == "approved" {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s has no approved entries", path)
	}
	return out
}

// TestRetiredTableMatchesManifest: the dispatch table and the append-only
// manifest are two views of one decision, so every approved entry must be
// dispatchable and every dispatched retirement must be approved. The manifest
// records replacements without the `pix ` prefix (historical entries), so both
// spellings are accepted.
func TestRetiredTableMatchesManifest(t *testing.T) {
	entries := loadRetirementManifest(t)
	inManifest := map[string]string{}
	for _, e := range entries {
		inManifest[retiredKey(e.Verb, e.Flag)] = e.Replacement
	}
	for key, want := range inManifest {
		got, ok := retiredSurfaces()[key]
		if !ok {
			t.Errorf("approved retirement %q has no entry in retiredSurfaces (an approved retirement must still answer when typed)", key)
			continue
		}
		if got != want && got != "pix "+want {
			t.Errorf("retirement %q: table replacement %q, manifest replacement %q", key, got, want)
		}
	}
	for key := range retiredSurfaces() {
		if _, ok := inManifest[key]; !ok {
			t.Errorf("retiredSurfaces has %q with no approved entry in corpus/retirement.jsonl (append one — retirement needs recorded approval)", key)
		}
	}
}

// TestRetiredMessageContract: the message is a machine-readable marker, the
// name that was typed, and the exact replacement — in that order, on stderr,
// with exit 2 at the call site.
func TestRetiredMessageContract(t *testing.T) {
	for key, replacement := range retiredSurfaces() {
		msg := retiredMessage(key)
		if !strings.HasPrefix(msg, "PIX_RETIRED") {
			t.Errorf("%q: message does not start with PIX_RETIRED:\n%s", key, msg)
		}
		if !strings.Contains(msg, strings.ReplaceAll(key, "\x00", " ")) {
			t.Errorf("%q: message does not name the retired surface:\n%s", key, msg)
		}
		terminal := terminalReplacement(key)
		if !strings.Contains(msg, terminal) {
			t.Errorf("%q: message does not name the replacement %q:\n%s", key, terminal, msg)
		}
		if !strings.HasSuffix(msg, "\n") {
			t.Errorf("%q: message is not newline-terminated", key)
		}
		_ = replacement
	}
}

// TestTerminalReplacementFollowsChains: a historical retirement whose recorded
// replacement was ITSELF later retired must not send the user to a second dead
// end — the message names the surface that still exists.
func TestTerminalReplacementFollowsChains(t *testing.T) {
	for key := range retiredSurfaces() {
		terminal := terminalReplacement(key)
		fields := strings.Fields(terminal)
		if len(fields) >= 2 && fields[0] == "pix" {
			if _, dead := retiredSurfaces()[retiredKey(fields[1], "")]; dead {
				t.Errorf("%q resolves to %q, whose verb is itself retired", key, terminal)
			}
		}
	}
	// gog -> gworkspace (retired) -> the live replacement.
	if got := terminalReplacement(retiredKey("gog", "")); got == "pix gworkspace" {
		t.Errorf("terminalReplacement(gog) = %q, want the live surface behind gworkspace", got)
	}
}

// TestRetiredVerbsAreGoneFromDiscovery: a retired verb must not survive in
// knownVerbs, in either help rendering, or in verbUsage — the three places a
// user or a test would learn the surface still exists.
func TestRetiredVerbsAreGoneFromDiscovery(t *testing.T) {
	for key := range retiredSurfaces() {
		verb, flag := retiredSplit(key)
		if flag != "" || verb == "pix" {
			continue
		}
		if knownVerbs[verb] {
			t.Errorf("retired verb %q is still in knownVerbs", verb)
		}
		if _, ok := verbUsage(verb); ok {
			t.Errorf("retired verb %q still has usage text (pix help %s)", verb, verb)
		}
	}
	for _, verb := range []string{"slack", "gworkspace", "knowledge", "upgrade", "backup", "restore"} {
		for name, text := range map[string]string{"helpText": helpText, "helpAllText": helpAllText} {
			for _, line := range strings.Split(text, "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), verb) {
					t.Errorf("%s still advertises retired verb %q:\n  %s", name, verb, line)
				}
			}
		}
	}
}

// TestRetiredSurfacesAreSorted keeps the table reviewable: the retirement set
// only grows, so a stable order is what makes the diff legible.
func TestRetiredSurfacesAreSorted(t *testing.T) {
	keys := make([]string, 0, len(retiredSurfaces()))
	for k := range retiredSurfaces() {
		keys = append(keys, k)
	}
	if len(keys) < 15 {
		t.Errorf("retiredSurfaces has %d entries, want the full W1 U01a set (>= 15)", len(keys))
	}
	sort.Strings(keys)
}
