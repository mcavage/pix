// docs_retired_test.go — the docs half of retirement parity. A verb can be
// gone from the binary and still be advertised by the README, the reference
// map, or the man page, which is how a user ends up typing something that
// cannot work. These tests read the shipped docs and fail on any retired verb
// still presented as a live command.
package main

import (
	"regexp"
	"strings"
	"testing"
)

// retiredVerbNames is the verb-granularity retirement set, derived from the
// dispatch table so the docs check cannot drift from the CLI.
func retiredVerbNames(t *testing.T) []string {
	t.Helper()
	var out []string
	for key := range retiredSurfaces() {
		verb, flag := retiredSplit(key)
		if flag == "" && verb != "pix" {
			out = append(out, verb)
		}
	}
	if len(out) == 0 {
		t.Fatal("no verb-granularity retirements found in retiredSurfaces")
	}
	return out
}

func TestDocsDoNotAdvertiseRetiredVerbs(t *testing.T) {
	for _, doc := range []string{"README.md", "docs/reference.md"} {
		body := readRepoFile(t, doc)
		for _, verb := range retiredVerbNames(t) {
			re := regexp.MustCompile(`(?m)pix ` + regexp.QuoteMeta(verb) + `\b`)
			if loc := re.FindStringIndex(body); loc != nil {
				line := lineAround(body, loc[0])
				if strings.Contains(line, "PIX_RETIRED") || strings.Contains(line, "retired") {
					continue // an explicit retirement note is the one allowed mention
				}
				t.Errorf("%s still advertises retired verb %q:\n  %s", doc, verb, line)
			}
		}
	}
}

// lineAround returns the whole line containing offset, for a readable failure.
func lineAround(s string, off int) string {
	start := strings.LastIndexByte(s[:off], '\n') + 1
	end := strings.IndexByte(s[off:], '\n')
	if end < 0 {
		return s[start:]
	}
	return s[start : off+end]
}
