package backup

import (
	"bytes"
	"pix/host/cli"
	"strings"
	"testing"
)

// TestBackupHelp proves `pix backup --help` prints usage without execing
// the host binary (config-independent, no side effects). The exec path itself is
// exercised end-to-end by the host-side backup tests.
func TestBackupHelp(t *testing.T) {
	var out bytes.Buffer
	if err := runBackupCore([]string{"--help"}, &out); err != nil {
		t.Fatalf("backup --help: %v", err)
	}
	if !strings.Contains(out.String(), "usage: pix backup") {
		t.Errorf("help output = %q, want it to mention 'usage: pix backup'", out.String())
	}
}

// TestBackupRejectsPositional proves an unexpected positional is a usage error
// before any exec.
func TestBackupRejectsPositional(t *testing.T) {
	if err := runBackupCore([]string{"extra"}, &bytes.Buffer{}); !cli.IsUsage(err) {
		t.Errorf("backup with a positional: err = %v, want cli.UsageError2", err)
	}
}

// TestRestoreHelp proves `pix restore --help` prints usage without execing
// the host binary.
func TestRestoreHelp(t *testing.T) {
	var out bytes.Buffer
	if err := runRestoreCore([]string{"--help"}, &out); err != nil {
		t.Fatalf("restore --help: %v", err)
	}
	if !strings.Contains(out.String(), "usage: pix restore") {
		t.Errorf("help output = %q, want it to mention 'usage: pix restore'", out.String())
	}
}

// TestRestoreNeedsArchive proves the launcher rejects a restore with no
// <archive> as a usage error before any exec.
func TestRestoreNeedsArchive(t *testing.T) {
	if err := runRestoreCore(nil, &bytes.Buffer{}); !cli.IsUsage(err) {
		t.Errorf("restore with no archive: err = %v, want cli.UsageError2", err)
	}
}
