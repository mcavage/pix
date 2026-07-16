package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// buildHostBinary compiles pi-stack-host to a temp binary so we can exercise the
// real process exit codes (os.Exit is not observable in-process).
func buildHostBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "pi-stack-host")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go build pi-stack-host failed: %v\n%s", err, out)
	}
	return bin
}

// TestMemoryHostUnknownSubExits2 is the gate: `pi-stack-host memory bogus` must
// print usage and exit 2, NOT fall through to starting the daemon (which would
// block forever on ListenAndServe).
func TestMemoryHostUnknownSubExits2(t *testing.T) {
	bin := buildHostBinary(t)
	cmd := exec.Command(bin, "memory", "bogus")
	// Point MEMORY_DB at a temp path so even a mistaken daemon start wouldn't
	// touch a real store; the process must exit 2 before that matters.
	cmd.Env = append(os.Environ(), "MEMORY_DB="+filepath.Join(t.TempDir(), "memory.db"))
	out, err := cmd.CombinedOutput()
	exit, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected a non-zero exit, got err=%v out=%s", err, out)
	}
	if exit.ExitCode() != 2 {
		t.Errorf("`memory bogus` exit code = %d, want 2\n%s", exit.ExitCode(), out)
	}
}
