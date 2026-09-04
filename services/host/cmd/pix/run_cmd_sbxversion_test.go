package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/cli"
)

// run_cmd_sbxversion_test.go proves `pix run`'s half of the native-
// environments sbx version gate (PRD .pi-agent/deliver/native-environments/
// prd.md section 5.6, AC-20): a too-old or unparsable sbx refuses the launch,
// BEFORE any sandbox side effect, with the byte-exact section 5.6 message and
// a non-zero exit. TDD red-first: written before gateSbxVersion existed in
// run_cmd.go, so `go test ./cmd/pix/... -run Sbx` failed to compile — the
// first, honest red for this half of the unit.
//
// gateSbxVersion is exercised directly rather than through the full
// runLaunch/runCmd.Run path: runLaunch also loads config, resolves the
// workspace, and touches the sandbox lifecycle, none of which this gate
// needs to prove, and pulling all of that in would make the test depend on
// far more than the property under test.

func writeSbxFixture(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sbx")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGateSbxVersion_TooOldFailsClosedWithExactCopy(t *testing.T) {
	bin := writeSbxFixture(t, "#!/bin/sh\necho 'sbx version 0.38.2'\n")
	var errBuf bytes.Buffer
	d := &cli.Deps{Err: &errBuf}
	err := gateSbxVersion(d, bin)
	if err == nil {
		t.Fatal("want a refusal for sbx 0.38.2, got nil")
	}
	var se cli.SilentError
	if !errors.As(err, &se) || se.Code == 0 {
		t.Fatalf("want a non-zero SilentError, got %v (%T)", err, err)
	}
	want := "pix: native environments require sbx 0.39.0 or later.\n" +
		"     found: 0.38.2\n" +
		"     upgrade it: brew upgrade docker/tap/sbx\n"
	if got := errBuf.String(); got != want {
		t.Errorf("stderr =\n%q\nwant\n%q", got, want)
	}
}

func TestGateSbxVersion_UnparsableFailsClosed(t *testing.T) {
	bin := writeSbxFixture(t, "#!/bin/sh\necho 'not a version at all'\n")
	var errBuf bytes.Buffer
	d := &cli.Deps{Err: &errBuf}
	err := gateSbxVersion(d, bin)
	if err == nil {
		t.Fatal("want a refusal for unparsable output, got nil")
	}
	var se cli.SilentError
	if !errors.As(err, &se) || se.Code == 0 {
		t.Fatalf("want a non-zero SilentError, got %v (%T)", err, err)
	}
	if !strings.Contains(errBuf.String(), "     found: unknown (sbx --version was not understood)\n") {
		t.Errorf("stderr = %q, want the exact unparsable found line", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "     upgrade it: brew upgrade docker/tap/sbx\n") {
		t.Errorf("stderr = %q, want the upgrade line", errBuf.String())
	}
}

func TestGateSbxVersion_AcceptedVersionsProceedSilently(t *testing.T) {
	for _, v := range []string{"0.39.0", "0.40.1"} {
		t.Run(v, func(t *testing.T) {
			bin := writeSbxFixture(t, "#!/bin/sh\necho 'sbx version "+v+"'\n")
			var errBuf bytes.Buffer
			d := &cli.Deps{Err: &errBuf}
			if err := gateSbxVersion(d, bin); err != nil {
				t.Fatalf("want no refusal for sbx %s, got %v", v, err)
			}
			if errBuf.Len() != 0 {
				t.Errorf("stderr = %q, want silence when the gate does not fire", errBuf.String())
			}
		})
	}
}

// TestGateSbxVersion_MissingSbxDoesNotBlock: a missing sbx is a DIFFERENT,
// already-handled gap (unloadedLocalImage / the "exec sbx" failure further
// down runLaunch, both naming doctor.SbxInstallHint) — this gate must never
// turn "sbx is not installed" into a version refusal.
func TestGateSbxVersion_MissingSbxDoesNotBlock(t *testing.T) {
	var errBuf bytes.Buffer
	d := &cli.Deps{Err: &errBuf}
	if err := gateSbxVersion(d, filepath.Join(t.TempDir(), "no-such-sbx")); err != nil {
		t.Fatalf("a missing sbx binary is a different, already-handled gap: %v", err)
	}
	if errBuf.Len() != 0 {
		t.Errorf("stderr = %q, want silence: this gate is not the missing-binary remedy", errBuf.String())
	}
}

// TestGateSbxVersion_DeniedDoesNotBlock: a policy refusal is its own honest
// verdict, not a version problem.
func TestGateSbxVersion_DeniedDoesNotBlock(t *testing.T) {
	bin := writeSbxFixture(t, "#!/bin/sh\necho 'operation not permitted: blocked by organization policy' 1>&2\nexit 1\n")
	var errBuf bytes.Buffer
	d := &cli.Deps{Err: &errBuf}
	if err := gateSbxVersion(d, bin); err != nil {
		t.Fatalf("a denied sbx probe must not trip the version gate: %v", err)
	}
}
