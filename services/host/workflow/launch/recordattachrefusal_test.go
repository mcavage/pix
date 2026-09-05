// recordattachrefusal_test.go — E2.6's wiring half (AC-68): exactly one
// recreatelog record appends on a creation-fingerprint attach refusal,
// nothing on an unrelated refusal, nothing on a successful attach, and
// nothing for the unnamed `none` environment.
package launch

import (
	"reflect"
	"testing"

	"pix/host/envinfo"
	"pix/host/recreatelog"
)

func TestRecordAttachRefusal_OnlyOnFingerprintDrift(t *testing.T) {
	dir := t.TempDir()

	// An unrelated refusal (no Drifts) appends nothing.
	unrelated := AttachDecision{Refusal: "is not a schema-verified running sandbox", Drifts: nil}
	if err := RecordAttachRefusal(dir, "work", unrelated); err != nil {
		t.Fatalf("RecordAttachRefusal: %v", err)
	}
	if recs := mustReadRecreateLog(t, dir); len(recs) != 0 {
		t.Fatalf("unrelated refusal appended %d records, want 0", len(recs))
	}

	// A successful attach appends nothing.
	ok := AttachDecision{Attach: true}
	if err := RecordAttachRefusal(dir, "work", ok); err != nil {
		t.Fatalf("RecordAttachRefusal: %v", err)
	}
	if recs := mustReadRecreateLog(t, dir); len(recs) != 0 {
		t.Fatalf("successful attach appended %d records, want 0", len(recs))
	}

	// A creation-fingerprint drift refusal appends exactly one record,
	// keyed by the stable canonical KeyPath (or, absent one, the composed
	// key) — sorted and deduplicated, never a facet value.
	drifted := AttachDecision{
		Refusal: `"pix-x" no longer matches its recorded creation fingerprint — refusing to attach.`,
		Drifts: []envinfo.Drift{
			{ComposedKey: "mcp.servers[github].url", KeyPath: "mcp.servers[github].url", Identity: true},
			{ComposedKey: "kits[]", EntriesChanged: 2},
		},
	}
	if err := RecordAttachRefusal(dir, "work", drifted); err != nil {
		t.Fatalf("RecordAttachRefusal: %v", err)
	}
	recs := mustReadRecreateLog(t, dir)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	if recs[0].Environment != "work" {
		t.Errorf("environment = %q, want work", recs[0].Environment)
	}
	want := []string{"kits[]", "mcp.servers[github].url"}
	if !reflect.DeepEqual(recs[0].ChangedKeyPaths, want) {
		t.Errorf("changed key paths = %v, want %v", recs[0].ChangedKeyPaths, want)
	}
}

// A reset-invalidated refusal (ResetInvalidatedDrift's "*" record, no
// KeyPath) still logs — it IS a creation-fingerprint drift, just one with
// no pre-composition source — falling back to its ComposedKey.
func TestRecordAttachRefusal_ResetInvalidatedFallsBackToComposedKey(t *testing.T) {
	dir := t.TempDir()
	drifted := AttachDecision{
		Refusal: "no longer matches its recorded creation fingerprint",
		Drifts:  []envinfo.Drift{envinfo.ResetInvalidatedDrift()},
	}
	if err := RecordAttachRefusal(dir, "home", drifted); err != nil {
		t.Fatalf("RecordAttachRefusal: %v", err)
	}
	recs := mustReadRecreateLog(t, dir)
	if len(recs) != 1 || len(recs[0].ChangedKeyPaths) != 1 || recs[0].ChangedKeyPaths[0] != "*" {
		t.Fatalf("got %+v, want one record with changed key path \"*\"", recs)
	}
}

// The unnamed `none` environment (no environment selected) has nothing to
// key a recreate record by: RecordAttachRefusal skips quietly rather than
// handing recreatelog an empty name it would refuse outright.
func TestRecordAttachRefusal_EmptyEnvironmentNameSkipsQuietly(t *testing.T) {
	dir := t.TempDir()
	drifted := AttachDecision{
		Refusal: "no longer matches its recorded creation fingerprint",
		Drifts:  []envinfo.Drift{envinfo.ResetInvalidatedDrift()},
	}
	if err := RecordAttachRefusal(dir, "", drifted); err != nil {
		t.Fatalf("RecordAttachRefusal: %v", err)
	}
	if recs := mustReadRecreateLog(t, dir); len(recs) != 0 {
		t.Fatalf("got %d records for an unnamed environment, want 0", len(recs))
	}
}

// Two DIFFERENT drift refusals against the SAME environment collapse to
// their own records — recreatelog's own bound (I4, cap 100) is what keeps
// this from growing unbounded, not this wiring.
func TestRecordAttachRefusal_DuplicatePathWithinOneRefusalCollapses(t *testing.T) {
	dir := t.TempDir()
	drifted := AttachDecision{
		Refusal: "no longer matches its recorded creation fingerprint",
		Drifts: []envinfo.Drift{
			{ComposedKey: "env.FOO", KeyPath: "env.FOO", Identity: true},
			{ComposedKey: "env.FOO", KeyPath: "env.FOO", Identity: true},
		},
	}
	if err := RecordAttachRefusal(dir, "work", drifted); err != nil {
		t.Fatalf("RecordAttachRefusal: %v", err)
	}
	recs := mustReadRecreateLog(t, dir)
	if len(recs) != 1 || len(recs[0].ChangedKeyPaths) != 1 || recs[0].ChangedKeyPaths[0] != "env.FOO" {
		t.Fatalf("got %+v, want one record with one deduplicated changed key path", recs)
	}
}

func mustReadRecreateLog(t *testing.T, dir string) []recreatelog.Record {
	t.Helper()
	recs, err := recreatelog.Read(dir)
	if err != nil {
		t.Fatalf("recreatelog.Read: %v", err)
	}
	return recs
}
