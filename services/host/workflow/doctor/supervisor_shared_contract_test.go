// supervisor_shared_contract_test.go proves readSupervisorSnapshot agrees
// with service.resolveServeUnits (see the matching contract test there) on
// the shared unitreporttest.Scenarios table — the same on-disk
// serve.units.json, classified by two independent readers.
package doctor

import (
	"path/filepath"
	"strings"
	"testing"

	"pix/host/unitreport"
	"pix/host/unitreport/unitreporttest"
)

func TestReadSupervisorSnapshot_SharedContract(t *testing.T) {
	for _, sc := range unitreporttest.Scenarios() {
		t.Run(sc.Name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("XDG_STATE_HOME", dir)
			if sc.Write {
				if err := unitreport.WriteReport(filepath.Join(dir, "pix", "serve.units.json"), sc.Report); err != nil {
					t.Fatalf("write snapshot: %v", err)
				}
			}
			got := readSupervisorSnapshot()
			if got.Units == nil {
				t.Error("units must never be nil — a JSON reader needs [] , not null")
			}
			if len(got.Units) != sc.WantUnitsLen {
				t.Errorf("units = %+v, want len %d", got.Units, sc.WantUnitsLen)
			}
			if got.Available != sc.WantHealthy {
				t.Errorf("available = %v (detail=%q), want %v", got.Available, got.Detail, sc.WantHealthy)
			}
			if sc.WantDetailSubstr != "" && !strings.Contains(got.Detail, sc.WantDetailSubstr) {
				t.Errorf("detail = %q, want it to contain %q", got.Detail, sc.WantDetailSubstr)
			}
		})
	}
}
