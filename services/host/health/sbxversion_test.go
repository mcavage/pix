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

// TestSbxProbe_VersionRequirementTable is the required version table.
// Beyond the original 0.38.2/0.39.0/0.40.1/empty/garbage cases, it pins the
// honest parser this file's package doc describes (see parseSbxVersion in
// probes.go): partial and extra-component versions are deliberate reads
// ("0.39", "0.39.0.1"), a "v" prefix and the real observed colon-labeled
// banner both parse, a prerelease at the minimum fails closed while a tagged
// build whose numeric core is newer than the minimum is accepted for development,
// chatty non-version text anchored elsewhere in the output
// (a Go build banner) never wins over the actual "sbx version" answer, and
// genuinely AMBIGUOUS output (two disagreeing version answers) fails closed
// exactly like no version at all — the low finding this table now proves
// fixed: a bare "first dotted numeric substring" scan used to read "built
// with go 1.21.5, sbx version 0.38.2" as "1.21.5" and fail OPEN on a
// too-old sbx.
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
		{"older patch, two-digit", "#!/bin/sh\necho 'sbx version 0.38.10'\n", StatusAbsent, true, "0.38.10"},
		{"partial version reads as its own .0", "#!/bin/sh\necho 'sbx version 0.39'\n", StatusReady, false, "0.39"},
		{"major bump", "#!/bin/sh\necho 'sbx version 1.0.0'\n", StatusReady, false, "1.0.0"},
		{"v-prefixed, colon-labeled real banner", "#!/bin/sh\necho 'sbx version: v0.39.0 def8cb0523a77e757bdd6ef52b459fe374f3783e'\n", StatusReady, false, "0.39.0"},
		{"extra trailing component is deliberately at-least, not rejected", "#!/bin/sh\necho 'sbx version 0.39.0.1'\n", StatusReady, false, "0.39.0.1"},
		{"newer major prerelease is accepted", "#!/bin/sh\necho 'sbx version 1.0.0-rc1'\n", StatusReady, false, "1.0.0-rc1"},
		{"prerelease at the minimum fails closed", "#!/bin/sh\necho 'sbx version 0.39.0rc1'\n", StatusAbsent, true, "0.39.0rc1"},
		{"newer minor prerelease is accepted", "#!/bin/sh\necho 'sbx version 0.40.0-rc'\n", StatusReady, false, "0.40.0-rc"},
		{"installed 0.41 release candidate is accepted", "#!/bin/sh\necho 'sbx version 0.41.0-rc1'\n", StatusReady, false, "0.41.0-rc1"},
		{"chatty Go banner never wins over the real sbx version", "#!/bin/sh\necho 'built with go 1.21.5, sbx version 0.38.2'\n", StatusAbsent, true, "0.38.2"},
		{"multiple disagreeing version numbers is ambiguous, not a guess",
			"#!/bin/sh\necho 'sbx version 0.38.2 (client)'\necho 'sbx version 0.40.1 (server)'\n",
			StatusUnknown, true, "unknown (sbx --version was not understood)"},
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

func TestValidateSbxVersionOutput_TaggedVersionPolicy(t *testing.T) {
	for _, accepted := range []string{"sbx version 0.40.0-rc1", "sbx version 0.41.0-rc1", "sbx version 1.0.0-beta"} {
		if err := ValidateSbxVersionOutput(accepted); err != nil {
			t.Errorf("ValidateSbxVersionOutput(%q) = %v, want accepted", accepted, err)
		}
	}
	for _, refused := range []string{"sbx version 0.38.9-rc1", "sbx version 0.39.0-rc1"} {
		if err := ValidateSbxVersionOutput(refused); err == nil {
			t.Errorf("ValidateSbxVersionOutput(%q) = nil, want refusal", refused)
		}
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
