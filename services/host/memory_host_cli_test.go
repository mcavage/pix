package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildHostBinary compiles pix-host to a temp binary so we can exercise the
// real process exit codes (os.Exit is not observable in-process).
func buildHostBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "pix-host")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go build pix-host failed: %v\n%s", err, out)
	}
	return bin
}

// TestMemoryHostUnknownSubExits2 is the gate: `pix-host memory bogus` must
// print usage and exit 2, NOT fall through to starting the daemon (which would
// block forever on ListenAndServe).
func TestMemoryHostUnknownSubExits2(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the real pix-host binary for an exit-code roundtrip; covered by the untimed race/metrics CI jobs")
	}
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

// TestMemoryHostHelpDescribesSnapshotRestoreOnly is a REAL BINARY test (not a
// string check against the const, which could pass while the shipped process
// prints something else — e.g. a build tag or an earlier `-h` branch
// short-circuiting first): `pix-host memory --help` and `-h` must describe
// snapshot/restore as memory.db-only primitives and must never resurrect the
// retired multi-component archive's claim of covering config or op-refs too.
func TestMemoryHostHelpDescribesSnapshotRestoreOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the real pix-host binary for a help-text roundtrip; covered by the untimed race/metrics CI jobs")
	}
	bin := buildHostBinary(t)
	for _, flag := range []string{"--help", "-h"} {
		cmd := exec.Command(bin, "memory", flag)
		cmd.Env = append(os.Environ(), "MEMORY_DB="+filepath.Join(t.TempDir(), "memory.db"))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("pix-host memory %s: %v\n%s", flag, err, out)
		}
		text := string(out)
		for _, want := range []string{"snapshot PATH", "restore PATH", "STOPPED"} {
			if !strings.Contains(text, want) {
				t.Errorf("pix-host memory %s missing %q:\n%s", flag, want, text)
			}
		}
		for _, unwanted := range []string{"config.toml", "op-refs", "archive", "tar.gz"} {
			if strings.Contains(text, unwanted) {
				t.Errorf("pix-host memory %s resurrects the retired archive's %q claim:\n%s", flag, unwanted, text)
			}
		}
	}
}

// TestHostBackupRestoreRetireToMemorySnapshot is a REAL BINARY test of the
// out-of-process retirement contract (retired_host_test.go only exercises
// retiredHostMessage in-process): the actual compiled pix-host, invoked the
// way a stale script or launchd plist would, must exit 2, print a
// PIX_RETIRED marker naming the live `memory snapshot`/`memory restore`
// primitives on stderr, write nothing to stdout, and must NOT start a daemon
// or touch the filesystem under MEMORY_DB.
func TestHostBackupRestoreRetireToMemorySnapshot(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the real pix-host binary for a retirement-chain roundtrip; covered by the untimed race/metrics CI jobs")
	}
	bin := buildHostBinary(t)
	cases := map[string]string{
		"backup":  "pix-host memory snapshot PATH",
		"restore": "pix-host memory restore PATH",
	}
	for sub, replacement := range cases {
		t.Run(sub, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "memory.db")
			cmd := exec.Command(bin, sub)
			cmd.Env = append(os.Environ(), "MEMORY_DB="+dbPath)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err := cmd.Run()
			exit, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("pix-host %s: want a non-zero exit, got err=%v", sub, err)
			}
			if exit.ExitCode() != 2 {
				t.Errorf("pix-host %s exit = %d, want 2", sub, exit.ExitCode())
			}
			if !strings.HasPrefix(stderr.String(), "PIX_RETIRED") {
				t.Errorf("pix-host %s stderr = %q, want it to start with PIX_RETIRED", sub, stderr.String())
			}
			if !strings.Contains(stderr.String(), replacement) {
				t.Errorf("pix-host %s stderr = %q, want it to name %q", sub, stderr.String(), replacement)
			}
			if strings.TrimSpace(stdout.String()) != "" {
				t.Errorf("pix-host %s wrote to stdout: %q", sub, stdout.String())
			}
			if _, err := os.Stat(dbPath); err == nil {
				t.Errorf("pix-host %s created %s; a retired surface must have no side effects", sub, dbPath)
			}
		})
	}
}
