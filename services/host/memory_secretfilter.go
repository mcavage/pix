// memory_secretfilter.go — a conservative, CAPTURE-ONLY secret filter, run at
// two points in memCapture (memory.go): before the watcher ever sees the
// user's message, and again before any extracted fact/correction is stored.
// Fail CLOSED (drop entirely, never redact-and-store); never logs the
// matched text, only a count. Does not apply to explicit `remember`.
//
// Best-effort, not a guarantee: known secret SHAPES only (private key
// blocks, vendor token prefixes, a JWT, a labeled assignment, an unbroken
// high-entropy run). docs/memory.md says so.
package main

import "regexp"

// memSecretPatterns: high-confidence shapes. The labeled-assignment pattern
// anchors on non-letter (not \b) so it still matches a SCREAMING_SNAKE_CASE
// env var glued to the keyword by underscores (AWS_SECRET_ACCESS_KEY=),
// which \b misses (underscore is a word char); up to 3 more "_word"
// segments may precede the assignment operator for the same reason.
var memSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),                                              // AWS access key id
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`),                                    // GitHub tokens
	regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`),                                  // Slack tokens
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`),                                         // OpenAI-shaped key
	regexp.MustCompile(`\bsk_(live|test)_[A-Za-z0-9]{16,}\b`),                               // Stripe secret key
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`),                                         // Google API key
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`), // JWT
	regexp.MustCompile(`(?i)(^|[^A-Za-z])(api[_-]?key|secret|token|password|passwd|bearer)(?:[_-][A-Za-z]+){0,3}\s*[:=]\s*['"]?[A-Za-z0-9/_+.=-]{12,}`),
}

// memHighEntropyRun: a long unbroken alnum run (32+) — the shape of a pasted
// token/hash even with no known prefix. 32 is long enough that ordinary
// prose essentially never produces one by accident.
var memHighEntropyRun = regexp.MustCompile(`\b[A-Za-z0-9]{32,}\b`)

// containsSecretShape reports whether text is secret-shaped. Never logs
// text; callers must not log it either.
func containsSecretShape(text string) bool {
	for _, re := range memSecretPatterns {
		if re.MatchString(text) {
			return true
		}
	}
	for _, run := range memHighEntropyRun.FindAllString(text, -1) {
		if hasLetterAndDigit(run) && !isAllHex(run) {
			return true
		}
	}
	return false
}

// hasDigitRe/hasLetterRe exclude an all-letter or all-digit run (a stretched
// word or padded number) from the high-entropy heuristic. allHexRe excludes
// an all-hex run (a git SHA, a sha256 digest — ordinary things to remember):
// it's indistinguishable from a hash by shape alone, so flagging it is a
// false positive this filter cannot avoid otherwise; a real secret that
// happens to be all-hex is a false negative this filter accepts.
var (
	hasDigitRe  = regexp.MustCompile(`[0-9]`)
	hasLetterRe = regexp.MustCompile(`[A-Za-z]`)
	allHexRe    = regexp.MustCompile(`^[0-9a-fA-F]+$`)
)

func hasLetterAndDigit(s string) bool { return hasLetterRe.MatchString(s) && hasDigitRe.MatchString(s) }
func isAllHex(s string) bool          { return allHexRe.MatchString(s) }
