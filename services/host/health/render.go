package health

import (
	"fmt"
	"io"
	"strings"
)

// render.go holds the two renderings, and the difference between them is the
// product decision: `status` is a glance (short, no repair commands, always
// exit 0 — a script asking "what is up" must never fail because a probe could
// not see something), `doctor` is the diagnosis (every result, its evidence,
// and the exact command that fixes it).

// Glyph is the one-character presentation of a status.
// writeEvidence prints a row's evidence. A probe that reports on a SET (mcp: one
// note per server) produced a single run-on line hundreds of characters wide,
// which is unreadable in a terminal and unskimmable anywhere: the reader cannot
// tell how many items there are, which one is theirs, or where one note ends and
// the next begins. Semicolon-separated notes therefore get one line each, under
// a count. A single-item evidence string is unchanged, so every other row reads
// exactly as before.
func writeEvidence(w io.Writer, ev string) {
	parts := strings.Split(ev, "\n")
	if len(parts) < 3 {
		fmt.Fprintf(w, "    evidence: %s\n", ev)
		return
	}
	fmt.Fprintf(w, "    evidence: %d items\n", len(parts))
	for _, part := range parts {
		fmt.Fprintf(w, "      - %s\n", strings.TrimSpace(part))
	}
}

func Glyph(s Status) string {
	switch s {
	case StatusReady:
		return "✓"
	case StatusAbsent:
		return "✗"
	case StatusDenied:
		return "⊘"
	default:
		return "?"
	}
}

// RenderStatus writes the short form: one line naming what is ready, what is
// missing, and what could not be checked. No repair commands — that is
// doctor's job, and the caller always exits 0.
func RenderStatus(w io.Writer, s Snapshot) {
	if len(s.Results) == 0 {
		fmt.Fprintln(w, "pix: nothing to check")
		return
	}
	var ready, gaps, unknown []string
	for _, r := range s.Results {
		switch r.Effective() {
		case StatusReady:
			ready = append(ready, r.Name)
		case StatusUnknown:
			unknown = append(unknown, r.Name)
		default:
			gaps = append(gaps, r.Name)
		}
	}
	fmt.Fprintf(w, "%s %s\n", headlineGlyph(s), headline(s))
	if len(ready) > 0 {
		fmt.Fprintf(w, "  ready    %s\n", strings.Join(ready, " "))
	}
	if len(gaps) > 0 {
		fmt.Fprintf(w, "  missing  %s\n", strings.Join(gaps, " "))
	}
	if len(unknown) > 0 {
		fmt.Fprintf(w, "  unknown  %s\n", strings.Join(unknown, " "))
	}
	// The one pointer status is allowed to make: the count, and where the
	// commands live. Status names no repair itself — printing a fix here is
	// how two surfaces start disagreeing about the same gap.
	if n := len(gaps); n > 0 {
		word := "the exact fix commands"
		if n == 1 {
			word = "the exact fix command"
		}
		fmt.Fprintf(w, "  %s. Run `%s` for %s.\n", plural(n, "issue"), DoctorCommand, word)
	}
}

// DoctorCommand is where status sends a user with something to fix. It is a
// constant because status must never print a repair itself: one surface owns
// the commands, the other owns the glance.
const DoctorCommand = "pix doctor"

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// headlineGlyph is the headline's marker. It needs one distinction the four
// status glyphs cannot make: something is definitively BROKEN, and you can
// still work. A ✓ would be false (an integration proved itself unusable), a ✗
// reads as "nothing works" and would send someone debugging a healthy core, and
// a ? claims we could not find out when we did. ⚠ says both true things at
// once, and the exit code stays 0.
func headlineGlyph(s Snapshot) string {
	if len(s.Blocking()) == 0 && len(s.OptionalGaps()) > 0 {
		return "⚠"
	}
	return Glyph(headlineStatus(s))
}

