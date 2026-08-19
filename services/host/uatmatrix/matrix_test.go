package uatmatrix_test

// matrix_test.go is the host-backed integration test for Run (matrix.go): it
// builds the REAL pix and pix-host
// binaries for the host's own platform (no cross-compile — this is a
// same-machine functional test, not the candidate_smoke docker/GOOS=darwin
// build), then runs the full matrix against them exactly the way
// executeCandidateSmoke does, asserting every check leaves a PASS artifact.
//
// This is deliberately NOT a mock-based test: HANDOFF-MEMORY-UAT.md's entire
// point is that repository unit tests are not a substitute for exercising the
// real candidate binaries, a real sqlite file, and real subprocess lifecycle.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pix/host/uatmatrix"
)

// buildMemoryMatrixBinaries compiles pix-host (services/host, package main)
// and pix (services/host/cmd/pix) for the current GOOS/GOARCH into a fresh
// temp dir, mirroring the two `go build` invocations executeCandidateSmoke
// runs inside docker, but native so this test can actually execute them.
func buildMemoryMatrixBinaries(t *testing.T) string {
	t.Helper()
	hostRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve services/host root: %v", err)
	}
	outDir := t.TempDir()

	build := func(pkgDir, out string) {
		cmd := exec.Command("go", "build", "-o", out, ".")
		cmd.Dir = pkgDir
		if out, buildErr := cmd.CombinedOutput(); buildErr != nil {
			t.Fatalf("go build (%s): %v\n%s", pkgDir, buildErr, out)
		}
	}
	build(hostRoot, filepath.Join(outDir, "pix-host"))
	build(filepath.Join(hostRoot, "cmd", "pix"), filepath.Join(outDir, "pix"))
	return outDir
}

// TestRealMemoryMatrix_FullSuite runs every memory UAT check end to end
// against real candidate binaries: cold start with zero Ollama traffic,
// repeated recall '*' with no restart/chatter, explicit-mode rejection,
// experimental-auto's 10-row/day budget with the "auto" tag rendered by the
// real CLI, external remember source spoofing normalized to unknown, v1
// schema migration (including a malformed-fixture rollback), forget-missing
// exit code and --json parseability, and a supervised plugin child restart
// that retains the listener and the row.
func TestRealMemoryMatrix_FullSuite(t *testing.T) {
	if testing.Short() {
		t.Skip("builds real pix/pix-host binaries and drives 8 real subprocess-backed memory checks; covered by the untimed CI job")
	}

	outDir := buildMemoryMatrixBinaries(t)

	runDir := t.TempDir()
	stepsDir := filepath.Join(runDir, "steps")
	if err := os.MkdirAll(stepsDir, 0700); err != nil {
		t.Fatalf("mkdir steps: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	err := uatmatrix.Run(ctx, uatmatrix.Inputs{OutDir: outDir, StepsDir: stepsDir})

	// Whether it passed or failed, every check's own bounded artifact is the
	// authoritative record — surface all of them so a failure is diagnosable
	// straight from the test log, the same way a real UAT run's steps/
	// directory would be inspected.
	entries, rerr := os.ReadDir(stepsDir)
	if rerr != nil {
		t.Fatalf("read steps dir: %v", rerr)
	}
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(stepsDir, e.Name()))
		t.Logf("--- %s ---\n%s", e.Name(), b)
	}
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	wantLogs := []string{
		"memory_cold_start_no_ollama.log",
		"memory_repeated_recall_star.log",
		"memory_stale_daemon_no_success.log",
		"memory_explicit_mode_no_watcher.log",
		"memory_experimental_auto_budget.log",
		"memory_remember_source_watcher_spoof.log",
		"memory_v1_migration.log",
		"memory_forget_missing_exit1.log",
		"memory_plugin_restart_retains_row.log",
	}
	for _, name := range wantLogs {
		b, rerr := os.ReadFile(filepath.Join(stepsDir, name))
		if rerr != nil {
			t.Errorf("missing bounded artifact %s: %v", name, rerr)
			continue
		}
		if !strings.Contains(string(b), "RESULT: PASS") {
			t.Errorf("%s does not record a PASS result", name)
		}
	}
}

// TestRealMemoryMatrix_MissingBinaryFailsClosed proves Run's fail-closed guard:
// a candidate directory with no real pix/pix-host
// binaries must return an explicit error, never silently skip the matrix.
func TestRealMemoryMatrix_MissingBinaryFailsClosed(t *testing.T) {
	empty := t.TempDir()
	stepsDir := filepath.Join(t.TempDir(), "steps")
	if err := os.MkdirAll(stepsDir, 0700); err != nil {
		t.Fatal(err)
	}
	err := uatmatrix.Run(context.Background(), uatmatrix.Inputs{OutDir: empty, StepsDir: stepsDir})
	if err == nil {
		t.Fatal("expected an error for a candidate dir with no real binaries, got nil")
	}
	if !strings.Contains(err.Error(), "candidate binary missing") {
		t.Fatalf("expected a 'candidate binary missing' error, got: %v", err)
	}
}
