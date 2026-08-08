package sys

import "testing"

// TestIsUsageMismatch_Recognized pins the exact parser vocabularies a grammar
// change actually prints — cobra, kong, clap, and the Go stdlib flag package
// — the phrasings a real `sbx` CLI update is likely to use for "I don't
// understand this argv".
func TestIsUsageMismatch_Recognized(t *testing.T) {
	cases := []string{
		"Error: unknown flag: --version\nUsage:\n  sbx [command]",
		"Error: unknown shorthand flag: 'v' in -v",
		`Error: unknown command "version" for "sbx"`,
		"unrecognized command 'bundle'",
		"flag provided but not defined: -version",
		"no such flag -url",
		"Error: accepts 1 arg(s), received 2",
		"Error: accepts at most 2 arg(s), received 3",
		"unexpected argument 'https://example.com/mcp' found",
		"too many arguments",
		`Error: required flag(s) "url" not set`,
		`Error: required flag "url" not registered`,
	}
	for _, out := range cases {
		if !IsUsageMismatch(out) {
			t.Errorf("IsUsageMismatch(%q) = false, want true", out)
		}
	}
}

// TestIsUsageMismatch_OperationalNeverMatches: an auth/policy/environmental
// failure — even one that shares generic words like "error" or "invalid" —
// must NEVER classify as a grammar mismatch. This is the property that keeps
// every caller from retrying an unrelated failure with a different argv.
func TestIsUsageMismatch_OperationalNeverMatches(t *testing.T) {
	cases := []string{
		"401 Unauthorized",
		"error: unauthorized",
		"operation not permitted: blocked by organization policy",
		"dial tcp: connect: connection refused",
		"context deadline exceeded",
		"exit status 1",
		"panic: runtime error: index out of range",
		"error: invalid manifest URL (connection refused)",
		"",
		"the server rejected the request: 500 internal error",
	}
	for _, out := range cases {
		if IsUsageMismatch(out) {
			t.Errorf("IsUsageMismatch(%q) = true, want false (operational, not grammar)", out)
		}
	}
}
