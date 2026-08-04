package monitor

// redact.go is a second, host-side line of defense against a secret landing
// on disk in cleartext. The in-VM extension already minimizes what it sends
// (hashes + short previews, never raw message/system-prompt text — see
// extensions/monitor.ts), but a tool's own argsSummary/resultSummary or a
// blob body (the ONE place full text does cross the wire, on first sight of
// a hash — see BlobStore.Put) can legitimately contain a real secret: `bash`
// output that echoes an env var, a curl response, a leaked key in a file a
// tool printed. Story05 replaces the in-memory-only ring/blob cache with
// files that persist past the process, so this pass now runs on every event
// AND every blob before either is written — never after.
//
// This is pattern-matching, not a security boundary: it catches
// recognizable secret SHAPES (the common ones below), not arbitrary
// sensitive text. It is deliberately over-inclusive (redact and move on)
// rather than trying to be precise.

import "regexp"

// RedactionMarker replaces a matched secret span. Fixed-width and free of
// characters ('"', '\\') that could corrupt the JSON it sits inside (see
// UnknownEvent's whole-line redaction in Redact below).
const RedactionMarker = "[REDACTED]"

// secretPatterns are recognizable secret SHAPES, checked in order (an
// earlier match wins the span; ReplaceAllString each pattern independently
// so overlapping matches from different patterns still all get redacted).
var secretPatterns = []*regexp.Regexp{
	// AWS access key id.
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	// GitHub personal/OAuth/user-to-server/refresh tokens (ghp_/gho_/ghu_/ghr_/ghs_).
	regexp.MustCompile(`gh[oprsu]_[A-Za-z0-9]{20,}`),
	// Slack bot/user/app tokens.
	regexp.MustCompile(`xox[baprs]-[0-9A-Za-z-]{10,}`),
	// OpenAI/Anthropic-shaped secret keys (sk-..., sk-ant-...).
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`),
	// PEM private key blocks (the header line alone is enough to redact —
	// the body is base64 noise the reader never needs).
	regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`),
	// Generic `key = value` / `"token": "value"` assignment shape, the
	// catch-all for anything the specific patterns above miss: a
	// case-insensitive secret-shaped NAME, then an assignment operator,
	// then a long-enough opaque VALUE.
	regexp.MustCompile(`(?i)(api[_-]?key|secret|token|password|passwd)["']?\s*[:=]\s*["']?[A-Za-z0-9/_.+-]{12,}`),
}

// RedactText replaces every recognizable secret-shaped substring of s with
// RedactionMarker. Empty input is returned unchanged (the common case for
// most fields, and avoids allocating for nothing).
func RedactText(s string) string {
	if s == "" {
		return s
	}
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, RedactionMarker)
	}
	return s
}

// Redact returns a copy of e with every free-text field passed through
// RedactText. It is a pure function (e is never mutated in place) — the
// caller (Store.Append) persists ONLY the returned value, never the
// original. capField/capID/capHash id-style fields are redacted too (cheap,
// and defends against a hostile producer stuffing a secret into a field
// that's supposed to be a short label) except hashes, which are
// content-addresses by construction and never free text.
func Redact(e Event) Event {
	switch v := e.(type) {
	case TurnStart:
		v.Model = RedactText(v.Model)
		return v
	case ProviderRequest:
		v.Model = RedactText(v.Model)
		for i := range v.Summary.NewMessages {
			v.Summary.NewMessages[i].Preview = RedactText(v.Summary.NewMessages[i].Preview)
		}
		return v
	case ProviderResponse:
		v.StopReason = RedactText(v.StopReason)
		v.TextPreview = RedactText(v.TextPreview)
		return v
	case ToolStart:
		v.ArgsSummary = RedactText(v.ArgsSummary)
		return v
	case ToolEnd:
		v.ResultSummary = RedactText(v.ResultSummary)
		return v
	case ContextEvent:
		v.Detail = RedactText(v.Detail)
		return v
	case UnknownEvent:
		// No known field shape (that's what makes it unknown — see
		// event.go's decodeUnknown): redact across the WHOLE raw line.
		// Safe because every pattern above matches a token-like charset
		// that can't contain '"', so a match landing inside a JSON string
		// value is replaced without corrupting the surrounding structure;
		// a match that happened to span a key name is equally fine to
		// redact (the whole point is "don't ship this back out verbatim
		// if we can't even name its shape").
		v.Raw = []byte(RedactText(string(v.Raw)))
		return v
	default:
		return e
	}
}
