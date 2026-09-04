// recreates.go — E2.6's doctor-side half of the I4 recreate diagnostic
// (docs/design/environments.md §9.4, AC-71): `pix doctor`'s default-tier
// one-line pointer and `pix doctor --recreates`'s full record listing.
//
// Both READ recreatelog's on-disk log through its own exported API; neither
// this file nor doctor_cmd.go ever writes it — the ONE write happens on a
// creation-fingerprint attach refusal, from workflow/launch's
// RecordAttachRefusal (E2.6's other, launch-side half). Keeping the write
// there and the read here mirrors the L1-capability/L3-workflow split every
// other diagnostic in this tree already uses.
package doctor

import (
	"fmt"
	"io"
	"time"

	"pix/host/recreatelog"
)

// RecreateSummaryLine renders §9.4's default-tier line: NOTHING at count
// zero — a host with no recreate drift never learns the counter exists —
// and, at count > 0, exactly the one line PRD §5.9/§9.4 name, verbatim:
//
//	environments   12 unplanned recreates recorded   pix doctor --recreates
//
// Never a key path, never which environment: full attribution needs the
// explicit, separate `pix doctor --recreates` below.
func RecreateSummaryLine(w io.Writer, dir string) error {
	records, err := recreatelog.Read(dir)
	if err != nil {
		return err
	}
	if len(records) == 0 {
		return nil
	}
	word := "recreates"
	if len(records) == 1 {
		word = "recreate"
	}
	fmt.Fprintf(w, "  environments   %d unplanned %s recorded   pix doctor --recreates\n", len(records), word)
	return nil
}

// RenderRecreates is `pix doctor --recreates` (§9.4's full form): the exact
// cap note, the file path, every retained record (timestamp, environment,
// and its drifted canonical key paths only), and the local-only/deletable
// note. It never runs a probe and never fails the process on an empty or
// missing log — only a genuinely corrupt one (recreatelog.Read's own
// contract) surfaces as an error here, because the user explicitly asked to
// read this file.
func RenderRecreates(w io.Writer, dir string) error {
	records, err := recreatelog.Read(dir)
	if err != nil {
		return err
	}
	path := recreatelog.Path(dir)
	fmt.Fprintf(w, "recreate records: %d (cap %d, oldest dropped)\n", len(records), recreatelog.MaxRecords)
	fmt.Fprintf(w, "file: %s\n", path)
	if len(records) > 0 {
		fmt.Fprintln(w)
		for _, r := range records {
			fmt.Fprintf(w, "%s  %s  %s\n", r.Timestamp.UTC().Format(time.RFC3339), r.Environment, joinChangedKeyPaths(r.ChangedKeyPaths))
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "local only, never uploaded. delete the file whenever you like; that is not an error.")
	return nil
}

func joinChangedKeyPaths(paths []string) string {
	out := ""
	for i, p := range paths {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}
