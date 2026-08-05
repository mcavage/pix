package doctor

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"pix/host/health"
)

// schema_contract_test.go is the enforcement half of the v4 break. Bumping a
// schema version in a constant costs nothing; what a downstream reader needs
// is the guarantee that the new shape is EXACTLY what was announced, that the
// old field names are gone (so a stale parser fails loudly rather than
// reading a subset it happens to recognize), and that the versions it
// replaces are named with a migration note.

// v4TopLevel is the published top-level key set. Changing it is a schema
// break and must come with a version bump and a RetiredSchemas entry — this
// test is what makes that non-optional.
var v4TopLevel = []string{
	"schema_version", "version", "config_path", "profile", "verdict", "ready",
	"checks", "fixes", "exit", "elapsed_ms",
}

// v4Check is one row's key set. `evidence` and `fix` are omitempty, so the
// contract is "no key outside this set", not "every key present".
var v4Check = map[string]bool{
	"name": true, "status": true, "required": true, "detail": true,
	"evidence": true, "fix": true, "duration_ms": true,
}

func v4Payload(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	snap := health.Snapshot{Results: []health.Result{
		{Name: "sbx", Status: health.StatusAbsent, Required: true, Detail: "not installed",
			Fix: health.SbxInstallFix, Evidence: "sbx is not on PATH"},
	}}
	b, err := json.Marshal(ReportJSON(snap, "default", snap.ExitCode()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func TestSchemaV4_TopLevelKeysAreExactlyTheContract(t *testing.T) {
	got := v4Payload(t)
	var keys []string
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := append([]string(nil), v4TopLevel...)
	sort.Strings(want)
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Errorf("top-level keys = %v\nwant %v", keys, want)
	}
}

func TestSchemaV4_CarriesNoRetiredKey(t *testing.T) {
	got := v4Payload(t)
	for _, dead := range RetiredSchemaKeys {
		if _, ok := got[dead]; ok {
			t.Errorf("v4 still publishes the retired key %q — the break is not clean", dead)
		}
	}
}

func TestSchemaV4_RowKeysAreExactlyTheContract(t *testing.T) {
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(v4Payload(t)["checks"], &rows); err != nil {
		t.Fatalf("checks is not an array of objects: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	for k := range rows[0] {
		if !v4Check[k] {
			t.Errorf("check row publishes undeclared key %q", k)
		}
	}
}

// The version constant and the retirement notes must agree: every version
// below the current one is accounted for, none of them claims to BE the
// current one, and each carries a migration note a human can act on.
func TestSchemaRetirement_IsCompleteAndConsistent(t *testing.T) {
	if _, ok := RetiredSchemas[SchemaVersion]; ok {
		t.Fatalf("the CURRENT schema version %d is listed as retired", SchemaVersion)
	}
	for v := 1; v < SchemaVersion; v++ {
		note, ok := RetiredSchemas[v]
		if !ok {
			t.Errorf("schema v%d is not accounted for in RetiredSchemas", v)
			continue
		}
		if len(strings.TrimSpace(note)) < 40 {
			t.Errorf("schema v%d's migration note is too thin to act on: %q", v, note)
		}
	}
	if len(RetiredSchemas) != SchemaVersion-1 {
		t.Errorf("RetiredSchemas has %d entries for a v%d schema", len(RetiredSchemas), SchemaVersion)
	}
}

// Both verbs publish the SAME schema version. They emit one shape now, and a
// consumer that can read `pix doctor --json` can read `pix status --json`.
func TestSchemaV4_IsPublishedByBothSurfaces(t *testing.T) {
	snap := health.Snapshot{}
	if got := ReportJSON(snap, "", snap.ExitCode()).SchemaVersion; got != SchemaVersion {
		t.Errorf("schema_version = %d, want %d", got, SchemaVersion)
	}
	if !strings.Contains(Usage, "schema_version 4") || !strings.Contains(StatusUsage, "schema_version 4") {
		t.Error("both --json flags must document the schema version they emit")
	}
}
