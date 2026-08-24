package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/health"
)

// doctor_sbxversion_test.go proves `pix doctor`'s half of the native-
// environments sbx version gate (PRD .pi-agent/deliver/native-environments/
// prd.md section 5.6, AC-20): a too-old or unparsable sbx fails RunDoctor
// closed with the byte-exact section 5.6 copy, while a healthy host on an
// accepted version stays green. TDD red-first: written before doctor.go
// consulted health.SbxVersionGate, so RunDoctor's exit code and output did
// not yet reflect the gate — the first, honest red for this half of the
// unit.

// sbxFixture writes a tiny shell script standing in for `sbx`, printing
// whatever version banner the test needs — no compiled Go fixture required,
// since this gate only cares about stdout content, not exec classification
// (probes_test.go and doctor_test.go's compiled fixture already own that).
func sbxFixture(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sbx")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunDoctor_TooOldSbxFailsClosedWithExactCopy(t *testing.T) {
	cfg, o := healthyHost(t)
	o.SbxBin = sbxFixture(t, "#!/bin/sh\necho 'sbx version 0.38.2'\n")
	o.SbxArgs = nil
	var out strings.Builder
	code := RunDoctor(context.Background(), cfg, "default", &out, o, false, false)
	if code != health.ExitNotReady {
		t.Fatalf("exit = %d, want %d (%s)", code, health.ExitNotReady, out.String())
	}
	want := "pix: native environments require sbx 0.39.0 or later.\n" +
		"     found: 0.38.2\n" +
		"     upgrade it: brew upgrade docker/tap/sbx\n"
	if !strings.Contains(out.String(), want) {
		t.Errorf("doctor output =\n%s\nmust contain the exact section 5.6 copy:\n%s", out.String(), want)
	}
}

func TestRunDoctor_UnparsableSbxFailsClosed(t *testing.T) {
	cfg, o := healthyHost(t)
	o.SbxBin = sbxFixture(t, "#!/bin/sh\necho 'not a version at all'\n")
	o.SbxArgs = nil
	var out strings.Builder
	code := RunDoctor(context.Background(), cfg, "default", &out, o, false, false)
	if code != health.ExitNotReady {
		t.Fatalf("exit = %d, want %d (%s)", code, health.ExitNotReady, out.String())
	}
	if !strings.Contains(out.String(), "     found: unknown (sbx --version was not understood)\n") {
		t.Errorf("doctor output = %s, want the exact unparsable found line", out.String())
	}
	if !strings.Contains(out.String(), "     upgrade it: brew upgrade docker/tap/sbx\n") {
		t.Errorf("doctor output = %s, want the upgrade line", out.String())
	}
}

func TestRunDoctor_GoodVersionsAreNotBlocked(t *testing.T) {
	for _, v := range []string{"0.39.0", "0.40.1"} {
		t.Run(v, func(t *testing.T) {
			cfg, o := healthyHost(t)
			o.SbxBin = sbxFixture(t, "#!/bin/sh\necho 'sbx version "+v+"'\n")
			o.SbxArgs = nil
			var out strings.Builder
			code := RunDoctor(context.Background(), cfg, "default", &out, o, false, false)
			if code != health.ExitOK {
				t.Fatalf("exit = %d, want %d for sbx %s:\n%s", code, health.ExitOK, v, out.String())
			}
			if strings.Contains(out.String(), "native environments require sbx") {
				t.Errorf("output = %s, the gate must not fire for an accepted version", out.String())
			}
		})
	}
}

// TestRunDoctor_JSONModeStillFailsClosed: --json must not print the plain-
// prose gate line (it would corrupt the JSON stream), but the exit code must
// still reflect the gate exactly like the text mode does.
func TestRunDoctor_JSONModeStillFailsClosed(t *testing.T) {
	cfg, o := healthyHost(t)
	o.SbxBin = sbxFixture(t, "#!/bin/sh\necho 'sbx version 0.38.2'\n")
	o.SbxArgs = nil
	var out strings.Builder
	code := RunDoctor(context.Background(), cfg, "default", &out, o, true, false)
	if code != health.ExitNotReady {
		t.Fatalf("exit = %d, want %d", code, health.ExitNotReady)
	}
	if strings.Contains(out.String(), "native environments require sbx") {
		t.Errorf("--json output must stay pure JSON, got %s", out.String())
	}
}
