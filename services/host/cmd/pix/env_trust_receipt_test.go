package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// env_trust_receipt_test.go proves the change screen from the REAL entry
// points a person uses — `pix env trust NAME` and `pix run --env NAME` —
// not from the renderer in isolation. The unit-level property (a changed
// fingerprint always produces a non-empty diff) lives in
// workflow/env/receipt_test.go; what these tests pin is that an acceptance
// actually records a receipt, and that the NEXT review reads as a change
// list rather than as a second full audit dump.

// mountEnvHome writes an environment whose only host-affecting fact is a
// mount list, so its bill of materials is Tier1 (and therefore gated)
// without depending on anything being on PATH.
func mountEnvHome(t *testing.T, envName string, mounts ...string) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "pixhome")
	writeMountEnv(t, home, envName, mounts...)
	return home
}

func writeMountEnv(t *testing.T, home, envName string, mounts ...string) {
	t.Helper()
	envDir := filepath.Join(home, "envs", envName)
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := "schemaVersion: \"1\"\nagent: pix\nadditionalWorkspaces:\n"
	for _, m := range mounts {
		doc += "  - path: " + m + "\n"
	}
	if err := os.WriteFile(filepath.Join(envDir, ".sbxenv.yaml"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readTrustRecordFile(t *testing.T, home, name string) envTrustRecord {
	t.Helper()
	data, err := os.ReadFile(trustRecordFile(home, name))
	if err != nil {
		t.Fatalf("reading trust record: %v", err)
	}
	var rec envTrustRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("parsing trust record: %v", err)
	}
	return rec
}

// TestEnvTrust_AcceptanceRecordsAReceipt: without this, every later review
// silently degrades to the full bill, so it is the load-bearing half of the
// feature and not a redundant assertion about a struct field.
func TestEnvTrust_AcceptanceRecordsAReceipt(t *testing.T) {
	mount := t.TempDir()
	home := mountEnvHome(t, "work", mount)
	d, out, _ := trustGateDeps(t, home)

	if code := dispatch([]string{"env", "trust", "work", "--yes"}, d); code != 0 {
		t.Fatalf("exit = %d (stdout=%q)", code, out.String())
	}
	rec := readTrustRecordFile(t, home, "work")
	if len(rec.Receipt) == 0 {
		t.Fatal("acceptance recorded no receipt; the next review cannot show a change list")
	}
	var sawMount bool
	for _, e := range rec.Receipt {
		if e.Section == "mount" && e.Key == mount {
			sawMount = true
		}
		if e.Digest == "" {
			t.Fatalf("receipt entry %+v has no digest", e)
		}
	}
	if !sawMount {
		t.Fatalf("receipt %+v does not itemize the environment's one mount", rec.Receipt)
	}
}

// TestEnvTrust_ReReviewShowsTheChangeNotTheWholeBill is the whole point: an
// operator who already accepted this environment is asked about the delta.
func TestEnvTrust_ReReviewShowsTheChangeNotTheWholeBill(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	home := mountEnvHome(t, "work", first)
	d, out, _ := trustGateDeps(t, home)
	if code := dispatch([]string{"env", "trust", "work", "--yes"}, d); code != 0 {
		t.Fatalf("first accept exit = %d (stdout=%q)", code, out.String())
	}

	// One mount added. The fingerprint moves, so the gate re-opens.
	writeMountEnv(t, home, "work", first, second)
	d2, out2, _ := trustGateDeps(t, home)
	if code := dispatch([]string{"env", "trust", "work", "--yes"}, d2); code != 0 {
		t.Fatalf("second accept exit = %d (stdout=%q)", code, out2.String())
	}
	got := out2.String()
	if !strings.Contains(got, "you accepted this environment on ") {
		t.Fatalf("re-review did not identify itself as a re-review:\n%s", got)
	}
	if !strings.Contains(got, "added    mount") {
		t.Fatalf("re-review did not name the added mount as the change:\n%s", got)
	}
	if !strings.Contains(got, second) {
		t.Fatalf("re-review did not name WHICH mount was added:\n%s", got)
	}
	// The unchanged mount must not be re-litigated: it is counted, not listed
	// as a change.
	if strings.Contains(got, "changed  mount    "+first) {
		t.Fatalf("re-review reported an unchanged mount as changed:\n%s", got)
	}
	if !strings.Contains(got, "1 reviewed fact(s) changed since, ") {
		t.Fatalf("re-review did not report the size of the change:\n%s", got)
	}
	if !strings.Contains(got, "full bill: pix env trust work --verbose") {
		t.Fatalf("re-review did not offer the full bill:\n%s", got)
	}
	// And the second acceptance replaces the receipt, so a THIRD review
	// diffs against what was accepted second, not first.
	rec := readTrustRecordFile(t, home, "work")
	var mounts int
	for _, e := range rec.Receipt {
		if e.Section == "mount" {
			mounts++
		}
	}
	if mounts != 2 {
		t.Fatalf("receipt after the second acceptance itemizes %d mount(s), want 2: %+v", mounts, rec.Receipt)
	}
}

// TestEnvTrust_FirstReviewIsStillTheFullBill: a person who has never seen
// this environment gets the bill, never a change list against nothing.
func TestEnvTrust_FirstReviewIsStillTheFullBill(t *testing.T) {
	home := mountEnvHome(t, "work", t.TempDir())
	d, out, _ := trustGateDeps(t, home)

	if code := dispatch([]string{"env", "trust", "work", "--yes"}, d); code != 0 {
		t.Fatalf("exit = %d (stdout=%q)", code, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "environment runs code on your host") {
		t.Fatalf("first review was not the full bill:\n%s", got)
	}
	if strings.Contains(got, "you accepted this environment on") {
		t.Fatalf("first review claimed a prior acceptance:\n%s", got)
	}
}

// TestEnvTrust_RecordWithoutAReceiptFallsBackHonestly is the upgrade path: a
// record written by a pix that predates receipts must not produce an
// invented or empty diff. It degrades to the full bill AND says why, rather
// than silently looking like a first review.
func TestEnvTrust_RecordWithoutAReceiptFallsBackHonestly(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	home := mountEnvHome(t, "work", first)
	d, out, _ := trustGateDeps(t, home)
	if code := dispatch([]string{"env", "trust", "work", "--yes"}, d); code != 0 {
		t.Fatalf("first accept exit = %d (stdout=%q)", code, out.String())
	}
	// Strip the receipt, exactly as an older pix would have left it.
	rec := readTrustRecordFile(t, home, "work")
	rec.Receipt = nil
	b, _ := json.MarshalIndent(rec, "", "  ")
	if err := os.WriteFile(trustRecordFile(home, "work"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	writeMountEnv(t, home, "work", first, second)
	d2, out2, _ := trustGateDeps(t, home)
	if code := dispatch([]string{"env", "trust", "work", "--yes"}, d2); code != 0 {
		t.Fatalf("second accept exit = %d (stdout=%q)", code, out2.String())
	}
	got := out2.String()
	if !strings.Contains(got, "environment runs code on your host") {
		t.Fatalf("a receiptless record must fall back to the full bill:\n%s", got)
	}
	if !strings.Contains(got, "pix cannot show what changed") {
		t.Fatalf("a receiptless record must say it could not compare:\n%s", got)
	}
	// And the fallback repairs itself: this acceptance records a receipt.
	if len(readTrustRecordFile(t, home, "work").Receipt) == 0 {
		t.Fatal("the fallback path did not record a receipt, so it would fall back forever")
	}
}

// TestRunTrustGate_ReReviewShowsTheChange proves `pix run`'s inline gate is
// wired to the same screen and the same writer — a feature is not done until
// its real caller is wired, and run's gate is the caller most people meet.
func TestRunTrustGate_ReReviewShowsTheChange(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	home := mountEnvHome(t, "work", first)
	d, out, _ := trustGateDeps(t, home)
	if code := dispatch([]string{"env", "trust", "work", "--yes"}, d); code != 0 {
		t.Fatalf("seed accept exit = %d (stdout=%q)", code, out.String())
	}

	writeMountEnv(t, home, "work", first, second)
	d2, _, errb := trustGateDeps(t, home)
	d2.Interactive = true
	d2.In = &countingReader{s: "n\n"}
	if code := dispatch([]string{"run", t.TempDir(), "--env", "work"}, d2); code == 0 {
		t.Fatal("declining the re-review must stop the launch")
	}
	got := errb.String()
	if !strings.Contains(got, "you accepted this environment on ") || !strings.Contains(got, "added    mount") {
		t.Fatalf("run's inline gate did not show the change list:\n%s", got)
	}
	if !strings.Contains(got, second) {
		t.Fatalf("run's inline gate did not name the added mount:\n%s", got)
	}
}
