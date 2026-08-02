// backup_usage_test.go — `pix backup`/`pix restore` must appear in the
// top-level verb usage table, which is a cmd/pix fact.
package main

import (
	"strings"
	"testing"
)

// TestBackupRestoreVerbUsage proves the top-level help routing knows the new
// verbs.
func TestBackupRestoreVerbUsage(t *testing.T) {
	for _, v := range []string{"backup", "restore"} {
		u, ok := verbUsage(v)
		if !ok {
			t.Errorf("verbUsage(%q) not found", v)
		}
		if !strings.Contains(u, "usage: pix "+v) {
			t.Errorf("verbUsage(%q) = %q, want it to start with the verb usage", v, u)
		}
	}
}
