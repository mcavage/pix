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
