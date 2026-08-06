// report.go — the supervision tree's own view, rendered into the shared
// unitreport shape (an L0 package, because `serve status` at L1 and doctor at L3
// read the same snapshot and neither may import this one).

package supervise

import (
	"os"
	"sort"
	"time"

	"pix/host/unitreport"
)

// Report snapshots every supervised unit, sorted by name so two consecutive
// reads of an unchanged tree are byte-identical (a status file that churns on
// map order is a status file nobody can diff).
func (t *Tree) Report() unitreport.Report {
	t.mu.Lock()
	units := make([]unitreport.Unit, 0, len(t.units))
	for _, st := range t.units {
		units = append(units, st.report())
	}
	t.mu.Unlock()
	sort.Slice(units, func(i, j int) bool { return units[i].Name < units[j].Name })
	return unitreport.Report{
		SchemaVersion: unitreport.SchemaVersion,
		PID:           os.Getpid(),
		GeneratedUnix: time.Now().Unix(),
		Units:         units,
	}
}

// report converts one live status into its published form. Called under t.mu.
func (st *UnitStatus) report() unitreport.Unit {
	return unitreport.Unit{
		Name:        st.Name,
		Kind:        st.Kind,
		Identity:    st.Identity,
		State:       string(st.State),
		PID:         st.PID,
		HealthOK:    st.HealthOK,
		Reattached:  st.Reattached,
		Restarts:    st.Restarts,
		Generation:  st.Generations,
		LastError:   unitreport.ScrubError(st.LastError),
		LastProbeUS: st.LastProbeUS,
		SinceUnix:   st.Since.Unix(),
	}
}
