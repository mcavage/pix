package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// run_trust_zerofootprint_test.go is the user-reported launch bug, at the
// real command boundary: the environment `pix setup` generates has a bill
// of materials with ZERO host-execution counts, and `pix run` still stopped
// to ask "Accept this host-execution footprint?" — a consent screen with
// nothing on it, which on a non-interactive terminal was an outright
// refusal nobody could satisfy except by accepting an empty bill.
//
// BillOfMaterials.Tier1() is the canonical answer and it was already false
// for this environment; these tests pin every caller to it, and pin that a
// REAL host-execution fact still prompts and still refuses.

// tier1TestHome is trustTestHome's counterpart: an environment carrying one
// real host-affecting fact (an authored additionalWorkspaces mount, which
// docs/design/environments.md §9.1 names as "expand mounted host access"),
// so its BOM is Tier1 without depending on what is on PATH.
func tier1TestHome(t *testing.T, envName string) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "pixhome")
	envDir := filepath.Join(home, "envs", envName)
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mount := t.TempDir()
	doc := "schemaVersion: \"1\"\nagent: pix\nadditionalWorkspaces:\n  - path: " + mount + "\n"
	if err := os.WriteFile(filepath.Join(envDir, ".sbxenv.yaml"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

// TestRunTrustGate_ZeroFootprintEnvironmentNeitherPromptsNorRefuses is the
// reported bug on the exact terminal shape a script or a piped run has:
// nothing host-executing is declared, so there is nothing to accept and the
// run must proceed past the gate without a refusal. (It still fails later,
// on the absent sbx this fixture forces — that is the proof the gate itself
// let it through rather than stopping it.)
func TestRunTrustGate_ZeroFootprintEnvironmentNeitherPromptsNorRefuses(t *testing.T) {
	home := trustTestHome(t, "default")
	d, out, errb := trustGateDeps(t, home)
	d.Interactive = false

	dispatch([]string{"run", t.TempDir(), "--env", "default"}, d)

	combined := out.String() + errb.String()
	if strings.Contains(combined, "has not been reviewed") ||
		strings.Contains(combined, "Accept this host-execution footprint?") ||
		strings.Contains(combined, "unreviewed environment") {
		t.Fatalf("a zero-footprint environment must neither prompt nor refuse; got %q", combined)
	}
	if _, err := os.Stat(trustRecordFile(home, "default")); err == nil {
		t.Fatal("a zero-footprint environment must never cause a trust-state write")
	}
}

// TestRunTrustGate_ZeroFootprintInteractiveDoesNotPrompt is the same fact on
// an INTERACTIVE terminal, which is where the user actually saw the prompt.
// The gate reads no answer at all: stdin stays untouched.
func TestRunTrustGate_ZeroFootprintInteractiveDoesNotPrompt(t *testing.T) {
	home := trustTestHome(t, "default")
	d, out, errb := trustGateDeps(t, home)
	d.Interactive = true
	stdin := &countingReader{s: "n\n"}
	d.In = stdin

	dispatch([]string{"run", t.TempDir(), "--env", "default"}, d)

	if stdin.reads > 0 {
		t.Fatalf("the gate read stdin for a zero-footprint environment (%d reads); it must not prompt", stdin.reads)
	}
	combined := out.String() + errb.String()
	if strings.Contains(combined, "Accept this host-execution footprint?") {
		t.Fatalf("interactive run prompted for a zero-footprint environment: %q", combined)
	}
}

// TestRunTrustGate_Tier1StillRefusesNonInteractive proves the fix did not
// widen into "never review anything": one real host-affecting fact and the
// non-interactive refusal is exactly as before.
func TestRunTrustGate_Tier1StillRefusesNonInteractive(t *testing.T) {
	home := tier1TestHome(t, "work")
	d, _, errb := trustGateDeps(t, home)
	d.Interactive = false

	code := dispatch([]string{"run", t.TempDir(), "--env", "work"}, d)
	if code == 0 {
		t.Fatalf("dispatch exit = 0, want nonzero for an unreviewed Tier1 environment (stderr=%q)", errb.String())
	}
	if !strings.Contains(errb.String(), "unreviewed environment") {
		t.Fatalf("stderr = %q, want the fail-closed refusal", errb.String())
	}
	if _, err := os.Stat(trustRecordFile(home, "work")); err == nil {
		t.Fatal("a refusal must never record trust")
	}
}

// TestRunTrustGate_Tier1StillPromptsInteractive proves the interactive
// default-No prompt still fires for a real fact, and that declining it stops
// the launch.
func TestRunTrustGate_Tier1StillPromptsInteractive(t *testing.T) {
	home := tier1TestHome(t, "work")
	d, _, errb := trustGateDeps(t, home)
	d.Interactive = true
	d.In = &countingReader{s: "n\n"}

	code := dispatch([]string{"run", t.TempDir(), "--env", "work"}, d)
	if code == 0 {
		t.Fatal("declining the prompt must stop the launch")
	}
	if !strings.Contains(errb.String(), "Accept this host-execution footprint?") {
		t.Fatalf("stderr = %q, want the default-No prompt for a Tier1 environment", errb.String())
	}
	if _, err := os.Stat(trustRecordFile(home, "work")); err == nil {
		t.Fatal("declining must never record trust")
	}
}

// TestEnvTrust_ZeroFootprintWritesNothing is `pix env trust NAME` on the
// generated default: it says plainly that there is nothing to accept, exits
// 0, and leaves no acceptance file behind.
func TestEnvTrust_ZeroFootprintWritesNothing(t *testing.T) {
	home := trustTestHome(t, "default")
	d, out, _ := trustGateDeps(t, home)
	d.Interactive = false

	if code := dispatch([]string{"env", "trust", "default"}, d); code != 0 {
		t.Fatalf("exit = %d, want 0 (stdout=%q)", code, out.String())
	}
	if !strings.Contains(out.String(), "nothing to accept") {
		t.Fatalf("stdout = %q, want the honest \"nothing to accept\" answer", out.String())
	}
	if _, err := os.Stat(trustRecordFile(home, "default")); err == nil {
		t.Fatal("`pix env trust` on a zero-footprint environment must write no trust state")
	}
}

// TestEnvList_ZeroFootprintReportsTrusted proves list/show do not label
// "nothing to review" as a pending approval.
func TestEnvList_ZeroFootprintReportsTrusted(t *testing.T) {
	home := trustTestHome(t, "default")
	d, out, _ := trustGateDeps(t, home)

	if code := dispatch([]string{"env", "show", "default", "--json"}, d); code != 0 {
		t.Fatalf("exit = %d (stdout=%q)", code, out.String())
	}
	if !strings.Contains(out.String(), "\"trusted\": true") {
		t.Fatalf("env show --json = %q, want trusted true for a zero-footprint environment", out.String())
	}
}

// countingReader is a stdin double that records whether it was read at all —
// the only way to prove a gate did not prompt, as opposed to prompting and
// getting an answer it liked.
type countingReader struct {
	s     string
	off   int
	reads int
}

func (r *countingReader) Read(p []byte) (int, error) {
	r.reads++
	if r.off >= len(r.s) {
		return 0, os.ErrClosed
	}
	n := copy(p, r.s[r.off:])
	r.off += n
	return n, nil
}
