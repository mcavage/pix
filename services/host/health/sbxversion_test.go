package health

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sbxversion_test.go proves the native-environments minimum-version gate this
// unit adds ON TOP of SbxProbe's own classification (PRD
// .pi-agent/deliver/native-environments/prd.md section 5.6, AC-20): sbx must
// be SbxMinVersion or later, and a version SbxProbe could not read AT ALL is
// refused exactly like a too-old one. TDD red-first: written before
// SbxMinVersion/SbxUpgradeFix/SbxVersionGate/SbxVersionGateMessage exist, so
// `go test ./health/...` fails to COMPILE — the first, honest red.
//
// The gate is deliberately layered ON TOP of, not folded into, SbxProbe's
// existing Result: a broken/timed-out/denied sbx must keep classifying
// exactly as it always has (TestSbxProbe_RealExecutableOutcomes and its
// siblings in probes_test.go are the oracle for that), so SbxVersionGate reads
// an ALREADY-CLASSIFIED Result rather than reinterpreting the exec outcome
// itself.

// TestSbxProbe_VersionRequirementTable is the required version table:
// 0.38.2 refused, 0.39.0 and 0.40.1 allowed, empty/garbage fail closed.
func TestSbxProbe_VersionRequirementTable(t *testing.T) {
	cases := []struct {
		name    string
		script  string // the fixture sbx's stdout-producing shell body
		want    Status
		blocked bool
		found   string
	}{
		{"too old", "#!/bin/sh\necho 'sbx version 0.38.2'\n", StatusAbsent, true, "0.38.2"},
		{"exactly the minimum", "#!/bin/sh\necho 'sbx version 0.39.0'\n", StatusReady, false, "0.39.0"},
		{"newer patch", "#!/bin/sh\necho 'sbx version 0.40.1'\n", StatusReady, false, "0.40.1"},
		{"empty output", "#!/bin/sh\ntrue\n", StatusUnknown, true, "unknown (sbx --version was not understood)"},
		{"garbage output", "#!/bin/sh\necho 'not a version at all'\n", StatusUnknown, true, "unknown (sbx --version was not understood)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bin := writeScript(t, "sbx", tc.script)
			r := check(t, SbxProbe{Bin: bin}, 5*time.Second)
			wantStatus(t, r, tc.want)
			blocked, found := SbxVersionGate(r)
			if blocked != tc.blocked {
				t.Fatalf("SbxVersionGate blocked = %v, want %v (result=%+v)", blocked, tc.blocked, r)
			}
			if found != tc.found {
				t.Errorf("found = %q, want %q", found, tc.found)
			}
		})
	}
}

// TestSbxVersionGate_MissingOrDeniedNeverBlocks: a version problem is a
// DIFFERENT, honest gap from "could not check at all" — SbxInstallFix already
// names the missing-binary remedy, and a policy refusal is its own verdict.
// Reinterpreting either as a version refusal would be exactly the dishonesty
// health.go's package doc bans.
func TestSbxVersionGate_MissingOrDeniedNeverBlocks(t *testing.T) {
	missing := check(t, SbxProbe{Bin: filepath.Join(t.TempDir(), "definitely-not-here")}, 5*time.Second)
	wantStatus(t, missing, StatusAbsent)
	if blocked, found := SbxVersionGate(missing); blocked {
		t.Errorf("a missing sbx binary must not trip the version gate (found=%q)", found)
	}

	deniedBin := writeScript(t, "sbx-denied", "#!/bin/sh\necho 'operation not permitted: blocked by organization policy' 1>&2\nexit 1\n")
	denied := check(t, SbxProbe{Bin: deniedBin}, 5*time.Second)
	wantStatus(t, denied, StatusDenied)
	if blocked, found := SbxVersionGate(denied); blocked {
		t.Errorf("a denied sbx probe must not trip the version gate (found=%q)", found)
	}

	timedOut := check(t, SbxProbe{Bin: writeScript(t, "sbx-hang", "#!/bin/sh\nsleep 5\n")}, 100*time.Millisecond)
	wantStatus(t, timedOut, StatusUnknown)
	if blocked, found := SbxVersionGate(timedOut); blocked {
		t.Errorf("a timed-out sbx probe must not trip the version gate (found=%q)", found)
	}
}

// TestSbxVersionGateMessage_ExactCopy pins the byte-exact PRD section 5.6
// string, including the five-space continuation indent, so `pix run` and
// `pix doctor` cannot drift onto slightly different wording for the same
// requirement.
func TestSbxVersionGateMessage_ExactCopy(t *testing.T) {
	want := "pix: native environments require sbx 0.39.0 or later.\n" +
		"     found: 0.38.2\n" +
		"     upgrade it: brew upgrade docker/tap/sbx\n"
	if got := SbxVersionGateMessage("0.38.2"); got != want {
		t.Errorf("message =\n%q\nwant\n%q", got, want)
	}
}

// TestSbxVersionGateMessage_Unparsable pins the documented unparsable
// found-line copy: "found: unknown (sbx --version was not understood)".
func TestSbxVersionGateMessage_Unparsable(t *testing.T) {
	got := SbxVersionGateMessage("unknown (sbx --version was not understood)")
	if !strings.Contains(got, "     found: unknown (sbx --version was not understood)\n") {
		t.Errorf("message = %q, want the exact unparsable found line", got)
	}
	if !strings.Contains(got, "     upgrade it: brew upgrade docker/tap/sbx\n") {
		t.Errorf("message = %q, want the upgrade line", got)
	}
}

// TestSbxMinVersion_IsTheDocumentedFloor guards against the constant silently
// drifting from the one PRD names.
func TestSbxMinVersion_IsTheDocumentedFloor(t *testing.T) {
	if SbxMinVersion != "0.39.0" {
		t.Errorf("SbxMinVersion = %q, want %q", SbxMinVersion, "0.39.0")
	}
}
