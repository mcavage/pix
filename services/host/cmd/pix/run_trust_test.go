package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/cli"
)

// run_trust_test.go proves the CRITICAL security re-review fix at the REAL
// command boundary: `dispatch` (the exact entry point `pix run` uses),
// never a call into gateEnvTrust or DecideEnvAttach directly. sbx is forced
// absent throughout (PATH pointed at an empty dir), the same technique
// root_test.go's bareLaunchDeps already established: this proves the
// refusal fires BEFORE any real sbx create/exec, not merely that some
// later, unrelated step also happens to fail.

// trustTestHome creates <PIX_HOME>/envs/<name>/.sbxenv.yaml (a minimal but
// completely valid environment — nothing host-exec at all, so its BOM is
// deterministic and never depends on what happens to be on PATH) and
// returns PIX_HOME.
func trustTestHome(t *testing.T, envName string) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "pixhome")
	envDir := filepath.Join(home, "envs", envName)
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := "schemaVersion: \"1\"\nagent: pix\n"
	if err := os.WriteFile(filepath.Join(envDir, ".sbxenv.yaml"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

// trustGateDeps builds the dispatch Deps for a trust-gate test: sbx forced
// ABSENT (an empty PATH — see root_test.go's bareLaunchDeps comment on why
// this must be an explicit contract rather than an accident of wherever
// `go test` happens to run) and a hermetic PIX_HOME/PIX_CONFIG, so the run
// resolves the fixture environment above, never a real machine's.
func trustGateDeps(t *testing.T, home string) (*cli.Deps, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	t.Setenv("PIX_HOME", home)
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	var out, errb bytes.Buffer
	return &cli.Deps{Out: &out, Err: &errb}, &out, &errb
}

func trustRecordFile(home, envName string) string {
	return filepath.Join(home, "state", "trust", "environments", envName+".json")
}

// TestRunTrustGate_NonInteractive_RefusesUnreviewedEnvironment is the
// CRITICAL finding itself: a first-ever `pix run --env NAME` against a
// never-reviewed environment must refuse, on a non-interactive terminal,
// before anything is created — no bill of materials on stdout (a script
// capturing it should see nothing), and no trust record written.
func TestRunTrustGate_NonInteractive_RefusesUnreviewedEnvironment(t *testing.T) {
	home := trustTestHome(t, "work")
	d, out, errb := trustGateDeps(t, home)
	d.Interactive = false

	dir := t.TempDir()
	code := dispatch([]string{"run", dir, "--env", "work"}, d)

	if code == 0 {
		t.Fatalf("dispatch exit = 0, want nonzero (stdout=%q stderr=%q)", out.String(), errb.String())
	}
	if !strings.Contains(errb.String(), "unreviewed environment") || !strings.Contains(errb.String(), "pix env trust work") {
		t.Fatalf("stderr = %q, want the fail-closed refusal naming `pix env trust work`", errb.String())
	}
	if _, err := os.Stat(trustRecordFile(home, "work")); err == nil {
		t.Fatal("a non-interactive refusal must never record trust")
	}
	// Never let any real sbx side effect message leak past this refusal —
	// proof this fired BEFORE any create/attach path, not merely that a
	// later step also failed.
	for _, leak := range []string{"attaching to running sandbox", "starting + attaching", "exec sbx:"} {
		if strings.Contains(errb.String()+out.String(), leak) {
			t.Errorf("output mentions %q — the trust gate did not fire first", leak)
		}
	}
}

// TestRunTrustGate_Interactive_DefaultNoRefusesAndRecordsNothing proves the
// exact default-No shape: an interactive terminal that answers anything
// other than "y" (including a bare newline, the default) refuses exactly
// like the non-interactive case, and writes no trust record.
func TestRunTrustGate_Interactive_DefaultNoRefusesAndRecordsNothing(t *testing.T) {
	home := trustTestHome(t, "work")
	d, out, errb := trustGateDeps(t, home)
	d.Interactive = true
	d.In = strings.NewReader("\n") // bare Enter: default is No

	dir := t.TempDir()
	code := dispatch([]string{"run", dir, "--env", "work"}, d)

	if code == 0 {
		t.Fatalf("dispatch exit = 0, want nonzero (stdout=%q stderr=%q)", out.String(), errb.String())
	}
	if !strings.Contains(errb.String(), "not accepted") {
		t.Fatalf("stderr = %q, want the explicit not-accepted refusal", errb.String())
	}
	if _, err := os.Stat(trustRecordFile(home, "work")); err == nil {
		t.Fatal("declining must never record trust")
	}
}

// TestRunTrustGate_Interactive_PrintsExactBOMBeforePrompting proves the
// interactive first-use path prints the SAME canonical bill of materials
// `pix env trust` itself prints (host commands/services, credential
// targets, mounts, MCP servers, kits, and the fingerprint) before ever
// reading the accept/decline answer — a user must see what they are
// approving, not merely a bare y/N prompt.
func TestRunTrustGate_Interactive_PrintsExactBOMBeforePrompting(t *testing.T) {
	home := trustTestHome(t, "work")
	d, _, errb := trustGateDeps(t, home)
	d.Interactive = true
	d.In = strings.NewReader("n\n")

	dir := t.TempDir()
	_ = dispatch([]string{"run", dir, "--env", "work"}, d)

	got := errb.String()
	for _, want := range []string{"pix env trust work", "host command(s)", "host service(s)", "credential target(s)", "fingerprint:"} {
		if !strings.Contains(got, want) {
			t.Errorf("interactive trust prompt did not print %q; got:\n%s", want, got)
		}
	}
}

// TestRunTrustGate_Interactive_AcceptRecordsTrustAndTheSecondRunSkipsThePrompt
// is the accept half, and the whole point of "immediately before mutation,
// recomputed": accepting on the first run durably records trust (readable
// by `pix env show`/`pix env list`, exactly the file `pix env trust`
// itself would have written), and re-running the identical launch
// afterward must NOT prompt again — the failure this time comes from
// further down run's own pipeline (sbx absent), never from the trust gate.
func TestRunTrustGate_Interactive_AcceptRecordsTrustAndTheSecondRunSkipsThePrompt(t *testing.T) {
	home := trustTestHome(t, "work")
	dir := t.TempDir()

	d, _, errb := trustGateDeps(t, home)
	d.Interactive = true
	d.In = strings.NewReader("y\n")
	_ = dispatch([]string{"run", dir, "--env", "work"}, d)

	if strings.Contains(errb.String(), "not accepted") {
		t.Fatalf("an explicit \"y\" was treated as a decline; stderr = %s", errb.String())
	}
	if !strings.Contains(errb.String(), `environment "work" trusted`) {
		t.Fatalf("accepting did not confirm trust; stderr = %s", errb.String())
	}
	if _, err := os.Stat(trustRecordFile(home, "work")); err != nil {
		t.Fatalf("accepting must durably record trust: %v", err)
	}

	// Second run: same environment, same PIX_HOME, no input needed at all —
	// a prompt here (or an EOF-driven decline) would mean the record from
	// the first run was never actually consulted.
	d2, _, errb2 := trustGateDeps(t, home)
	// trustGateDeps re-points PATH/PIX_CONFIG at fresh temp dirs, so re-set
	// PIX_HOME to the SAME home the first run trusted.
	t.Setenv("PIX_HOME", home)
	d2.Interactive = false // no TTY at all: a re-prompt would hang/EOF, not merely annoy.
	d2.In = strings.NewReader("")
	_ = dispatch([]string{"run", dir, "--env", "work"}, d2)

	if strings.Contains(errb2.String(), "unreviewed environment") || strings.Contains(errb2.String(), "not accepted") {
		t.Fatalf("a previously-trusted, unchanged environment re-prompted on a later run; stderr = %s", errb2.String())
	}
}
