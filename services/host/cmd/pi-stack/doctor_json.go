package main

// doctorSchemaVersion is bumped whenever the JSON shape gains/changes fields a
// machine consumer might depend on. v1 (implicit — no schema_version field)
// carried verdict/profile/todos/groups/services/mcp/sbx_absent with per-check
// label/state/detail/todo. v2 adds schema_version itself, the top-level
// blocking flag, and the structured per-check readiness fields
// (group/requirement/verdict/evidence, plus note); the v1 fields are all
// RETAINED unchanged for compatibility.
const doctorSchemaVersion = 2

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
}

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
func (r *report) jsonView(profile string) doctorJSON {
	todos := r.todos()
	// Verdict derives from the same axes the headline uses: a verified core
	// failure → blocked; any verified failure → outstanding; nothing verified
	// failing but unverifiable checks → unverifiable; else pass.
	verdict := "pass"
	switch {
	case r.blocking():
		verdict = "blocked"
	case r.outstanding() > 0:
		verdict = "outstanding"
	case r.unverifiableCount() > 0:
		verdict = "unverifiable"
	}
	v := doctorJSON{
		SchemaVersion: doctorSchemaVersion,
		Verdict:       verdict,
		Blocking:      r.blocking(),
		Profile:       profile,
		Todos:         todos,
		Services:      r.services,
		MCP:           r.mcp,
		SbxAbsent:     r.sbxAbsent,
	}
	for _, g := range r.groups {
		gj := doctorGroupJSON{Title: g.title}
		for _, c := range g.checks {
			res := c.result()
			todo := c.todo
			if res != verdictTodo && res != verdictDenied {
				todo = "" // only a verified failure carries a repair command
			}
			gj.Checks = append(gj.Checks, doctorCheckJSON{
				Group:       g.title,
				Label:       c.label,
				Requirement: string(c.req()),
				Verdict:     string(res),
				Evidence:    c.evidenceString(),
				Todo:        todo,
				Note:        c.note,
				State:       stateName(c.state()),
				Detail:      c.detail,
			})
		}
		v.Groups = append(v.Groups, gj)
	}
	return v
}

// stateName maps a rendered checkState to its v1-compatible JSON string.
// stateWarn renders as "warn" (additive — v1 never produced it) so a JSON
// consumer can tell an unverifiable result apart from a plain info line.
func stateName(s checkState) string {
	switch s {
	case stateOK:
		return "ok"
	case stateTODO:
		return "todo"
	case stateWarn:
		return "warn"
	default:
		return "info"
	}
}
