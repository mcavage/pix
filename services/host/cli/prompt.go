package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"
	"strings"
)

// IO is the three things an interactive command needs: where input comes from,
// where output goes, and whether a human is watching. It lives here because
// every interactive verb needs the same triple, and because a workflow that
// takes an IO instead of reaching for os.Stdin is one a test can drive.
type IO struct {
	In    io.Reader
	Out   io.Writer
	IsTTY bool
}

// LineIn is the ONE way this codebase turns a stdin-ish reader into a
// line reader, and it exists to stop a real bug rather than to save three
// characters: a `bufio.NewScanner`/`bufio.NewReader` created per prompt
// pulls up to its whole buffer out of the underlying stream and then throws
// the remainder away when it goes out of scope. `pix setup` asks several
// questions from several different functions, so anything a user typed
// ahead of the current prompt — or simply the rest of a piped script —
// vanished between them. Reusing an already-buffered reader keeps one
// buffer for the whole command, so no prompt can eat the next prompt's
// answer.
func LineIn(in io.Reader) *bufio.Reader {
	if br, ok := in.(*bufio.Reader); ok {
		return br
	}
	return bufio.NewReader(in)
}

// Line is THIS command's single buffered stdin reader, created on first use
// and memoized on Deps. Every interactive step in one command must read
// through it (directly, or by handing it to a helper that takes an
// io.Reader, which LineIn then recognizes) so the command has exactly one
// stdin buffer.
func (d *Deps) Line() *bufio.Reader {
	if d.line == nil {
		in := d.In
		if in == nil {
			in = strings.NewReader("")
		}
		d.line = LineIn(in)
	}
	return d.line
}

// Question is one interactive value request: what to call it, what it is
// for, what it already is, and what counts as a valid answer. It is the
// shared shape every `pix setup` prompt uses, so a typo is correctable in
// place instead of aborting the step and sending the user off to run a
// separate command and start over.
type Question struct {
	// Label names the value, and is what the prompt line shows.
	Label string
	// Detail is one line of context printed once, before the first
	// attempt: what this value is for, where it comes from. Optional.
	Detail string
	// Example is a well-formed sample. It is shown when an answer is
	// rejected, where it is actually useful, not as decoration on the
	// first ask. Optional.
	Example string
	// Current is the value already recorded, if any. It is offered as the
	// default, so a bare Enter keeps it (and returns it) rather than
	// clearing it.
	Current string
	// Accept validates and, for a caller that persists as it goes, records
	// the answer. A returned error is shown to the user and the SAME
	// question is asked again — this is the whole point of the type.
	Accept func(string) error
	// Attempts bounds the retry loop. Zero means the default (3): enough
	// to fix a paste, never an unclosable loop on a pipe that keeps
	// answering wrong.
	Attempts int
}

// Ask puts q to the user and returns the accepted answer. It returns
// ok=false, having changed nothing, when there is no one to ask (a
// non-interactive command), when the user declines by answering blank, when
// stdin ends, and when the attempt budget is exhausted — a caller must
// treat every one of those as "still not recorded" and print its own
// non-interactive remedy, exactly as it would have with no prompt at all.
func (d *Deps) Ask(q Question) (string, bool) {
	if !d.Interactive || d.In == nil {
		return "", false
	}
	attempts := q.Attempts
	if attempts <= 0 {
		attempts = 3
	}
	if q.Detail != "" {
		fmt.Fprintf(d.Out, "  %s\n", q.Detail)
	}
	for attempt := 0; attempt < attempts; attempt++ {
		prompt := "  " + q.Label
		if q.Current != "" {
			prompt += " [" + q.Current + "]"
		}
		line, err := d.ReadLine(d.Out, prompt+": ")
		if err != nil && line == "" {
			return "", false
		}
		answer := strings.TrimSpace(line)
		if answer == "" && q.Current != "" {
			answer = q.Current
		}
		if answer == "" {
			return "", false
		}
		if q.Accept != nil {
			if aerr := q.Accept(answer); aerr != nil {
				fmt.Fprintf(d.Out, "    %v\n", aerr)
				if q.Example != "" {
					fmt.Fprintf(d.Out, "    example: %s\n", q.Example)
				}
				if err != nil {
					// The rejection was the last thing on a closed
					// stream: there is nothing left to re-read.
					return "", false
				}
				continue
			}
		}
		return answer, true
	}
	return "", false
}

// AskYN asks a yes/no question through the command's one shared stdin
// buffer. It is ConfirmYN's behaviour (blank answer takes def) without the
// per-call reader, so a "y" typed ahead of the next prompt is still there
// when that prompt arrives.
func (d *Deps) AskYN(prompt string, def bool) bool {
	if !d.Interactive || d.In == nil {
		return def
	}
	return d.Confirm(d.Out, prompt, def)
}

func (d *Deps) Confirm(out io.Writer, prompt string, def bool) bool {
	line, err := d.ReadLine(out, prompt)
	if err != nil {
		return false
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	if ans == "" {
		return def
	}
	return ans == "y" || ans == "yes"
}

// ReadLine uses a real line editor on terminals and one shared buffer on pipes.
// Restore cooked mode before handing the terminal to an authentication process.
func (d *Deps) ReadLine(out io.Writer, prompt string) (string, error) {
	if f, ok := d.In.(*os.File); ok && d.Interactive && term.IsTerminal(int(f.Fd())) {
		state, err := term.MakeRaw(int(f.Fd()))
		if err != nil {
			return "", err
		}
		defer term.Restore(int(f.Fd()), state)
		if d.terminal == nil {
			d.terminal = term.NewTerminal(struct {
				io.Reader
				io.Writer
			}{f, out}, prompt)
		} else {
			d.terminal.SetPrompt(prompt)
		}
		if width, height, err := term.GetSize(int(f.Fd())); err == nil && width > 0 && height > 0 {
			d.terminal.SetSize(width, height)
		}
		line, err := d.terminal.ReadLine()
		if errors.Is(err, io.EOF) {
			fmt.Fprint(out, "\r\n")
			panic(errPromptCanceled)
		}
		if errors.Is(err, term.ErrPasteIndicator) {
			err = nil
		}
		return line, err
	}
	fmt.Fprint(out, prompt)
	return d.Line().ReadString('\n')
}
