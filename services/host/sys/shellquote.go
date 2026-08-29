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

// sanitizeControlBytes replaces every ASCII control byte (0x00-0x1f, 0x7f)
// in s with a visible backslash-escape placeholder: `\n`/`\r`/`\t` for the
// three common ones, `\xHH` for anything else. Each placeholder is plain
// text — a single-quoted shell string gives backslash no special meaning
// at all — so ShellQuote's output can never contain a literal line break
// or other control byte, no matter what was in the original name/path.
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
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
