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
	fmt.Fprintf(w, "%s %s\n", Glyph(headlineStatus(s)), headline(s))
	if len(ready) > 0 {
		fmt.Fprintf(w, "  ready    %s\n", strings.Join(ready, " "))
	}
	if len(gaps) > 0 {
		fmt.Fprintf(w, "  missing  %s\n", strings.Join(gaps, " "))
	}
	if len(unknown) > 0 {
		fmt.Fprintf(w, "  unknown  %s\n", strings.Join(unknown, " "))
	}
}

// headlineStatus reduces a snapshot to the single status its headline claims:
// a verified required gap dominates, then anything unproven, then ready.
func headlineStatus(s Snapshot) Status {
	if len(s.Blocking()) > 0 {
		return StatusAbsent
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
func RenderDoctor(w io.Writer, s Snapshot) {
	fmt.Fprintf(w, "%s %s\n\n", Glyph(headlineStatus(s)), headline(s))
	for _, r := range s.Results {
		req := "optional"
		if r.Required {
			req = "required"
		}
		fmt.Fprintf(w, "%s %-10s %-8s %s\n", Glyph(r.Effective()), r.Name, req, r.Detail)
		if ev := strings.TrimSpace(r.Evidence); ev != "" {
			fmt.Fprintf(w, "    evidence: %s\n", ev)
		}
	}
	fixes := s.Fixes()
	if len(fixes) == 0 {
		return
	}
	fmt.Fprintf(w, "\nFix:\n")
	for _, f := range fixes {
		fmt.Fprintf(w, "    %s\n", f)
	}
}
