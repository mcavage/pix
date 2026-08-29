package sys

import (
	"fmt"
	"strings"
)

// ShellQuote quotes s for safe copy-paste into a POSIX shell: a clearly-safe
// token passes through untouched (so a plain registered name like "work"
// stays visually bare); anything else is single-quoted
// (close-escape-reopen), which defeats every shell metacharacter a single
// quote can contain (spaces, `$()`, backticks, `;`, `|`, `&`, ...) — the
// ONLY thing a POSIX single-quoted string cannot itself represent is
// another single quote, handled by closing, an escaped literal quote, then
// reopening.
//
// A newline (or any other ASCII control byte) is never passed through
// raw, quoted or not: sanitizeControlBytes replaces each one with a
// visible, inert backslash-escape placeholder first. This is deliberate
// lossy sanitization, not an attempt at byte-exact round-tripping — a
// single quote preserves an embedded newline exactly as a real line
// break, which would split a printed "one line, safe to copy-paste"
// command into two: pasted into an interactive shell, the first line
// would execute as its own command, newline and all, before the second
// line is even read. Quoting alone cannot fix that; only refusing to ever
// print the raw byte can.
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
	sanitized := sanitizeControlBytes(s)
	return "'" + strings.ReplaceAll(sanitized, "'", `'\''`) + "'"
}

// sanitizeControlBytes replaces every terminal control in s with a
// visible backslash-escape placeholder: `\n`/`\r`/`\t` for the three
// common C0 controls, `\xHH` for the rest of C0 and DEL, and `\uXXXX` for
// the C1 range (U+0080–U+009F — U+009B is a ONE-RUNE CSI on terminals
// honoring 8-bit controls, so C0-only sanitization is not enough) and the
// Unicode bidi controls (U+061C, U+200E/U+200F, U+202A–U+202E,
// U+2066–U+2069), which visually reorder the very text a human reads.
// Each placeholder is plain text — a single-quoted shell string gives
// backslash no special meaning at all — so neither ShellQuote's output nor
// TerminalSafe's can ever contain a literal line break, escape sequence,
// or reordering control, no matter what was in the original name/path.
func sanitizeControlBytes(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r >= 0x80 && r <= 0x9f, isBidiControl(r):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isBidiControl reports whether r is one of the Unicode bidirectional
// control characters relevant to terminal display: the explicit
// embedding/override pairs and PDF (U+202A–U+202E), the isolates
// (U+2066–U+2069), the marks (U+200E/U+200F), and the Arabic letter mark
// (U+061C).
func isBidiControl(r rune) bool {
	switch {
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return true
	case r == 0x200e || r == 0x200f || r == 0x061c:
		return true
	}
	return false
}