// headlineStatus reduces a snapshot to the single status its headline claims:
// a verified required gap dominates, then a verified OPTIONAL gap, then
// anything unproven, then ready.
//
// An optional gap must not render as ✓. The glyph is the only part of this
// report some people read, and a ✓ over a row that proved an integration broken
// is the same lie — in one character — that this whole report was rebuilt to
// stop telling. It stays non-blocking for the exit code; it does not stay
// invisible.
func headlineStatus(s Snapshot) Status {
	if len(s.Blocking()) > 0 {
		return StatusAbsent
	}
	if len(s.OptionalGaps()) > 0 {
		return StatusUnknown
	}
	if s.Ready() {
		return StatusReady
	}
	return StatusUnknown
}

func headline(s Snapshot) string {
	switch {
	case len(s.Blocking()) > 0:
		return fmt.Sprintf("%d of %d required checks failed", len(s.Blocking()), requiredCount(s))
	case len(s.OptionalGaps()) > 0:
		// A plain "ready" over a red row is the bug this whole report exists to
		// stop telling. An OPTIONAL check that verified a gap is still a
		// verified gap: the integration does not work. Someone who reads the
		// first line and stops must not walk away believing otherwise, so the
		// headline names the failing check. The exit code stays unchanged —
		// optional means it does not fail a script, not that it is invisible.
		return fmt.Sprintf("core ready; %s not usable", strings.Join(s.OptionalGaps(), ", "))
	case s.Ready() && len(s.Unknown()) == 0:
		return "ready"
	case s.Ready():
		return fmt.Sprintf("ready (%d not checkable from here)", len(s.Unknown()))
	default:
		return fmt.Sprintf("not proven ready (%d unchecked)", len(s.Unknown()))
	}
}

func requiredCount(s Snapshot) int {
	n := 0
	for _, r := range s.Results {
		if r.Required {
			n++
		}
	}
	return n
}

// RenderDoctor writes the full diagnosis: every result with its evidence,
// then the exact fixes, in order. An unknown result explains WHY it is
// unknown and offers no command — doctor never guesses a repair for something
// it could not verify.
func RenderDoctor(w io.Writer, s Snapshot) { RenderDoctorWith(w, s, DoctorOpts{Verbose: true}) }

// DoctorOpts tunes the full report. Concise (the default for `pix doctor`)
// prints the evidence only for what is NOT ready: a green line's proof is
// noise until someone doubts it, and --verbose is that doubt.
type DoctorOpts struct {
	Verbose bool
	// Headline replaces the computed one. It exists for a caller that knows
	// something this snapshot cannot: `pix setup` runs PHASES, and a phase can
	// fail while every required probe still passes — which printed `✓ ready`
	// directly above "setup could not apply pack". The snapshot is not wrong;
	// it is answering a narrower question than the report was claiming to.
	Headline string
}

// RenderDoctorWith is RenderDoctor with the verbosity decision made by the
// caller.
func RenderDoctorWith(w io.Writer, s Snapshot, o DoctorOpts) {
	if o.Headline != "" {
		fmt.Fprintf(w, "✗ %s\n\n", o.Headline)
	} else {
		fmt.Fprintf(w, "%s %s\n\n", headlineGlyph(s), headline(s))
	}
	for _, r := range s.Results {
		req := "optional"
		if r.Required {
			req = "required"
		}
		fmt.Fprintf(w, "%s %-10s %-8s %s\n", Glyph(r.Effective()), r.Name, req, r.Detail)
		if ev := strings.TrimSpace(r.Evidence); ev != "" && (o.Verbose || !r.OK()) {
			writeEvidence(w, ev)
		}
	}
	if fixes := s.Fixes(); len(fixes) > 0 {
		fmt.Fprintf(w, "\nFix:\n")
		for _, f := range fixes {
			// A probe may hand back several commands when several servers need
			// different remedies; indent each so the block reads as a list
			// rather than one wrapped line.
			for _, line := range strings.Split(f, "\n") {
				fmt.Fprintf(w, "    %s\n", line)
			}
		}
	}
}
