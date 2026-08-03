// Moved from workflow/doctor: the subject is the argv seam (runDoctorCmd /
// runStatusCmd), which owns os.Exit and lives at L4.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"pix/host/hostenv"
	"pix/host/readiness"
	"pix/host/sys/systest"
	"pix/host/workflow/doctor"
	"strings"
	"testing"
)

// TestProvidersGroup_RenderZeroConfirmed_OneTodoLine: the concise render must
// surface exactly the one fix command, in the TODO section.
func TestProvidersGroup_RenderZeroConfirmed_OneTodoLine(t *testing.T) {
	r := &readiness.Report{Groups: []readiness.Group{doctor.ProvidersGroup(nil, hostenv.Env{System: &systest.Fake{}}, "", true)}}
	var buf bytes.Buffer
	r.Render(&buf, false, doctor.Hints())
	out := buf.String()
	if strings.Count(out, "TODO: pix models add") != 1 {
		t.Errorf("expected exactly one provider TODO line, got:\n%s", out)
	}
}

// sbxSecretLsScript writes a fake `sbx` on the given dir's PATH that answers
// ONLY `secret ls` (with the given output/exit code) and no-ops everything
// else (0, empty), so a real subprocess run of runDoctorCmd can exercise the
// exact tri-state launch.SbxModelKeyState/secret.ProbeSbxSecrets share, without a real sbx
// install.

// sbxSecretLsScript writes a fake `sbx` on the given dir's PATH that answers
// ONLY `secret ls` (with the given output/exit code) and no-ops everything
// else (0, empty), so a real subprocess run of runDoctorCmd can exercise the
// exact tri-state launch.SbxModelKeyState/secret.ProbeSbxSecrets share, without a real sbx
// install.
func sbxSecretLsScript(t *testing.T, dir, output string, exitCode int) {
	t.Helper()
	body := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"secret\" ] && [ \"$2\" = \"ls\" ]; then\n  printf '%%s' '%s'\n  exit %d\nfi\nexit 0\n", output, exitCode)
	if err := os.WriteFile(filepath.Join(dir, "sbx"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestDoctorCmd_RealExitCodes exercises runDoctorCmd's ACTUAL process exit
// code end to end (real os.Exit, real defaultShellEnv, a real fake `sbx` on
// PATH) \u2014 not just the in-process report/blocking() unit tests above.

// TestDoctorCmd_RealExitCodes exercises runDoctorCmd's ACTUAL process exit
// code end to end (real os.Exit, real defaultShellEnv, a real fake `sbx` on
// PATH) \u2014 not just the in-process report/blocking() unit tests above.
func TestDoctorCmd_RealExitCodes(t *testing.T) {
	cases := []struct {
		name     string
		argv     []string
		sbxOut   string
		sbxExit  int
		noSbx    bool
		wantExit int
	}{
		{name: "one_key_present", sbxOut: "anthropic\n", sbxExit: 0, wantExit: 0},
		{name: "zero_keys_confirmed", sbxOut: "", sbxExit: 0, wantExit: 1},
		{name: "usage_error", argv: []string{"--bogus"}, wantExit: 2},
		// A failed `sbx secret ls` leaves the CORE model-key axis
		// unverifiable, which is exit 3 under the shared contract
		// (AC-P0-207): doctor no longer collapses "could not check" into 0.
		{name: "probe_failed", sbxExit: 7, wantExit: 3},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if os.Getenv("PIX_DOCTOR_HELPER") == tc.name {
				runDoctorCmd(tc.argv)
				return
			}
			dir := t.TempDir()
			if !tc.noSbx {
				sbxSecretLsScript(t, dir, tc.sbxOut, tc.sbxExit)
			}
			cmd := exec.Command(os.Args[0], "-test.run", "TestDoctorCmd_RealExitCodes/"+tc.name)
			cmd.Env = append(os.Environ(),
				"PIX_DOCTOR_HELPER="+tc.name,
				"PATH="+dir,
				"PIX_CONFIG="+filepath.Join(dir, "config.toml"), // absent file -> defaults
				"HOME="+dir,
			)
			out, err := cmd.CombinedOutput()
			exit := 0
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				exit = ee.ExitCode()
			} else if err != nil {
				t.Fatalf("unexpected run error: %v\noutput:\n%s", err, out)
			}
			if exit != tc.wantExit {
				t.Errorf("%s: exit = %d, want %d\noutput:\n%s", tc.name, exit, tc.wantExit, out)
			}
		})
	}
}
