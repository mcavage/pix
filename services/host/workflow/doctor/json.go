package doctor

import (
	"pix/host/config"
	"pix/host/health"
	"pix/host/launcher"
)

// json.go is ONE schema for both verbs — one parser, and no second chance to
// disagree about the same host. A consumer reads one shape from either command
// and gets the same rows.
//
// SchemaVersion 4 is a BREAK, not an extension: v1-v3 described the
// requirement/verdict matrix this wave deleted, and there is no honest
// mapping from a four-status probe result onto a vocabulary that could not
// say "unknown". The version number is the contract that says so out loud,
// and RetiredSchemas below is the migration note that says what a consumer
// must do about it.
const SchemaVersion = 4

// RetiredSchemas is the MIGRATION CONTRACT for the versions this one
// replaces, keyed by version. Each value states what that shape was and what
// a consumer pinned to it has to do — which in every case is "read
// schema_version and re-read the rows", because there is no field-level
// mapping to write:
//
//   - v1/v2 published a nested `groups` tree of readiness Checks whose
//     verdict vocabulary (ready/todo/denied/unverifiable, crossed with
//     required/optional) does not survive; `unverifiable` and `todo` both
//     land in a four-status model that separates "I could not check" from
//     "I checked and it is missing" DIFFERENTLY per probe, so translating
//     mechanically would invent verdicts.
//   - v3 added a flat `checks` array alongside the groups, and `pix status`
//     published a SECOND, differently-shaped `dashboard` object. Both are
//     gone: the two verbs now emit one shape.
//
// The list is exhaustive and frozen. A consumer that finds a version not in
// {4} ∪ keys(RetiredSchemas) is reading output from a build newer than its
// own, which is the one case it should fail loudly on.
var RetiredSchemas = map[int]string{
	1: "nested `groups` of readiness Checks; no field maps onto the four-status model — read schema_version and re-read `checks`",
	2: "as v1 plus per-axis requirement rollups; same break, same migration",
	3: "`groups` plus a flat `checks` array, and a separate `dashboard` object from `pix status`; both verbs now emit one shape — read `checks`",
}

// RetiredSchemaKeys are the top-level field names v1-v3 published that v4
// does NOT. They are listed so the break is testable in both directions: a v4
// payload must carry none of them, which is what makes a stale parser fail on
// a missing field instead of silently reading a subset it recognizes.
var RetiredSchemaKeys = []string{"groups", "dashboard", "axes", "ok"}

// ReportJSONView is the machine-readable snapshot behind `--json`.
type ReportJSONView struct {
	SchemaVersion int    `json:"schema_version"`
	Version       string `json:"version"`
	ConfigPath    string `json:"config_path"`
	Profile       string `json:"profile"`
	// Verdict is the snapshot in one word: ready | gaps | unknown.
	Verdict string `json:"verdict"`
	// Ready is true only when every REQUIRED check proved ready. An unknown
	// is never ready.
	Ready  bool        `json:"ready"`
	Checks []CheckJSON `json:"checks"`
	// Fixes are the exact repair commands, de-duplicated, in probe order.
	// Only a verified gap contributes one.
	Fixes []string `json:"fixes"`
	// Exit is the code `pix doctor` returns for this snapshot. `pix status`
	// publishes the same number while itself exiting 0, so the two surfaces
	// cannot tell a reader different things.
	Exit      int   `json:"exit"`
	ElapsedMS int64 `json:"elapsed_ms"`
}

// CheckJSON is one probe's answer.
type CheckJSON struct {
	Name     string `json:"name"`
	Status   string `json:"status"` // ready | absent | denied | unknown
	Required bool   `json:"required"`
	Detail   string `json:"detail"`
	Evidence string `json:"evidence,omitempty"`
	// Fix is present ONLY on a verified gap. A green row with a TODO under it
	// is the report undermining itself.
	Fix        string `json:"fix,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

// ReportJSON renders a snapshot into its serializable form. exit is passed in
// rather than derived so status can publish doctor's verdict while returning
// its own (always 0) exit code.
func ReportJSON(s health.Snapshot, profile string, exit int) ReportJSONView {
	v := ReportJSONView{
		SchemaVersion: SchemaVersion,
		Version:       launcher.Version,
		ConfigPath:    config.Path(),
		Profile:       profile,
		Verdict:       verdictOf(s),
		Ready:         s.Ready(),
		Checks:        make([]CheckJSON, 0, len(s.Results)),
		Fixes:         s.Fixes(),
		Exit:          exit,
		ElapsedMS:     s.Elapsed.Milliseconds(),
	}
	if v.Fixes == nil {
		v.Fixes = []string{}
	}
	for _, r := range s.Results {
		v.Checks = append(v.Checks, CheckJSON{
			Name:       r.Name,
			Status:     string(r.Effective()),
			Required:   r.Required,
			Detail:     r.Detail,
			Evidence:   r.Evidence,
			Fix:        r.Fix,
			DurationMS: r.Took.Milliseconds(),
		})
	}
	return v
}

// verdictOf reduces a snapshot to one word, by the same precedence the
// headline uses: a verified required gap dominates, then anything unproven.
func verdictOf(s health.Snapshot) string {
	switch {
	case len(s.Blocking()) > 0:
		return "gaps"
	case s.Ready() && len(s.Unknown()) == 0:
		return "ready"
	case s.Ready():
		return "unknown"
	default:
		return "unknown"
	}
}
