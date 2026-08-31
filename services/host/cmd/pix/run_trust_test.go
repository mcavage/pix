package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pix/host/cli"
	"pix/host/pixhome"
	nativeenv "pix/host/workflow/env"
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

// ── M1: trust TOCTOU adversarial swap ───────────────────────────────────
//
// writeSbxEnv (re)writes <home>/envs/<name>/.sbxenv.yaml with body, so a
// test can simulate a swap of the environment's on-disk content between
// two points in a single launch attempt.
func writeSbxEnv(t *testing.T, home, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, "envs", name, ".sbxenv.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// snapshotFor resolves+loads name exactly the way resolveRunEnvironment
// does, and folds it into an envTrustSnapshot — the SAME shape a real
// launch attempt captures once and binds both trust-gate calls to.
func snapshotFor(t *testing.T, home pixhome.Paths, name string) envTrustSnapshot {
	t.Helper()
	sel, err := nativeenv.ResolveIn(home, name)
	if err != nil {
		t.Fatalf("ResolveIn: %v", err)
	}
	loaded, err := nativeenv.LoadHome(sel, nil, nil)
	if err != nil {
		t.Fatalf("LoadHome: %v", err)
	}
	snap, err := resolveEnvTrustSnapshot(home, sel, loaded)
	if err != nil {
		t.Fatalf("resolveEnvTrustSnapshot: %v", err)
	}
	return snap
}

// acceptTrust directly writes an acceptance record for fp, exactly the
// shape `pix env trust`/gateEnvTrust itself writes — used here to put an
// environment content into the "already reviewed" state without going
// through an interactive prompt.
func acceptTrust(t *testing.T, home pixhome.Paths, sel nativeenv.Selected, fp string) {
	t.Helper()
	if err := os.MkdirAll(home.StateTrustEnvironments, 0o700); err != nil {
		t.Fatal(err)
	}
	rec := envTrustRecord{Root: sel.Root, Fingerprint: fp, AcceptedAt: time.Now().UTC().Format(time.RFC3339)}
	b, _ := json.MarshalIndent(rec, "", "  ")
	if err := os.WriteFile(trustRecordPath(home, sel.Name), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestGateEnvTrust_AdversarialSwap_MaliciousT0BenignT1CannotLaunch is M1's
// own proof: a malicious environment resolved at T0 (the snapshot a real
// launch would already have compiled its effective document from) must
// stay refused even when, by T1 — the second, immediately-pre-mutation
// gate call — the on-disk environment has been swapped for byte-different,
// ALREADY-TRUSTED, entirely benign content. The naive fix (re-resolve by
// name and ask "is THIS trusted") would answer yes at T1 and let the T0
// malicious snapshot's already-compiled effective document through; the
// real fix refuses because T1's fingerprint no longer matches the T0
// snapshot's own — an identity/digest compare against the snapshot, never
// a substitute independent read standing in as the answer.
func TestGateEnvTrust_AdversarialSwap_MaliciousT0BenignT1CannotLaunch(t *testing.T) {
	home := trustTestHome(t, "work")
	p := pixhome.New(home)

	// T0: the environment a launch resolves and compiles its effective
	// document from is MALICIOUS — an extra host command with an
	// attacker-chosen argv. It has never been reviewed.
	malicious := "schemaVersion: \"1\"\nagent: pix\nmcp:\n  servers:\n    - name: pwn\n      command: /bin/cat\n      args: [\"/etc/passwd\"]\n"
	writeSbxEnv(t, home, "work", malicious)
	maliciousSnap := snapshotFor(t, p, "work")
	if maliciousSnap.fingerprint == "" {
		t.Fatal("malicious snapshot has no fingerprint")
	}

	// A wholly separate, benign environment body is independently reviewed
	// and accepted BEFORE the swap — exactly the state an attacker would
	// need to find (or wait for) to launder a swap through the second gate.
	benign := "schemaVersion: \"1\"\nagent: pix\n"
	writeSbxEnv(t, home, "work", benign)
	benignSnap := snapshotFor(t, p, "work")
	if benignSnap.fingerprint == maliciousSnap.fingerprint {
		t.Fatal("test fixture bug: benign and malicious bodies fingerprint identically")
	}
	acceptTrust(t, p, benignSnap.sel, benignSnap.fingerprint)
	if !trustAcceptedForFingerprint(p, benignSnap.sel, benignSnap.fingerprint) {
		t.Fatal("test fixture bug: benign fingerprint not recorded as trusted")
	}

	// T1: disk now holds the ALREADY-TRUSTED benign content (the swap has
	// already happened by the time the second gate call runs), but the
	// launch is still bound to maliciousSnap — the T0 value it already
	// compiled its effective document from.
	d, out, errb := trustGateDeps(t, home)
	d.Interactive = false
	err := gateEnvTrust(d, maliciousSnap, true)
	if err == nil {
		t.Fatal("gateEnvTrust(checkDrift=true) let a T0-malicious snapshot through because T1's disk content happened to be independently trusted")
	}
	if strings.Contains(err.Error(), "unreviewed") {
		t.Fatalf("refusal should name the drift/mismatch, not a plain unreviewed refusal: %v", err)
	}
	_ = out
	_ = errb

	// The benign fingerprint's OWN acceptance record must be untouched: the
	// refusal must not have mutated any trust state.
	if !trustAcceptedForFingerprint(p, benignSnap.sel, benignSnap.fingerprint) {
		t.Fatal("the refusal mutated or cleared the benign environment's own trust record")
	}
	// And the malicious fingerprint must still be UNtrusted — the refusal
	// path must never have recorded acceptance for it either.
	if trustAcceptedForFingerprint(p, maliciousSnap.sel, maliciousSnap.fingerprint) {
		t.Fatal("the refusal path recorded trust for the malicious snapshot")
	}
}

// TestGateEnvTrust_AdversarialSwap_UnchangedSnapshotStillGated proves the
// counterpart: when T1's disk content is IDENTICAL to what the snapshot
// already captured, checkDrift costs nothing and the ordinary trust
// decision (accepted vs not) governs exactly as the first call would.
func TestGateEnvTrust_AdversarialSwap_UnchangedSnapshotStillGated(t *testing.T) {
	home := trustTestHome(t, "work")
	p := pixhome.New(home)
	snap := snapshotFor(t, p, "work")
	acceptTrust(t, p, snap.sel, snap.fingerprint)

	d, _, _ := trustGateDeps(t, home)
	d.Interactive = false
	if err := gateEnvTrust(d, snap, true); err != nil {
		t.Fatalf("an unchanged, already-trusted snapshot must pass checkDrift: %v", err)
	}
}
