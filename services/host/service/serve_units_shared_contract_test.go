// serve_units_shared_contract_test.go proves resolveServeUnits agrees with
// workflow/doctor's readSupervisorSnapshot (see the matching contract test
// there) on the shared unitreporttest.Scenarios table — the same on-disk
// serve.units.json, classified by two independent readers.
package service

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"pix/host/unitreport"
	"pix/host/unitreport/unitreporttest"
)

func TestResolveServeUnits_SharedContract(t *testing.T) {
	for _, sc := range unitreporttest.Scenarios() {
		t.Run(sc.Name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "serve.units.json")
			if sc.Write {
				if err := unitreport.WriteReport(path, sc.Report); err != nil {
					t.Fatalf("write snapshot: %v", err)
				}
			}
			units, detail := resolveServeUnits(path, true, unitreporttest.PID, time.Now())
			if units == nil {
				t.Error("units must never be nil — a JSON reader needs [] , not null")
			}
			if len(units) != sc.WantUnitsLen {
				t.Errorf("units = %+v, want len %d", units, sc.WantUnitsLen)
			}
			if want := sc.WantUnits(); !reflect.DeepEqual(units, want) {
				t.Errorf("units = %+v, want exactly %+v", units, want)
			}
			healthy := detail == ""
			if healthy != sc.WantHealthy {
				t.Errorf("healthy = %v (detail=%q), want %v", healthy, detail, sc.WantHealthy)
			}
			if sc.WantDetailSubstr != "" && !strings.Contains(detail, sc.WantDetailSubstr) {
				t.Errorf("detail = %q, want it to contain %q", detail, sc.WantDetailSubstr)
			}
		})
	}
}
