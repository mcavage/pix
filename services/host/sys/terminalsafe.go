package sys

// TerminalSafe returns s as terminal-safe DISPLAY text: every raw terminal
// control — ASCII C0 (including ESC, the CSI/OSC lead-in, and newline/CR,
// the line-forgery primitives), DEL, the C1 range U+0080–U+009F (U+009B is
// a one-rune CSI on terminals honoring 8-bit controls), and the Unicode
// bidi controls that visually reorder rendered text — is replaced with a
// visible, inert backslash escape (sanitizeControlBytes, the SAME
// sanitizer ShellQuote already runs). Normal printable Unicode (accents,
// CJK, emoji) passes through byte-identical, and nothing is quoted: this
// is ShellQuote's display-text sibling for values that are READ by a
// human, not pasted into a shell.
//
// Deliberately lossy and display-only: canonical bytes (a fingerprinted
// bill of materials, a hashed document) must never round-trip through it —
// sanitize at the render boundary, never at the compute boundary.
func TerminalSafe(s string) string {
	return sanitizeControlBytes(s)
}
