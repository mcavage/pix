package sys

import "strings"

// ShellQuote quotes s for safe copy-paste into a POSIX shell: a token made
// only of clearly-safe characters passes through untouched; anything else is
// single-quoted using the standard close-escape-reopen sequence for embedded
// apostrophes, so a DIR like `my repo's` round-trips exactly.
func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.' || r == '/' || r == ':' || r == '@' || r == '%' || r == '+' || r == ',' || r == '=':
		default:
			safe = false
		}
		if !safe {
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
