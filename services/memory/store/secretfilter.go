// secretfilter.go is a conservative, CAPTURE-ONLY secret filter (see
// capture.go). Fail CLOSED (drop entirely, never redact-and-store); never
// logs the matched text, only a count. Does not apply to explicit
// memory_remember. Ported verbatim from
// services/host/memory_secretfilter.go (pix-v2 U2).
package store

import "regexp"

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`),
	regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`),
	regexp.MustCompile(`\bsk_(live|test)_[A-Za-z0-9]{16,}\b`),
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`),
	regexp.MustCompile(`(?i)(^|[^A-Za-z])(api[_-]?key|secret|token|password|passwd|bearer)(?:[_-][A-Za-z]+){0,3}\s*[:=]\s*['"]?[A-Za-z0-9/_+.=-]{12,}`),
	regexp.MustCompile(`\bop://[A-Za-z0-9_.%-]+/[A-Za-z0-9_.%-]+/[A-Za-z0-9_.%-]+(?:/[A-Za-z0-9_.%-]+)?\b`),
}

var highEntropyRun = regexp.MustCompile(`\b[A-Za-z0-9]{32,}\b`)

// containsSecretShape reports whether text is secret-shaped. Never logs
// text; callers must not log it either.
func containsSecretShape(text string) bool {
	for _, re := range secretPatterns {
		if re.MatchString(text) {
			return true
		}
	}
	for _, run := range highEntropyRun.FindAllString(text, -1) {
		if hasLetterAndDigit(run) && !isAllHex(run) {
			return true
		}
	}
	return false
}

var (
	hasDigitRe  = regexp.MustCompile(`[0-9]`)
	hasLetterRe = regexp.MustCompile(`[A-Za-z]`)
	allHexRe    = regexp.MustCompile(`^[0-9a-fA-F]+$`)
)

func hasLetterAndDigit(s string) bool { return hasLetterRe.MatchString(s) && hasDigitRe.MatchString(s) }
func isAllHex(s string) bool          { return allHexRe.MatchString(s) }
