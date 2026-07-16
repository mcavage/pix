package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestBackupHelp proves `pi-stack backup --help` prints usage without execing
// the host binary (config-independent, no side effects). The exec path itself is
// exercised end-to-end by the host-side backup tests.
func TestBackupHelp(t *testing.T) {
	var out bytes.Buffer
	if err := runBackupCore([]string{"--help"}, &out); err != nil {
		t.Fatalf("backup --help: %v", err)
	}
	if !strings.Contains(out.String(), "usage: pi-stack backup") {
		t.Errorf("help output = %q, want it to mention 'usage: pi-stack backup'", out.String())
	}
}

// TestBackupRejectsPositional proves an unexpected positional is a usage error
// before any exec.
func TestBackupRejectsPositional(t *testing.T) {
	if err := runBackupCore([]string{"extra"}, &bytes.Buffer{}); !isUsage(err) {
		t.Errorf("backup with a positional: err = %v, want usageError", err)
	}
}

// TestRestoreHelp proves `pi-stack restore --help` prints usage without execing
// the host binary.
func TestRestoreHelp(t *testing.T) {
	var out bytes.Buffer
	if err := runRestoreCore([]string{"--help"}, &out); err != nil {
		t.Fatalf("restore --help: %v", err)
	}
	if !strings.Contains(out.String(), "usage: pi-stack restore") {
		t.Errorf("help output = %q, want it to mention 'usage: pi-stack restore'", out.String())
	}
}

// TestRestoreNeedsArchive proves the launcher rejects a restore with no
// <archive> as a usage error before any exec.
func TestRestoreNeedsArchive(t *testing.T) {
	if err := runRestoreCore(nil, &bytes.Buffer{}); !isUsage(err) {
		t.Errorf("restore with no archive: err = %v, want usageError", err)
	}
}

// TestBackupRestoreVerbUsage proves the top-level help routing knows the new
// verbs.
func TestBackupRestoreVerbUsage(t *testing.T) {
	for _, v := range []string{"backup", "restore"} {
		u, ok := verbUsage(v)
		if !ok {
			t.Errorf("verbUsage(%q) not found", v)
		}
		if !strings.Contains(u, "usage: pi-stack "+v) {
			t.Errorf("verbUsage(%q) = %q, want it to start with the verb usage", v, u)
		}
	}
}
