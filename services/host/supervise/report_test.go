package supervise

import (
	"os"
	"strings"
	"testing"
	"time"

	"pix/host/unitreport"
)

func TestReportPublishesTheOperatorFieldsSorted(t *testing.T) {
	tr := &Tree{units: map[string]*UnitStatus{
		"memory": {Name: "memory", Kind: "memory", Identity: "abc", State: UnitRunning, PID: 42,
			HealthOK: true, Reattached: true, Restarts: 2, Generations: 3, LastProbeUS: 1200,
			Since: time.Unix(1700000000, 0)},
		"broker": {Name: "broker", Kind: "svc", State: UnitBackoff, LastError: "boom"},
	}}
	rep := tr.Report()
	if rep.SchemaVersion != unitreport.SchemaVersion || rep.PID != os.Getpid() || rep.GeneratedUnix == 0 {
		t.Fatalf("report envelope is not self-describing: %+v", rep)
	}
	if len(rep.Units) != 2 || rep.Units[0].Name != "broker" || rep.Units[1].Name != "memory" {
		t.Fatalf("units must be name-sorted so two reads of an unchanged tree are byte-identical: %+v", rep.Units)
	}
	m := rep.Units[1]
	if m.Identity != "abc" || m.State != "running" || m.PID != 42 || !m.HealthOK || !m.Reattached ||
		m.Restarts != 2 || m.Generation != 3 || m.LastProbeUS != 1200 || m.SinceUnix != 1700000000 {
		t.Fatalf("memory report dropped an operator field: %+v", m)
	}
	if rep.Units[0].State != "backoff" || rep.Units[0].LastError != "boom" {
		t.Fatalf("a failing unit must publish its state and error: %+v", rep.Units[0])
	}
}

func TestReportRedactsUnitErrors(t *testing.T) {
	tr := &Tree{units: map[string]*UnitStatus{
		"broker": {Name: "broker", LastError: "spawn failed: BROKER_TOKEN=super-secret-value"},
	}}
	if got := tr.Report().Units[0].LastError; strings.Contains(got, "super-secret-value") {
		t.Fatalf("published report leaked a grant value: %q", got)
	}
}
