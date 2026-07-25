package main

// readiness_render.go holds the ONE verdict vocabulary. Every renderer —
// doctor, status, run's warnings, the onboarding host-state payload, setup —
// maps a (requirement, verdict) pair to a glyph and a word THROUGH these two
// functions. A renderer that spells a glyph itself is how two commands start
// disagreeing about the same fact, which is the whole defect this wave exists
// to remove.
//
// The mapping, and the only four verdict words that may appear anywhere:
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

// verdictGlyph maps a (requirement, verdict, note) triple to its marker.
// A requested axis renders exactly like a core one: the user asked for it, so
// a gap there is a hard ✗, not a shrug.
func verdictGlyph(req requirement, v verdict, note bool) string {
	if note {
		return glyphNote
	}
	switch v {
	case verdictReady:
		return glyphReady
	case verdictDenied:
		return glyphDenied
	case verdictTodo:
		if blocksExit(req) {
			return glyphTodoCore
		}
		return glyphTodoOptional
	default:
		return glyphUnverifiable
	}
}

// verdictWord maps a verdict to the ONE word every renderer uses for it.
func verdictWord(v verdict) string {
	switch v {
	case verdictReady:
		return "ready"
	case verdictTodo:
		return "needs setup"
	case verdictDenied:
		return "blocked"
	default:
		return "can't check from here"
	}
}

// checkGlyph is the check-level shorthand every renderer calls.
func checkGlyph(c check) string { return verdictGlyph(c.req(), c.result(), c.note) }

// checkWord is the check-level shorthand for the verdict word.
func checkWord(c check) string {
	if c.note {
		return verdictWord(verdictUnverifiable)
	}
	return verdictWord(c.result())
}

// readinessFooter names the ONE next command for a renderer, so no surface
// ends with a menu of options.
func readinessFooter(surface string, s Snapshot) string {
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
