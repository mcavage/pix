// supervisor_test.go — the `supervisor` object doctor/status publish. The rule
// is the same one the rest of doctor lives by: a check that could not be MADE
// says "unknown", it never renders as green.
package doctor

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pix/host/health"
	"pix/host/unitreport"
)

func snapshotFrom(t *testing.T, rep unitreport.Report, write bool) SupervisorJSON {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	if write {
		if err := unitreport.WriteReport(filepath.Join(dir, "pix", "serve.units.json"), rep); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	return readSupervisorSnapshot()
}

func TestSupervisorSnapshotAvailability(t *testing.T) {
	live := unitreport.Report{SchemaVersion: unitreport.SchemaVersion, PID: 4242,
		GeneratedUnix: time.Now().Unix(),
		Units: []unitreport.Unit{{Name: "memory", Kind: "memory", Identity: "abc", State: "running",
			PID: 4243, HealthOK: true, Generation: 2, Restarts: 1, LastProbeUS: 800}}}

	got := snapshotFrom(t, live, true)
	if !got.Available || got.PID != 4242 || len(got.Units) != 1 || got.Units[0].Restarts != 1 {
		t.Fatalf("a fresh snapshot must be published as-is: %+v", got)
	}

	missing := snapshotFrom(t, unitreport.Report{}, false)
	if missing.Available || !strings.Contains(missing.Detail, "no supervision snapshot") || missing.Units == nil {
		t.Fatalf("a missing snapshot must be unavailable with a reason and an empty (non-null) unit list: %+v", missing)
	}

	stale := live
	stale.GeneratedUnix = time.Now().Add(-5 * time.Minute).Unix()
	st := snapshotFrom(t, stale, true)
	if st.Available || !strings.Contains(st.Detail, "stale") || len(st.Units) != 1 {
		t.Fatalf("a stale snapshot shows its units but is NOT available: %+v", st)
	}

	future := live
	future.SchemaVersion = unitreport.SchemaVersion + 1
	fu := snapshotFrom(t, future, true)
	if fu.Available || !strings.Contains(fu.Detail, "schema") {
		t.Fatalf("an unknown schema must not be parsed as if it were ours: %+v", fu)
	}
}

func TestReportJSONCarriesTheSupervisorObject(t *testing.T) {
	prev := supervisorSnapshot
	t.Cleanup(func() { supervisorSnapshot = prev })
	supervisorSnapshot = func() SupervisorJSON {
		return SupervisorJSON{Available: true, PID: 9, GeneratedUnix: 1700000000,
			Units: []unitreport.Unit{{Name: "memory", State: "degraded", Restarts: 3,
				Generation: 4, LastProbeUS: 3100, LastError: "probe timeout"}}}
	}
	snap := health.Snapshot{Results: []health.Result{{Name: "sbx", Status: health.StatusReady, Required: true}}}
	b, err := json.Marshal(ReportJSON(snap, "default", snap.ExitCode()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out struct {
		SchemaVersion int            `json:"schema_version"`
		Supervisor    SupervisorJSON `json:"supervisor"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.SchemaVersion != 5 {
		t.Fatalf("the supervisor object ships in schema 5, got %d", out.SchemaVersion)
	}
	u := out.Supervisor.Units
	if len(u) != 1 || u[0].State != "degraded" || u[0].Restarts != 3 || u[0].Generation != 4 ||
		u[0].LastProbeUS != 3100 || u[0].LastError != "probe timeout" {
		t.Fatalf("doctor --json dropped a supervision field: %+v", u)
	}
}

func TestRetiredSchemasNamesV4(t *testing.T) {
	note, ok := RetiredSchemas[4]
	if !ok || !strings.Contains(note, "supervisor") {
		t.Fatalf("v4 must be retired with a note naming what v5 added: %q", note)
	}
}
