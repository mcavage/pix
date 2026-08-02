package main

import (
	"strings"

	"pix/host/readiness"
)

// doctorSchemaVersion is bumped whenever the JSON shape gains/changes fields a
// machine consumer might depend on. v1 (implicit — no schema_version field)
// carried verdict/profile/todos/groups/services/mcp/sbx_absent with per-check
// label/state/detail/todo. v2 adds schema_version itself, the top-level
// blocking flag, and the structured per-check readiness fields
// (group/requirement/verdict/evidence, plus note); the v1 fields are all
// RETAINED unchanged for compatibility.
// v3 adds the flat readiness `checks` array (axis/requirement/verdict/
// evidence/fix/duration_ms/endpoint) and the `exit` sibling that equals the
// process exit code. Additive only: every v1/v2 key keeps its name.
const doctorSchemaVersion = 3

// doctorJSON is the machine-readable doctor report (behind --json).
type doctorJSON struct {
	SchemaVersion int `json:"schema_version"`
	// Verdict: pass | outstanding | unverifiable | blocked. v1 emitted only
	// pass|outstanding; the two new values are additive (they appear only
	// when the new axes detect what v1 could not express).
	Verdict string `json:"verdict"`
	// Blocking mirrors the exit code: true iff a positively verified core
	// todo/denied exists (doctor exits 1).
	Blocking  bool              `json:"blocking"`
	Profile   string            `json:"profile"`
	Todos     []string          `json:"todos"`
	Groups    []doctorGroupJSON `json:"groups"`
	Services  []string          `json:"services"`
	MCP       []string          `json:"mcp"`
	SbxAbsent bool              `json:"sbx_absent"`
	// Checks is the flat, axis-keyed readiness array every readiness command
	// emits identically, and Exit is the process exit code this same data
	// produced. A consumer reading `exit` and a consumer reading the rows can
	// never reach different conclusions.
	Checks []readinessCheckJSON `json:"checks"`
	Exit   int                  `json:"exit"`
}

// readinessCheckJSON is the shared per-check JSON row. It is emitted by every
// readiness command's --json (doctor, status), so one parser reads them all.
type readinessCheckJSON struct {
	Axis        string   `json:"axis"`
	Label       string   `json:"label"`
	Requirement string   `json:"requirement"`
	Verdict     string   `json:"verdict"`
	Evidence    string   `json:"evidence"`
	Fix         []string `json:"fix"`
	DurationMS  int64    `json:"duration_ms"`
	Endpoint    string   `json:"endpoint,omitempty"`
	Note        bool     `json:"note,omitempty"`
}

// readinessChecksJSON renders any set of checks into the shared array. Only a
// VERIFIED failure carries a fix, mirroring report.todos().
func readinessChecksJSON(checks []readiness.Check) []readinessCheckJSON {
	out := make([]readinessCheckJSON, 0, len(checks))
	for _, c := range checks {
		row := readinessCheckJSON{
			Axis:        string(c.AxisOf()),
			Label:       strings.TrimSpace(c.Label),
			Requirement: string(c.Req()),
			Verdict:     string(c.Result()),
			Evidence:    c.EvidenceString(),
			Fix:         []string{},
			DurationMS:  c.Duration.Milliseconds(),
			Endpoint:    c.Endpoint,
			Note:        c.Note,
		}
		if v := c.Result(); (v == readiness.VerdictTodo || v == readiness.VerdictDenied) && c.Todo != "" {
			row.Fix = []string{c.Todo}
		}
		out = append(out, row)
	}
	return out
}

// Report.Snapshot moved to the readiness package: reconstructing a Snapshot
// from a Report is model machinery, and Go requires a method to live with its
// type anyway.

type doctorGroupJSON struct {
	Title  string            `json:"title"`
	Checks []doctorCheckJSON `json:"checks"`
}

type doctorCheckJSON struct {
	// Group names the parent group on every check so a flat consumer never
	// has to re-derive it from the nesting.
	Group       string `json:"group"`
	Label       string `json:"label"`
	Requirement string `json:"requirement"` // core | optional
	Verdict     string `json:"verdict"`     // ready | todo | unverifiable | denied
	// Evidence is the concrete machine-readable proof string behind the
	// verdict (falls back to the human detail, never empty for a real check).
	Evidence string `json:"evidence"`
	// Todo is the exact copy-pasteable command; emitted ONLY for a verified
	// todo/denied (an unverifiable check never carries a repair command).
	Todo string `json:"todo,omitempty"`
	Note bool   `json:"note,omitempty"`
	// State + Detail are the v1 compatibility fields. State maps the verdict
	// onto the v1 vocabulary: ready→ok, todo/denied→todo, note→info, and
	// unverifiable→warn (additive: v1 never emitted it).
	State  string `json:"state"`
	Detail string `json:"detail"`
}

// jsonView renders the report into its serializable form (the same data render
// prints, minus the glyph presentation).
// jsonView is a function rather than a method now: doctorJSON is doctor's own
// wire schema (v3, with its compatibility history), and readiness has no
// business owning it. Report moved; the schema stayed.
func jsonView(r *readiness.Report, profile string) doctorJSON {
	todos := r.Todos()
	// Verdict derives from the same axes the headline uses: a verified core
	// failure → blocked; any verified failure → outstanding; nothing verified
	// failing but unverifiable checks → unverifiable; else pass.
	verdict := "pass"
	switch {
	case r.Blocking():
		verdict = "blocked"
	case r.Outstanding() > 0:
		verdict = "outstanding"
	case r.UnverifiableCount() > 0:
		verdict = "unverifiable"
	}
	v := doctorJSON{
		SchemaVersion: doctorSchemaVersion,
		Verdict:       verdict,
		Blocking:      r.Blocking(),
		Profile:       profile,
		Todos:         todos,
		Services:      r.Services,
		MCP:           r.MCP,
		SbxAbsent:     r.SbxAbsent,
	}
	snap := r.Snapshot()
	v.Checks = readinessChecksJSON(snap.All())
	v.Exit = snap.ExitCode()
	for _, g := range r.Groups {
		gj := doctorGroupJSON{Title: g.Title}
		for _, c := range g.Checks {
			res := c.Result()
			todo := c.Todo
			if res != readiness.VerdictTodo && res != readiness.VerdictDenied {
				todo = "" // only a verified failure carries a repair command
			}
			gj.Checks = append(gj.Checks, doctorCheckJSON{
				Group:       g.Title,
				Label:       c.Label,
				Requirement: string(c.Req()),
				Verdict:     string(res),
				Evidence:    c.EvidenceString(),
				Todo:        todo,
				Note:        c.Note,
				State:       stateName(c.State()),
				Detail:      c.Detail,
			})
		}
		v.Groups = append(v.Groups, gj)
	}
	return v
}

// stateName maps a rendered checkState to its v1-compatible JSON string.
// readiness.StateWarn renders as "warn" (additive — v1 never produced it) so a JSON
// consumer can tell an unverifiable result apart from a plain info line.
func stateName(s readiness.CheckState) string {
	switch s {
	case readiness.StateOK:
		return "ok"
	case readiness.StateTODO:
		return "todo"
	case readiness.StateWarn:
		return "warn"
	default:
		return "info"
	}
}
