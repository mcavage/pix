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
// recognized inside), double quotes ("...", where a backslash removes
// quoting ONLY for the next ", \, $, backtick, or newline — before any
// other rune the backslash is preserved literally, exactly as POSIX
// specifies), and a bare backslash outside any quote escaping the next
// character outright (so a space can be embedded in a token without
// quoting it) — WITHOUT ever constructing or invoking a shell. It
// performs no globbing, no variable expansion, no command substitution: a
// shell metacharacter that appears outside quotes (or an unescaped one
// inside double quotes) is ordinary token text. Adjacent quoted and
// unquoted runs with no separating whitespace merge into a single token,
// exactly as they would in a real shell (`foo"bar baz"qux` -> one token
// `foo` + `bar baz` + `qux`).
//
// The double-quote/backslash distinction matters in practice: a naive
// "backslash escapes anything" tokenizer mangles Windows-style paths
// (`"C:\path\to\editor.exe"`) and regex arguments (`"s/\s+/ /g"`) by
// silently eating backslashes that were never a shell escape to begin
// with.
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
	runes := []rune(s)
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

	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case inSingle:
			if r == '\'' {
				inSingle = false
			} else {
				cur.WriteRune(r)
			}
		case inDouble:
			switch {
			case r == '"':
				inDouble = false
			case r == '\\' && i+1 < len(runes):
				// POSIX: inside "..." a backslash only loses its literal
				// meaning (and is dropped) when it precedes ", \, $, a
				// backtick, or a newline (line continuation, dropped
				// entirely). Before any other rune the backslash stays —
				// this is what keeps regexes (`\s+`) and Windows paths
				// (`C:\path`) intact.
				next := runes[i+1]
				switch next {
				case '"', '\\', '$', '`':
					cur.WriteRune(next)
					i++
				case '\n':
					i++ // line continuation: backslash + newline both dropped
				default:
					cur.WriteRune(r)
				}
			default:
				cur.WriteRune(r)
			}
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\':
			// Outside any quote a bare backslash escapes the very next
			// rune outright (dropped itself), letting e.g. a space be
			// embedded in a token without quoting it.
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
