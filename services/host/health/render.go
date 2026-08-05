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
func RenderDoctor(w io.Writer, s Snapshot) { RenderDoctorWith(w, s, DoctorOpts{Verbose: true}) }

// DoctorOpts tunes the full report. Concise (the default for `pix doctor`)
// prints the evidence only for what is NOT ready: a green line's proof is
// noise until someone doubts it, and --verbose is that doubt.
type DoctorOpts struct{ Verbose bool }

// RenderDoctorWith is RenderDoctor with the verbosity decision made by the
// caller.
func RenderDoctorWith(w io.Writer, s Snapshot, o DoctorOpts) {
	fmt.Fprintf(w, "%s %s\n\n", Glyph(headlineStatus(s)), headline(s))
	for _, r := range s.Results {
		req := "optional"
		if r.Required {
			req = "required"
		}
		fmt.Fprintf(w, "%s %-10s %-8s %s\n", Glyph(r.Effective()), r.Name, req, r.Detail)
		if ev := strings.TrimSpace(r.Evidence); ev != "" && (o.Verbose || !r.OK()) {
			fmt.Fprintf(w, "    evidence: %s\n", ev)
		}
	}
	if fixes := s.Fixes(); len(fixes) > 0 {
		fmt.Fprintf(w, "\nFix:\n")
		for _, f := range fixes {
			fmt.Fprintf(w, "    %s\n", f)
		}
	}
	if usesMCP(s) {
		fmt.Fprintf(w, "\n%s\n", MCPHostTrustNotice)
	}
}

// usesMCP reports whether this host has any MCP server configured, which is
// the gate on the host-trust disclosure. A host that configured none has
// taken no such risk, and telling it about one anyway is how a report teaches
// its reader to skim the footer.
func usesMCP(s Snapshot) bool {
	r, ok := s.Find("mcp")
	return ok && r.Detail != MCPNoneConfigured
}
