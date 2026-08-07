// Package unitreporttest is the shared table of supervision-snapshot scenarios
// that `service` (`pix serve status`) and `workflow/doctor` (`pix doctor`/
// `pix status --json`) both classify from the same on-disk serve.units.json.
// Two independent readers of one published file are exactly the pair that
// drifts silently apart — this table is the contract that keeps them agreeing
// on "fresh", "stale by age", "missing", and "unknown schema", so a future
// change to either reader's wording or thresholds fails a shared test instead
// of only the one file its author happened to be editing.
//
// It is production-shaped (a plain .go filename, not fixtures_test.go) for the
// same reason sys/systest is (see that package's doc comment): a package
// built only from _test.go files cannot be imported by another package's
// tests. Its only real importers, across the module, are _test.go files.
package unitreporttest

import (
	"time"

	"pix/host/unitreport"
)

// PID is the pid the fixture snapshots claim to have been published by — the
// "running pid" a caller must supply to get the WantHealthy/WantUnitsLen
// outcome below.
const PID = 4242

// Scenario is one way a reader can find (or fail to find) serve.units.json.
// Every scenario assumes the daemon IS running under PID and reads with the
// real wall clock: that is the only case in which a snapshot's absence or
// staleness is worth reporting on, and the one both readers agree on — doctor
// has no injectable clock, so a shared scenario cannot use one either.
// service also has a quieter "not running at all" path doctor has no
// equivalent for; that path is deliberately not part of this shared table
// (see resolveServeUnits's own, package-local tests for it).
type Scenario struct {
	Name string
	// Report is the snapshot to publish, GeneratedUnix already offset from
	// real time.Now() by whatever this scenario needs. Zero value when Write
	// is false.
	Report unitreport.Report
	Write  bool
	// WantUnitsLen is how many units a reader must report seeing.
	WantUnitsLen int
	// WantHealthy is true only when a reader may treat the snapshot as
	// current health: no stale/missing/schema/foreign-pid detail attached.
	WantHealthy bool
	// WantDetailSubstr, when non-empty, must appear in whatever "why not
	// healthy" string the reader produces.
	WantDetailSubstr string
}

// WantUnits is the exact slice both readers must produce, not merely a
// length: WantUnitsLen alone would pass a reader that returns the right
// COUNT of units with the wrong content (e.g. a stale copy that happened to
// keep the same length). Zero units always means the empty (non-nil) slice;
// otherwise it is the scenario's own published Report.Units, unmodified.
func (sc Scenario) WantUnits() []unitreport.Unit {
	if sc.WantUnitsLen == 0 {
		return []unitreport.Unit{}
	}
	return sc.Report.Units
}

func baseReport(generated time.Time) unitreport.Report {
	return unitreport.Report{
		SchemaVersion: unitreport.SchemaVersion, PID: PID, GeneratedUnix: generated.Unix(),
		Units: []unitreport.Unit{{Name: "memory", Kind: "memory", Identity: "abc",
			State: "running", PID: PID + 1, HealthOK: true, Generation: 2, Restarts: 1, LastProbeUS: 900}},
	}
}

// Scenarios returns the shared table, built against the real wall clock at
// call time (doctor's reader has no injectable clock, so neither does this
// fixture). Call it immediately before writing/reading the snapshot.
func Scenarios() []Scenario {
	now := time.Now()
	fresh := baseReport(now)

	stale := baseReport(now.Add(-2 * time.Minute))

	future := fresh
	future.SchemaVersion = unitreport.SchemaVersion + 1

	zero := unitreport.Report{
		SchemaVersion: unitreport.SchemaVersion, PID: PID, GeneratedUnix: now.Unix(),
		Units: []unitreport.Unit{},
	}

	return []Scenario{
		{
			Name: "fresh snapshot from the live pid", Report: fresh, Write: true,
			WantUnitsLen: 1, WantHealthy: true,
		},
		{
			Name: "fresh snapshot with zero supervised units is healthy, not unknown",
			Report: zero, Write: true,
			WantUnitsLen: 0, WantHealthy: true,
		},
		{
			Name: "no snapshot on disk at all, daemon running", Write: false,
			WantUnitsLen: 0, WantHealthy: false, WantDetailSubstr: "no supervision snapshot",
		},
		{
			// A stale snapshot is refused the same as an unreadable, missing,
			// or schema-mismatched one: its units must NOT render as current
			// rows next to an "unknown"/unavailable verdict.
			Name: "snapshot aged past the stale budget hides its units, not just flags them",
			Report: stale, Write: true,
			WantUnitsLen: 0, WantHealthy: false, WantDetailSubstr: "stale",
		},
		{
			Name: "snapshot on a schema this build does not read",
			Report: future, Write: true,
			WantUnitsLen: 0, WantHealthy: false, WantDetailSubstr: "schema",
		},
	}
}
