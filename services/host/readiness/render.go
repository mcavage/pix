package readiness

// readiness_render.go holds the ONE Verdict vocabulary. Every renderer —
// doctor, status, run's warnings, the onboarding host-state payload, setup —
// maps a (Requirement, Verdict) pair to a glyph and a word THROUGH these two
// functions. A renderer that spells a glyph itself is how two commands start
// disagreeing about the same fact, which is the whole defect this wave exists
// to remove.
//
// The mapping, and the only four Verdict words that may appear anywhere:
//
//	ready         ✓  "ready"
//	todo (core)   ✗  "needs setup"
//	todo (opt)    ⚠  "needs setup"
//	unverifiable  ?  "can't check from here"
//	denied        ⊘  "blocked"
//
// A note (a pure annotation, asserting nothing) renders ·.

const (
	glyphReady        = "✓"
	glyphTodoCore     = "✗"
	glyphTodoOptional = "⚠"
	glyphUnverifiable = "?"
	glyphDenied       = "⊘"
	glyphNote         = "·"
)

// VerdictGlyph maps a (Requirement, Verdict, note) triple to its marker.
// A requested axis renders exactly like a core one: the user asked for it, so
// a gap there is a hard ✗, not a shrug.
func VerdictGlyph(req Requirement, v Verdict, note bool) string {
	if note {
		return glyphNote
	}
	switch v {
	case VerdictReady:
		return glyphReady
	case VerdictDenied:
		return glyphDenied
	case VerdictTodo:
		if BlocksExit(req) {
			return glyphTodoCore
		}
		return glyphTodoOptional
	default:
		return glyphUnverifiable
	}
}

// VerdictWord maps a Verdict to the ONE word every renderer uses for it.
func VerdictWord(v Verdict) string {
	switch v {
	case VerdictReady:
		return "ready"
	case VerdictTodo:
		return "needs setup"
	case VerdictDenied:
		return "blocked"
	default:
		return "can't check from here"
	}
}

// Glyph is the Check-level shorthand every renderer calls.
func Glyph(c Check) string { return VerdictGlyph(c.Req(), c.Result(), c.Note) }

// Word is the Check-level shorthand for the Verdict word.
func Word(c Check) string {
	if c.Note {
		return VerdictWord(VerdictUnverifiable)
	}
	return VerdictWord(c.Result())
}

// Footer names the ONE next command for a renderer, so no surface
// ends with a menu of options.
func Footer(surface string, s Snapshot) string {
	switch surface {
	case "status":
		return "pix doctor"
	case "doctor":
		if s.Outstanding() > 0 {
			return "pix setup"
		}
		return "pix run"
	case "setup":
		return "pix run"
	case "run", "onboard":
		// The daily surfaces show at most a handful of rows; the full list,
		// with every fix command, is doctor's job.
		return "pix doctor"
	default:
		return "pix doctor"
	}
}
