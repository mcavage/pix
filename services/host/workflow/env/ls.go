// ls.go — E1.9's `pix env ls`: registered environments, the machine
// default, and each one's current host-review state. Per doc.go's "no
// process globals" rule, this file never calls config.Load or touches the
// filesystem beyond the environment trust store review.go already owns —
// a caller (cmd/pix/env_cmd.go) supplies cfg explicitly.
package env

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"pix/host/config"
)

// LsSchemaVersion is `env ls --json`'s schema_version (AC-64: every `pix
// env` --json surface carries one).
const LsSchemaVersion = 1

// LsEntry is one registered environment row. ReviewState is the full
// four-state answer (computeReviewState, reviewstate.go); Accepted is kept
// alongside it — never derived independently again — purely for backward
// compatibility with a caller still reading the pre-existing bool: it is
// exactly `ReviewState == ReviewAccepted`, no longer a separate
// presence-only check.
type LsEntry struct {
	Name        string      `json:"name"`
	Root        string      `json:"root"`
	Default     bool        `json:"default"`
	Accepted    bool        `json:"accepted"`
	ReviewState ReviewState `json:"review_state"`
}

// LsResult is `env ls`'s complete, already-computed answer, before either
// of RenderLs/RenderLsJSON turns it into text.
type LsResult struct {
	// Default is the machine default NAME, or "" when none is selected
	// (D17's `none` state — naming that word is a renderer's job, not
	// this type's).
	Default string
	Entries []LsEntry
}

// ComputeLs builds LsResult from cfg: one row per Known(cfg) name — already
// sorted, so the listing is deterministic across runs and working
// directories (AC-10's same discipline extended to a listing rather than a
// single lookup) — each marked Default when it equals cfg.Environment and
// carrying the full computeReviewState answer for its CURRENT canonical
// root. A repointed name (AC-16) is never cached as accepted: its NEW root
// is simply a Subject the trust store has never seen, so this reports it
// unaccepted the instant it is looked up.
//
// A single broken registration must never take down the whole listing: a
// name whose Load fails outright (a missing or now-invalid `.sbxenv.yaml`,
// a location refusal, ...) — or whose bill cannot be computed even once
// Load succeeds — renders as ReviewInvalid rather than aborting ComputeLs
// for every OTHER, perfectly healthy row. `pix env show NAME` (or `edit`)
// is where that one broken entry's actual diagnostic lives; `ls` only ever
// names that something is wrong, never why.
func ComputeLs(cfg *config.Config) (LsResult, error) {
	ts, err := loadEnvironmentTrustStore()
	if err != nil {
		return LsResult{}, err
	}
	names := Known(cfg)
	entries := make([]LsEntry, 0, len(names))
	for _, name := range names {
		root, _ := Root(cfg, name)
		entry := LsEntry{Name: name, Root: root, Default: name == cfg.Environment}
		loaded, loadErr := Load(cfg, &ts.AcceptanceStore, name, nil, nil)
		if loadErr != nil {
			entry.ReviewState = ReviewInvalid
			entries = append(entries, entry)
			continue
		}
		status, statusErr := computeReviewState(loaded, ts, nil, nil)
		if statusErr != nil {
			entry.ReviewState = ReviewInvalid
			entries = append(entries, entry)
			continue
		}
		entry.ReviewState = status.State
		entry.Accepted = status.State == ReviewAccepted
		entries = append(entries, entry)
	}
	return LsResult{Default: cfg.Environment, Entries: entries}, nil
}

// lsReviewColumn renders one entry's REVIEW column: the four explicit
// states in their own short, lowercase ls-column spelling —
// ReviewNotRequired as "n/a" (nothing to review, never a fifth taxonomy
// word), plus ReviewInvalid (a Load/bill failure ComputeLs degrades to
// rather than aborting the whole listing — its own doc comment). Any other
// value (never produced by ComputeLs today) prints its own raw string
// rather than silently blanking a state a future caller adds.
func lsReviewColumn(s ReviewState) string {
	switch s {
	case ReviewNotRequired:
		return "n/a"
	case ReviewUnaccepted:
		return "unaccepted"
	case ReviewAccepted:
		return "accepted"
	case ReviewChanged:
		return "changed"
	case ReviewInvalid:
		return "invalid"
	default:
		return string(s)
	}
}

// RenderLs writes `env ls`'s human presentation: PRD D17's own "built-in
// defaults" vocabulary when nothing is registered at all (never "default
// environment" — banned as vocabulary, §5 rule 4), otherwise one row per
// entry naming only the facts §5.10 promises ("List registered
// environments. Marks the default.") plus the review state every other
// `pix env` verb already exposes — no status taxonomy beyond the shared
// four-state ReviewState, no WHY column.
func RenderLs(out io.Writer, r LsResult) {
	if len(r.Entries) == 0 {
		fmt.Fprintln(out, "no environments registered; pix runs with its own built-in defaults.")
		fmt.Fprintln(out, "register one: pix env add <name> [path]")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tDEFAULT\tREVIEW\tROOT")
	for _, e := range r.Entries {
		def := ""
		if e.Default {
			def = "*"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", e.Name, def, lsReviewColumn(e.ReviewState), e.Root)
	}
	tw.Flush()
}

// lsJSONView is `env ls --json`'s wire shape.
type lsJSONView struct {
	SchemaVersion int       `json:"schema_version"`
	Environment   string    `json:"environment"`
	Environments  []LsEntry `json:"environments"`
}

// RenderLsJSON writes `env ls --json`. Environment is "none" exactly when
// no machine default is selected (D17) — the identical literal `env show
// --json` (AC-46) emits for the same fact, so a script never special-cases
// which verb it asked. Environments is always an array, never `null`, even
// when empty.
func RenderLsJSON(out io.Writer, r LsResult) error {
	name := r.Default
	if name == "" {
		name = "none"
	}
	entries := r.Entries
	if entries == nil {
		entries = []LsEntry{}
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(lsJSONView{SchemaVersion: LsSchemaVersion, Environment: name, Environments: entries})
}
