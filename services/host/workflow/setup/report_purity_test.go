// report_purity_test.go — the grep that IS the review. It reads this package's
// own source, so it lives here; in cmd/pix it read "setup_models.go" by bare
// name and broke the moment the file moved.
package setup

import (
	"os"
	"strings"
	"testing"
)

// AC-P0-302, the grep that IS the review: the render path must not read the
// inventory. A report that consults pre-mutation state is a report that can
// print what setup INTENDED rather than what it achieved, so the source itself
// is asserted — PrintSetupSummary and its helpers neither take a
// setupInventory nor name one.
func TestSetupReport_NeverReadsInventory(t *testing.T) {
	for _, file := range []string{"setup_models.go", "setup.go"} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		for _, fn := range []string{"func PrintSetupSummary(", "func SetupProvidersAxis(", "func setupPackAxis("} {
			i := strings.Index(src, fn)
			if i < 0 {
				continue
			}
			body := src[i:]
			if j := strings.Index(body, "\n}\n"); j > 0 {
				body = body[:j]
			}
			if strings.Contains(body, "setupInventory") || strings.Contains(body, "inv.") {
				t.Errorf("%s: %s reads the inventory; the report must be a pure function of post-mutation evidence", file, fn)
			}
		}
	}
}
