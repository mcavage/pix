package sys

import (
	"os/exec"
	"strings"
	"testing"
)

// shellquote_test.go — 811dbde9 post-merge BLOCK fix: ShellQuote itself had
// no direct test at all before this file, only indirect coverage through
// workflow/env's rendered-error assertions (which never happened to pass a
// value ShellQuote had to actually escape anything more hostile than a
// space or `&`). Every case here is red-first against the pre-fix
// ShellQuote, which passed a raw embedded newline straight through inside
// its single quotes unmodified.

// realShellRoundTrip hands quoted to a REAL `sh -c` (via `set --`) and
// reads back argv[1] exactly as a shell would expand it — the same
// technique cmd/pix/env_cmd_test.go's shellTokenize already uses to prove a
// retry command is genuinely shell-quoted, not merely printf-formatted to
// look that way.
func realShellRoundTrip(t *testing.T, quoted string) string {
	t.Helper()
	script := "set -- " + quoted + "\nprintf '%s' \"$1\""
	out, err := exec.Command("sh", "-c", script).Output()
	if err != nil {
		t.Fatalf("sh -c tokenize %q: %v", quoted, err)
	}
	return string(out)
}

// TestShellQuote_SafeTokensPassThroughBare pins "safe registered names
// remain visually bare" (811dbde9's review BLOCK note): a plain name never
// grows quotes nobody asked for.
func TestShellQuote_SafeTokensPassThroughBare(t *testing.T) {
	for _, s := range []string{"work", "home-env", "a.b_c:1", "/data/envs/home", "v1.2,3+4=5%6@7"} {
		if got := ShellQuote(s); got != s {
			t.Errorf("ShellQuote(%q) = %q, want it unchanged (a clearly-safe token)", s, got)
		}
	}
}

// TestShellQuote_EmptyStringIsQuotedEmpty pins the zero-length case: `”`,
// never a bare zero-byte argument a naive caller could lose track of.
func TestShellQuote_EmptyStringIsQuotedEmpty(t *testing.T) {
	if got := ShellQuote(""); got != "''" {
		t.Errorf("ShellQuote(\"\") = %q, want \"''\"", got)
	}
}

// TestShellQuote_InjectionRoundTrip is the family's core proof: for every
// classic shell-injection shape (a space, a `$()` command substitution, a
// semicolon command separator, a single quote, a backtick, an ampersand),
// ShellQuote's output, handed to a REAL shell, reproduces the ORIGINAL
// string byte-for-byte as ONE argument — never split into two arguments,
// never executed as a second command.
func TestShellQuote_InjectionRoundTrip(t *testing.T) {
	cases := []string{
		"needs quoting & stuff",
		"work; rm -rf /",
		"$(rm -rf /)",
		"`rm -rf /`",
		"work && echo pwned",
		"it's a trap",
		"name with space",
		"pipe | to | somewhere",
		"redirect > /etc/passwd",
		"glob*star?",
		"tilde~expansion",
		"hash#comment",
		"bang!history",
	}
	for _, s := range cases {
		quoted := ShellQuote(s)
		if strings.ContainsAny(quoted, "\n\r") {
			t.Errorf("ShellQuote(%q) = %q, must never contain a raw control byte", s, quoted)
		}
		got := realShellRoundTrip(t, quoted)
		if got != s {
			t.Errorf("ShellQuote(%q) = %q; real shell round-trip = %q, want %q", s, quoted, got, s)
		}
	}
}

// TestShellQuote_NewlineIsSanitizedNotPreserved is the newline-specific
// half of the BLOCK finding: a raw embedded newline is never printed, quoted
// or not — it is REJECTED from the output entirely (replaced with a visible,
// inert `\n` text placeholder) rather than preserved as a real line break
// that would split a copy-pasted command in two.
func TestShellQuote_NewlineIsSanitizedNotPreserved(t *testing.T) {
	s := "work\nrm -rf /"
	quoted := ShellQuote(s)
	if strings.ContainsAny(quoted, "\n\r") {
		t.Fatalf("ShellQuote(%q) = %q, contains a raw newline/CR byte; want it sanitized to visible text", s, quoted)
	}
	if !strings.Contains(quoted, `\n`) {
		t.Errorf("ShellQuote(%q) = %q, want the sanitized `\\n` placeholder somewhere in the output", s, quoted)
	}
	// The whole point: a single line, so a real shell only ever sees ONE
	// command here, never two — the second half of the original string is
	// inert literal text inside the quotes, not a second statement.
	if lines := strings.Split(quoted, "\n"); len(lines) != 1 {
		t.Errorf("ShellQuote(%q) = %q, want exactly one output line, got %d", s, quoted, len(lines))
	}
	got := realShellRoundTrip(t, quoted)
	if strings.Contains(got, "\n") {
		t.Errorf("real shell round-trip of %q = %q, must never reintroduce a real newline", quoted, got)
	}
}

// TestShellQuote_OtherControlBytesAreSanitized covers carriage return, tab,
// and an arbitrary low control byte alongside the newline case above.
func TestShellQuote_OtherControlBytesAreSanitized(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // substring the sanitized+quoted output must contain
	}{
		{"carriage return", "work\rmore", `\r`},
		{"tab", "work\tmore", `\t`},
		{"bell", "work\x07more", `\x07`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			quoted := ShellQuote(c.in)
			if strings.ContainsAny(quoted, "\r\t\x07") {
				t.Errorf("ShellQuote(%q) = %q, must not contain the raw control byte", c.in, quoted)
			}
			if !strings.Contains(quoted, c.want) {
				t.Errorf("ShellQuote(%q) = %q, want it to contain %q", c.in, quoted, c.want)
			}
		})
	}
}

// TestShellQuote_SingleQuoteEscaping pins the close-escape-reopen shape for
// a literal single quote, the one character a single-quoted string cannot
// represent directly.
func TestShellQuote_SingleQuoteEscaping(t *testing.T) {
	s := "it's a trap"
	want := `'it'\''s a trap'`
	if got := ShellQuote(s); got != want {
		t.Errorf("ShellQuote(%q) = %q, want %q", s, got, want)
	}
}
