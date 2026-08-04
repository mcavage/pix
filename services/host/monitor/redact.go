package monitor

// redact.go is the host-side line of defense against a secret landing on
// disk in cleartext. The tap already sends hashes and short previews, but a
// tool's argsSummary/resultSummary — or a blob body, the one place full text
// crosses the wire — can carry a real secret: bash echoing an env var, a
// curl response, a key in a file a tool printed. Everything this package
// writes is written redacted; nothing is redacted after the fact.
//
// Pattern matching, not a security boundary: recognizable secret SHAPES,
// deliberately over-inclusive, not arbitrary sensitive text.

import "regexp"

// redactionMarker replaces a matched secret span. Fixed-width and free of
// characters ('"', '\\') that could corrupt the JSON it sits inside.
const redactionMarker = "[REDACTED]"

// secretPatterns are recognizable secret shapes. Each is applied
// independently so overlapping matches from different patterns all land.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),             // AWS access key id
	regexp.MustCompile(`gh[oprsu]_[A-Za-z0-9]{20,}`),   // GitHub tokens
	regexp.MustCompile(`xox[baprs]-[0-9A-Za-z-]{10,}`), // Slack tokens
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`),        // OpenAI/Anthropic keys
	regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`),
	// Catch-all `key = value` / `"token": "value"` assignment shape: a
	// secret-shaped NAME, an assignment operator, then a long opaque VALUE.
	regexp.MustCompile(`(?i)(api[_-]?key|secret|token|password|passwd)["']?\s*[:=]\s*["']?[A-Za-z0-9/_.+-]{12,}`),
}

// redactText replaces every recognizable secret-shaped substring with
// redactionMarker.
func redactText(s string) string {
	if s == "" {
		return s
	}
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, redactionMarker)
	}
	return s
}

// redact returns a copy of e with every free-text field scrubbed (e is never
// mutated; Store.Append persists only the return value). Short label fields
// are scrubbed too — cheap, and defends against a hostile tap hiding a
// secret in one — but hashes are not, being content-addresses.
func redact(e Event) Event {
	switch v := e.(type) {
	case TurnStart:
		v.Model = redactText(v.Model)
		return v
	case ProviderRequest:
		v.Model = redactText(v.Model)
		for i := range v.Summary.NewMessages {
			v.Summary.NewMessages[i].Preview = redactText(v.Summary.NewMessages[i].Preview)
		}
		return v
	case ProviderResponse:
		v.StopReason = redactText(v.StopReason)
		v.TextPreview = redactText(v.TextPreview)
		return v
	case ToolStart:
		v.ArgsSummary = redactText(v.ArgsSummary)
		return v
	case ToolEnd:
		v.ResultSummary = redactText(v.ResultSummary)
		return v
	case ContextEvent:
		v.Detail = redactText(v.Detail)
		return v
	case UnknownEvent:
		// No known field shape, so redact the WHOLE raw line. Safe because
		// every pattern matches a token-like charset with no '"', so a match
		// inside a JSON string is replaced without corrupting the structure.
		v.Raw = []byte(redactText(string(v.Raw)))
		return v
	default:
		return e
	}
}
