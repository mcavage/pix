// serve_units_test.go — `pix serve status` must never render an absent, stale
// or foreign supervision snapshot as health. Each case below is a way the
// snapshot can lie, and the assertion is that status says so out loud.
package service

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pix/host/unitreport"
)

func writeSnap(t *testing.T, rep unitreport.Report) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "serve.units.json")
	if err := unitreport.WriteReport(p, rep); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	return p
}

func freshRep(now time.Time, pid int) unitreport.Report {
	return unitreport.Report{
		SchemaVersion: unitreport.SchemaVersion, PID: pid, GeneratedUnix: now.Unix(),
		Units: []unitreport.Unit{{Name: "memory", Kind: "memory", Identity: "abc",
			State: "running", PID: pid + 1, HealthOK: true, Generation: 2, Restarts: 1, LastProbeUS: 900}},
	}
}

func TestResolveServeUnits(t *testing.T) {
	now := time.Unix(1700000000, 0)
	t.Run("fresh snapshot from the running pid is trusted", func(t *testing.T) {
		units, detail := resolveServeUnits(writeSnap(t, freshRep(now, 100)), true, 100, now)
		if detail != "" || len(units) != 1 || units[0].Name != "memory" || units[0].Restarts != 1 {
			t.Fatalf("units=%+v detail=%q", units, detail)
		}
	})
	t.Run("not running and no snapshot is quiet", func(t *testing.T) {
		units, detail := resolveServeUnits(filepath.Join(t.TempDir(), "none.json"), false, 0, now)
		if len(units) != 0 || detail != "" {
			t.Fatalf("units=%+v detail=%q", units, detail)
		}
	})
	t.Run("running with no snapshot is reported, not assumed healthy", func(t *testing.T) {
		_, detail := resolveServeUnits(filepath.Join(t.TempDir(), "none.json"), true, 100, now)
		if !strings.Contains(detail, "no supervision snapshot") {
			t.Fatalf("detail=%q", detail)
		}
	})
	t.Run("snapshot without a running serve is stale", func(t *testing.T) {
		units, detail := resolveServeUnits(writeSnap(t, freshRep(now, 100)), false, 0, now)
		if len(units) != 0 || !strings.Contains(detail, "stale") {
			t.Fatalf("units=%+v detail=%q", units, detail)
		}
	})
	t.Run("snapshot from another pid is refused", func(t *testing.T) {
		units, detail := resolveServeUnits(writeSnap(t, freshRep(now, 100)), true, 999, now)
		if len(units) != 0 || !strings.Contains(detail, "belongs to pid 100") {
			t.Fatalf("units=%+v detail=%q", units, detail)
		}
	})
	t.Run("schema mismatch is refused", func(t *testing.T) {
		r := freshRep(now, 100)
		r.SchemaVersion = unitreport.SchemaVersion + 1
		units, detail := resolveServeUnits(writeSnap(t, r), true, 100, now)
		if len(units) != 0 || !strings.Contains(detail, "schema") {
			t.Fatalf("units=%+v detail=%q", units, detail)
		}
	})
	t.Run("an old snapshot still shows units but is flagged stale", func(t *testing.T) {
		units, detail := resolveServeUnits(writeSnap(t, freshRep(now, 100)), true, 100, now.Add(2*time.Minute))
		if len(units) != 1 || !strings.Contains(detail, "stale") {
			t.Fatalf("units=%+v detail=%q", units, detail)
		}
	})
}

func TestPrintServeStatusRendersUnitsAndJSON(t *testing.T) {
	st := serveState{Running: true, PID: 100, Memory: true, MemoryPort: 11435,
		Units: freshRep(time.Now(), 100).Units}
	st.Units[0].LastError = "unit memory: 1 failed probe"

	var human bytes.Buffer
	printServeStatus(st, &human, false)
	for _, want := range []string{"unit memory (memory): running", "gen=2", "restarts=1", "probe=900us", "last error: unit memory: 1 failed probe"} {
		if !strings.Contains(human.String(), want) {
			t.Errorf("human status is missing %q:\n%s", want, human.String())
		}
	}

	var js bytes.Buffer
	printServeStatus(st, &js, true)
	var got map[string]any
	if err := json.Unmarshal(js.Bytes(), &got); err != nil {
		t.Fatalf("status --json is not json: %v", err)
	}
	units, _ := got["units"].([]any)
	if len(units) != 1 {
		t.Fatalf("units missing from status --json: %s", js.String())
	}
	u, _ := units[0].(map[string]any)
	for _, k := range []string{"name", "kind", "identity", "state", "health_ok", "reattached", "restarts", "generation", "last_probe_us", "last_error"} {
		if _, ok := u[k]; !ok {
			t.Errorf("status --json unit is missing %q: %v", k, u)
		}
	}

	// Unknown must be visible in BOTH renderings.
	unknown := serveState{Running: true, PID: 100, MemoryPort: 11435, UnitsDetail: "no supervision snapshot (serve not running?)"}
	var h2 bytes.Buffer
	printServeStatus(unknown, &h2, false)
	if !strings.Contains(h2.String(), "units: unknown") {
		t.Errorf("an unseen tree must render as unknown:\n%s", h2.String())
	}
}

// TestPrintServeStatusJSONAlwaysHasUnitsFields is the JSON half of "units:
// unknown" above: a machine reader gets no human sentence to fall back on, so
// `units` and `units_detail` must always be keys in the object — `units` an
// empty array (never absent, never null) whenever there is nothing to show,
// with `units_detail` explaining why when that emptiness means something.
func TestPrintServeStatusJSONAlwaysHasUnitsFields(t *testing.T) {
	cases := []struct {
		name string
		st   serveState
	}{
		{"zero value", serveState{}},
		{"running with no supervised units", serveState{Running: true, PID: 100}},
		{"running with an unreadable snapshot", serveState{Running: true, PID: 100, UnitsDetail: "no supervision snapshot (serve not running?)"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			printServeStatus(tc.st, &buf, true)
			var got map[string]any
			if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
				t.Fatalf("status --json is not json: %v", err)
			}
			raw, ok := got["units"]
			if !ok {
				t.Fatalf("units key missing entirely: %s", buf.String())
			}
			units, ok := raw.([]any)
			if !ok {
				t.Fatalf("units is not a JSON array (got %T, probably null): %s", raw, buf.String())
			}
			if len(units) != 0 {
				t.Fatalf("units should be empty here: %v", units)
			}
			if _, ok := got["units_detail"]; !ok {
				t.Fatalf("units_detail key missing entirely: %s", buf.String())
			}
		})
	}
}
