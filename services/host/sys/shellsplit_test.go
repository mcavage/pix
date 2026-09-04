package sys

import (
	"errors"
	"reflect"
	"testing"
)

// shellsplit_test.go — E1.12 pre-merge BLOCK fix: ShellSplit replaces
// workflow/env/edit.go's old strings.Fields() editor-argv parsing with a
// small, independently tested, shell-quote-aware tokenizer that never
// invokes a shell. Every case here is red-first: it failed to compile
// before shellsplit.go existed, and each assertion pins one concrete
// quoting/escaping/failure shape a naive whitespace split gets wrong.

func TestShellSplit_Table(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    []string
		wantErr error // non-nil: want an error satisfying errors.Is(err, wantErr)
	}{
		{
			name: "plain whitespace, no quoting",
			in:   "code --wait",
			want: []string{"code", "--wait"},
		},
		{
			name: "multiple spaces collapse like a shell would",
			in:   "code   --wait   --flag",
			want: []string{"code", "--wait", "--flag"},
		},
		{
			name: "double-quoted executable path with spaces",
			in:   `"/Applications/My Editor.app/Contents/MacOS/edit" --wait`,
			want: []string{"/Applications/My Editor.app/Contents/MacOS/edit", "--wait"},
		},
		{
			name: "single-quoted executable path with spaces",
			in:   `'/usr/local/my editor' --wait`,
			want: []string{"/usr/local/my editor", "--wait"},
		},
		{
			name: "double-quoted argument containing spaces",
			in:   `editor --message="hello world" --wait`,
			want: []string{"editor", "--message=hello world", "--wait"},
		},
		{
			name: "single-quoted argument, no escape processing inside",
			in:   `editor --literal='a\nb $HOME'`,
			want: []string{"editor", `--literal=a\nb $HOME`},
		},
		{
			name: "backslash-escaped space outside any quote",
			in:   `editor foo\ bar`,
			want: []string{"editor", "foo bar"},
		},
		{
			name: "backslash-escaped quote inside double quotes",
			in:   `editor "say \"hi\""`,
			want: []string{"editor", `say "hi"`},
		},
		{
			name: "adjacent quoted and unquoted runs merge into one token",
			in:   `editor foo"bar baz"qux`,
			want: []string{"editor", "foobar bazqux"},
		},
		{
			name:    "unmatched double quote is rejected",
			in:      `editor "unterminated`,
			wantErr: ErrUnmatchedQuote,
		},
		{
			name:    "unmatched single quote is rejected",
			in:      `editor 'unterminated`,
			wantErr: ErrUnmatchedQuote,
		},
		{
			name:    "trailing bare backslash is rejected",
			in:      `editor foo\`,
			wantErr: ErrUnmatchedQuote,
		},
		{
			name:    "empty quoted command is rejected",
			in:      `""`,
			wantErr: ErrEmptyCommand,
		},
		{
			name:    "whitespace-only input is rejected",
			in:      "   ",
			wantErr: ErrEmptyCommand,
		},
		{
			name:    "empty input is rejected",
			in:      "",
			wantErr: ErrEmptyCommand,
		},
		// --- POSIX double-quote backslash semantics ---------------------
		// Inside double quotes a backslash removes quoting ONLY when it
		// precedes ", \, $, or a backtick (or a newline, as a line
		// continuation): those four/five are the only runes a shell lets
		// you escape inside "...". Before any other rune the backslash is
		// NOT an escape — it is preserved literally, and the following
		// rune is also preserved. A naive "backslash escapes anything"
		// implementation (the pre-fix behavior here) eats backslashes it
		// has no business touching: regex character classes, Windows
		// paths, anything with backslash sequences that aren't shell
		// escapes.
		{
			name: "backslash before an ordinary rune inside double quotes stays literal (regex)",
			in:   `editor "s/\s+/ /g"`,
			want: []string{"editor", `s/\s+/ /g`},
		},
		{
			name: "Windows-style backslash path inside double quotes is untouched",
			in:   `editor "C:\path\to\editor.exe" --wait`,
			want: []string{"editor", `C:\path\to\editor.exe`, "--wait"},
		},
		{
			// \" -> ", \\ -> \, \$ -> $, \` -> ` (the four POSIX-recognized
			// double-quote escapes), each consuming its backslash.
			name: "escaped quote, backslash, dollar, and backtick inside double quotes are unescaped",
			in:   "editor \"" + "\\\"" + "\\\\" + "\\$" + "\\`" + "x\"",
			want: []string{"editor", "\"\\$`x"},
		},
		{
			// A trailing \\ right before the closing quote is an escaped
			// backslash (-> one literal \), not an escape of the quote: the
			// quote still closes normally afterward.
			name: "escaped backslash immediately before the closing double quote",
			in:   "editor \"path\\\\\"",
			want: []string{"editor", "path\\"},
		},
		{
			// A single trailing backslash right before the closing quote
			// instead escapes the QUOTE (not one of the four specials'
			// siblings, but " itself is), so the quote never closes: the
			// whole command line is an unterminated double quote.
			name:    "trailing backslash escaping the closing double quote leaves it unterminated",
			in:      `editor "path\"`,
			wantErr: ErrUnmatchedQuote,
		},
		// --- POSIX line continuation outside quotes ---------------------
		// Outside any quote, a backslash immediately followed by a newline
		// is a line continuation: BOTH characters vanish and the two lines
		// join with no separator — it is not a literal newline embedded in
		// the token (that would be the pre-fix, wrong behavior: the
		// existing outside-quote backslash rule treats \<newline> like any
		// other bare escape and keeps the newline as token text).
		{
			name: "line continuation within a token",
			in:   "editor fo\\\no --wait",
			want: []string{"editor", "foo", "--wait"},
		},
		{
			name: "line continuation between tokens (joins two words with no space)",
			in:   "editor foo\\\nbar --wait",
			want: []string{"editor", "foobar", "--wait"},
		},
		{
			name: "line continuation before an argument, whitespace on both sides",
			in:   "editor \\\n  --wait",
			want: []string{"editor", "--wait"},
		},
		{
			name: "line continuation at end of input leaves no trailing artifact",
			in:   "editor --wait\\\n",
			want: []string{"editor", "--wait"},
		},
		{
			name:    "trailing standalone backslash with no newline is still unmatched",
			in:      `editor --wait\`,
			wantErr: ErrUnmatchedQuote,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ShellSplit(tc.in)
			if tc.wantErr != nil {
				if err == nil {
					t.Fatalf("ShellSplit(%q) = %v, nil, want an error", tc.in, got)
				}
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ShellSplit(%q) error = %v, want it to wrap %v", tc.in, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ShellSplit(%q) unexpected error: %v", tc.in, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ShellSplit(%q) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}

// TestShellSplit_NeverInvokesAShell is documentation-as-test: ShellSplit's
// signature is a pure string -> ([]string, error) function with no exec.Cmd
// anywhere in its call graph, so a shell metacharacter that would matter to
// /bin/sh (a bare `;`, `|`, `$(...)`, backtick) is just ordinary token text
// here — never interpreted, never executed.
func TestShellSplit_ShellMetacharactersAreInertText(t *testing.T) {
	got, err := ShellSplit(`editor --title=$(whoami)`)
	if err != nil {
		t.Fatalf("ShellSplit: %v", err)
	}
	want := []string{"editor", "--title=$(whoami)"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ShellSplit(...) = %#v, want %#v (command substitution left as literal text)", got, want)
	}
}

// TestShellSplit_MetacharactersInertInsideDoubleQuotes pins the same
// no-expansion guarantee as TestShellSplit_ShellMetacharactersAreInertText,
// but for text sitting inside double quotes: a shell would still leave `;`
// and `|` alone there (only $, `, ", and \ are special), and unescaping the
// backslash in front of $( must not make ShellSplit start interpreting it.
func TestShellSplit_MetacharactersInertInsideDoubleQuotes(t *testing.T) {
	got, err := ShellSplit(`editor "a;b|c\$(whoami)"`)
	if err != nil {
		t.Fatalf("ShellSplit: %v", err)
	}
	want := []string{"editor", "a;b|c$(whoami)"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ShellSplit(...) = %#v, want %#v (metacharacters and unescaped $ stay literal text)", got, want)
	}
}
