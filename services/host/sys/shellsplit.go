package sys

import (
	"errors"
	"fmt"
	"strings"
)

// ErrUnmatchedQuote is ShellSplit's refusal for an unterminated single or
// double quote, or a bare trailing backslash with nothing left to escape.
var ErrUnmatchedQuote = errors.New("unmatched quote or trailing escape")

// ErrEmptyCommand is ShellSplit's refusal when the input tokenizes to zero
// argv entries, or to a single empty argv[0] (`""` or `”` alone) — never a
// program to exec either way.
var ErrEmptyCommand = errors.New("empty command")

// ShellSplit tokenizes s the way a POSIX shell would split ONE command
// line into argv — single quotes ('...', fully literal, no escapes
// recognized inside), double quotes ("...", where a backslash escapes the
// next character), and a bare backslash outside any quote escaping the
// next character (so a space can be embedded in a token without quoting
// it) — WITHOUT ever constructing or invoking a shell. It performs no
// globbing, no variable expansion, no command substitution: a shell
// metacharacter that appears outside quotes is ordinary token text.
// Adjacent quoted and unquoted runs with no separating whitespace merge
// into a single token, exactly as they would in a real shell
// (`foo"bar baz"qux` -> one token `foo` + `bar baz` + `qux`).
//
// This exists so workflow/env's editor invocation ($VISUAL/$EDITOR) can
// accept a quoted executable path containing spaces ("code --wait" would
// otherwise split the editor's own binary path in two) while still handing
// the result to sys.RunInteractive as a real argv — argv[0] plus its own
// flags — never through a shell -c string.
//
// An unterminated quote or a trailing bare backslash is ErrUnmatchedQuote:
// fail closed rather than guess where the token was meant to end. A result
// that tokenizes to nothing runnable — no tokens at all, or a lone empty
// first token (`""`, `”`) — is ErrEmptyCommand.
func ShellSplit(s string) ([]string, error) {
	var argv []string
	var cur strings.Builder
	haveToken := false
	inSingle, inDouble, escaped := false, false, false

	flush := func() {
		if haveToken {
			argv = append(argv, cur.String())
			cur.Reset()
			haveToken = false
		}
	}

	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case inSingle:
			if r == '\'' {
				inSingle = false
			} else {
				cur.WriteRune(r)
			}
		case inDouble:
			switch r {
			case '"':
				inDouble = false
			case '\\':
				escaped = true
			default:
				cur.WriteRune(r)
			}
		case r == '\\':
			escaped = true
			haveToken = true
		case r == '\'':
			inSingle = true
			haveToken = true
		case r == '"':
			inDouble = true
			haveToken = true
		case r == ' ' || r == '\t' || r == '\n':
			flush()
		default:
			cur.WriteRune(r)
			haveToken = true
		}
	}

	if escaped || inSingle || inDouble {
		return nil, fmt.Errorf("%w: %q", ErrUnmatchedQuote, s)
	}
	flush()

	if len(argv) == 0 || argv[0] == "" {
		return nil, fmt.Errorf("%w: %q", ErrEmptyCommand, s)
	}
	return argv, nil
}
