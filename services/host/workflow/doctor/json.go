package doctor

import (
	"pix/host/config"
	"pix/host/health"
	"pix/host/launcher"
)

// json.go is ONE schema for both verbs. The pair used to emit two (doctor's
// nested groups + a flat checks array, status's dashboard object with a
// different notion of "up"), which meant two parsers and two chances to
// disagree about the same host. A consumer now reads one shape from either
// command and gets the same rows.
//
// schemaVersion 4 is a BREAK, not an extension: v1-v3 described the
// requirement/verdict matrix this wave deleted, and there is no honest
// mapping from a four-status probe result onto a vocabulary that could not
// say "unknown". The version number is the contract that says so out loud.
const schemaVersion = 4

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
		SchemaVersion: schemaVersion,
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
