package sys

// terminalsafe_test.go — Wave C security M1's red-first proof for
// TerminalSafe, the display-text twin of ShellQuote's control sanitizer.
// A string rendered to a terminal (the env trust bill's names, argv,
// credential refs, mounts, digests — all authored file content, i.e.
// attacker-controlled for a cloned/shared environment) must never carry a
// raw terminal control: an ESC-led CSI/OSC sequence can recolor, retitle,
// or overwrite the consent screen; a raw newline can forge whole renderer
// lines (a fake count line, a fake "[y/N]" prompt); C1 controls (U+0080–
// U+009F, e.g. the one-byte CSI U+009B) do the same without ESC; and the
// Unicode bidi controls visually reorder what the human reads. TerminalSafe
// replaces every one with a visible, inert backslash escape while leaving
// normal Unicode (accents, CJK, emoji) byte-identical.

import (
	"strings"
	"testing"
)

// TestTerminalSafe_NormalUnicodePreserved pins the "display text" half:
// ordinary printable Unicode passes through byte-identical — no quoting,
// no escaping, no normalization.
func TestTerminalSafe_NormalUnicodePreserved(t *testing.T) {
	for _, s := range []string{
		"",
		"work",
		"héllo wörld",
		"日本語テキスト",
		"emoji 🚀 ok",
		"op://Personal/Anthropic/api-key -> api.anthropic.com",
		"path/with spaces and 'quotes' and $(dollar)",
	} {
		if got := TerminalSafe(s); got != s {
			t.Errorf("TerminalSafe(%q) = %q, want it byte-identical (normal text, display-only helper)", s, got)
		}
	}
}

// TestTerminalSafe_C0DELAndEscapeSequences proves every ASCII C0 control
// and DEL is replaced with its visible placeholder — including the ESC
// that leads CSI (`ESC [`) and OSC (`ESC ]`) sequences, the classic
// terminal-injection vectors.
func TestTerminalSafe_C0DELAndEscapeSequences(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // placeholder the output must contain
		raw  string // raw byte(s) the output must NOT contain
	}{
		{"newline", "a\nb", `\n`, "\n"},
		{"carriage return", "a\rb", `\r`, "\r"},
		{"tab", "a\tb", `\t`, "\t"},
		{"CSI color", "a\x1b[31mred", `\x1b`, "\x1b"},
		{"OSC title", "a\x1b]0;pwn\x07b", `\x1b`, "\x1b"},
		{"bell", "a\x07b", `\x07`, "\x07"},
		{"NUL", "a\x00b", `\x00`, "\x00"},
		{"DEL", "a\x7fb", `\x7f`, "\x7f"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := TerminalSafe(c.in)
			if strings.Contains(got, c.raw) {
				t.Errorf("TerminalSafe(%q) = %q, must not contain the raw control", c.in, got)
			}
			if !strings.Contains(got, c.want) {
				t.Errorf("TerminalSafe(%q) = %q, want the visible placeholder %q", c.in, got, c.want)
			}
		})
	}
}

// TestTerminalSafe_C1Controls proves the C1 range (U+0080–U+009F) is
// escaped too: U+009B is a ONE-RUNE CSI — a terminal that honors 8-bit
// controls starts a control sequence from it with no ESC anywhere in the
// string, so C0-only sanitization is not enough.
func TestTerminalSafe_C1Controls(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"a\u009bb", `\u009b`}, // one-rune CSI
		{"a\u0085b", `\u0085`}, // NEL: a line break outside C0
		{"a\u0090b", `\u0090`}, // DCS
		{"a\u009db", `\u009d`}, // OSC
	}
	for _, c := range cases {
		got := TerminalSafe(c.in)
		if strings.ContainsRune(got, []rune(c.in)[1]) {
			t.Errorf("TerminalSafe(%q) = %q, must not contain the raw C1 control", c.in, got)
		}
		if !strings.Contains(got, c.want) {
			t.Errorf("TerminalSafe(%q) = %q, want the visible placeholder %q", c.in, got, c.want)
		}
	}
}

// TestTerminalSafe_BidiControls proves the terminal-relevant Unicode bidi
// controls are escaped visibly: RLO (U+202E) and friends visually reorder
// the very text a human is consenting to (the classic "gpj.exe" attack,
// here aimed at a credential destination or an argv).
func TestTerminalSafe_BidiControls(t *testing.T) {
	bidi := []rune{'\u061c', '\u200e', '\u200f', '\u202a', '\u202b', '\u202c', '\u202d', '\u202e', '\u2066', '\u2067', '\u2068', '\u2069'}
	for _, r := range bidi {
		in := "a" + string(r) + "b"
		got := TerminalSafe(in)
		if strings.ContainsRune(got, r) {
			t.Errorf("TerminalSafe(%q) = %q, must not contain the raw bidi control %U", in, got, r)
		}
		if !strings.Contains(got, `\u`) {
			t.Errorf("TerminalSafe(%q) = %q, want a visible \\uXXXX placeholder for %U", in, got, r)
		}
	}
}

// TestShellQuote_C1AndBidiAreSanitizedToo pins that ShellQuote (which
// shares the one control sanitizer TerminalSafe reuses) no longer passes a
// C1 control or a bidi control through raw inside its quotes either.
func TestShellQuote_C1AndBidiAreSanitizedToo(t *testing.T) {
	for _, c := range []struct {
		in   string
		raw  rune
		want string
	}{
		{"work\u009bmore", '\u009b', `\u009b`},
		{"work\u202emore", '\u202e', `\u202e`},
	} {
		quoted := ShellQuote(c.in)
		if strings.ContainsRune(quoted, c.raw) {
			t.Errorf("ShellQuote(%q) = %q, must not contain the raw control %U", c.in, quoted, c.raw)
		}
		if !strings.Contains(quoted, c.want) {
			t.Errorf("ShellQuote(%q) = %q, want the visible placeholder %q", c.in, quoted, c.want)
		}
	}
}
