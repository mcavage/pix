package main

import (
	"strings"
	"testing"
)

// TestMigrateHelp_ConfigIndependent: `pi-stack migrate -h/--help` prints usage
// and never execs the host binary (config-independent, no side effects). It runs
// with a bogus PATH/config so any attempt to resolve or exec would fail loudly.
func TestMigrateHelp_ConfigIndependent(t *testing.T) {
	for _, flag := range []string{"-h", "--help"} {
		out := captureStdout(t, func() { runMigrate([]string{flag}) })
		if !strings.Contains(out, "pi-stack migrate") {
			t.Errorf("migrate %s: usage missing, got %q", flag, out)
		}
		if !strings.Contains(out, "XDG") {
			t.Errorf("migrate %s: expected XDG layout description, got %q", flag, out)
		}
	}
}
