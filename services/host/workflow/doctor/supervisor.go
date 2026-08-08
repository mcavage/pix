// supervisor.go — the supervision tree, as doctor/status publish it.
//
// `pix-host serve` owns the Suture tree; this process is a different process and
// does not get to guess. It reads the snapshot serve publishes (state dir, see
// config.ServeUnitsPath) and reports exactly what it found, including "I could
// not see it" — a supervision surface that renders silence as health is the
// surface that misses the outage.

package doctor

import (
	"fmt"
	"time"

	"pix/host/config"
	"pix/host/unitreport"
)

// supervisorStaleAfter matches serve's publish interval budget: serve
// republishes every 5s, so three missed intervals is a wedged or dead daemon.
const supervisorStaleAfter = 20 * time.Second

// SupervisorJSON is the `supervisor` object in the v5 snapshot. It is ALWAYS
// present: a reader must be able to tell "no units" from "could not look".
type SupervisorJSON struct {
	// Available is true only when a fresh, schema-matching snapshot was read.
	Available bool `json:"available"`
	// Detail explains an unavailable or stale snapshot in one line.
	Detail        string            `json:"detail,omitempty"`
	PID           int               `json:"pid,omitempty"`
	GeneratedUnix int64             `json:"generated_unix,omitempty"`
	Units         []unitreport.Unit `json:"units"`
}

// supervisorSnapshot is the seam tests replace; production reads the state dir.
var supervisorSnapshot = readSupervisorSnapshot

// readSupervisorSnapshot loads and freshness-checks serve's published tree.
func readSupervisorSnapshot() SupervisorJSON {
	out := SupervisorJSON{Units: []unitreport.Unit{}}
	rep, found, err := unitreport.ReadReport(config.ServeUnitsPath())
	switch {
	case err != nil:
		out.Detail = fmt.Sprintf("unreadable supervision snapshot (%v)", err)
		return out
	case !found:
		out.Detail = "no supervision snapshot (serve not running?)"
		return out
	case rep.SchemaVersion != unitreport.SchemaVersion:
		out.Detail = fmt.Sprintf("supervision snapshot schema %d, this build reads %d",
			rep.SchemaVersion, unitreport.SchemaVersion)
		return out
	}
	out.PID, out.GeneratedUnix = rep.PID, rep.GeneratedUnix
	// Staleness is checked BEFORE the units are attached, same as the
	// unreadable/missing/schema-mismatch cases above: a stale snapshot's
	// units are refused, not shown alongside Available=false, so a reader
	// never mistakes a dead tree's last known rows for current state.
	if age := time.Since(time.Unix(rep.GeneratedUnix, 0)); age > supervisorStaleAfter {
		out.Detail = fmt.Sprintf("supervision snapshot is %ds stale", int(age.Seconds()))
		return out
	}
	if len(rep.Units) > 0 {
		out.Units = rep.Units
	}
	out.Available = true
	return out
}
