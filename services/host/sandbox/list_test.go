package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

// TestParseList_Canonical: a bare-array listing using only canonical field
// names parses with every entry (and the overall result) verified, and both
// the tri-state State and the optional InstanceID land correctly.
func TestParseList_Canonical(t *testing.T) {
	res, err := ParseList(readFixture(t, "list_canonical.json"))
	if err != nil {
		t.Fatalf("ParseList: %v", err)
	}
	if !res.SchemaVerified {
		t.Fatalf("SchemaVerified = false, want true for an all-canonical bare-array listing")
	}
	if len(res.Entries) != 2 {
		t.Fatalf("len(Entries) = %d, want 2", len(res.Entries))
	}

	e0 := res.Entries[0]
	if e0.Name != "pix-demo" || e0.State != StateRunning || !e0.IdentityVerified {
		t.Errorf("entry 0 = %+v, want Name=pix-demo State=Running Verified=true", e0)
	}
	if e0.InstanceID == nil || *e0.InstanceID != "abc123" {
		t.Errorf("entry 0 InstanceID = %v, want abc123", e0.InstanceID)
	}

	e1 := res.Entries[1]
	// "exited" is a documented VALUE alias for Stopped; the KEY used ("state")
	// is still canonical, so this row stays verified.
	if e1.Name != "pix-other" || e1.State != StateStopped || !e1.IdentityVerified {
		t.Errorf("entry 1 = %+v, want Name=pix-other State=Stopped Verified=true", e1)
	}
	if e1.InstanceID != nil {
		t.Errorf("entry 1 InstanceID = %v, want nil (field absent)", *e1.InstanceID)
	}
}

// TestParseList_AliasWrapped: an object-wrapped listing using alias field
// names throughout parses successfully but is NOT verified — at any level
// (wrapper, per-row).
func TestParseList_AliasWrapped(t *testing.T) {
	res, err := ParseList(readFixture(t, "list_alias_wrapped.json"))
	if err != nil {
		t.Fatalf("ParseList: %v", err)
	}
	if res.SchemaVerified {
		t.Fatalf("SchemaVerified = true, want false for an alias-wrapped, alias-keyed listing")
	}
	if len(res.Entries) != 1 {
		t.Fatalf("len(Entries) = %d, want 1", len(res.Entries))
	}
	e := res.Entries[0]
	if e.Name != "pix-bar" || e.State != StateStopped {
		t.Fatalf("entry = %+v, want Name=pix-bar State=Stopped", e)
	}
	if e.IdentityVerified {
		t.Errorf("IdentityVerified = true, want false: every field used an alias key")
	}
	if e.InstanceID == nil || *e.InstanceID != "xyz789" {
		t.Errorf("InstanceID = %v, want xyz789 (parsed leniently despite the alias key)", e.InstanceID)
	}
}

// TestParseList_UnknownStateValue: an undocumented state VALUE resolves to
// StateUnknown, but does NOT by itself unverify the row — the KEY used was
// still canonical.
func TestParseList_UnknownStateValue(t *testing.T) {
	res, err := ParseList(readFixture(t, "list_unknown_state.json"))
	if err != nil {
		t.Fatalf("ParseList: %v", err)
	}
	if len(res.Entries) != 1 {
		t.Fatalf("len(Entries) = %d, want 1", len(res.Entries))
	}
	e := res.Entries[0]
	if e.State != StateUnknown {
		t.Errorf("State = %v, want Unknown for an undocumented value", e.State)
	}
	if !e.IdentityVerified {
		t.Errorf("IdentityVerified = false, want true: the KEY was canonical even though the VALUE was unrecognized")
	}
	if !res.SchemaVerified {
		t.Errorf("SchemaVerified = false, want true")
	}
}

// TestParseList_MissingInstanceID: InstanceID is genuinely optional.
func TestParseList_MissingInstanceID(t *testing.T) {
	res, err := ParseList(readFixture(t, "list_missing_instance_id.json"))
	if err != nil {
		t.Fatalf("ParseList: %v", err)
	}
	if len(res.Entries) != 1 {
		t.Fatalf("len(Entries) = %d, want 1", len(res.Entries))
	}
	if res.Entries[0].InstanceID != nil {
		t.Errorf("InstanceID = %v, want nil", *res.Entries[0].InstanceID)
	}
	if !res.Entries[0].IdentityVerified {
		t.Errorf("IdentityVerified = false, want true: a MISSING optional field is not an alias fallback")
	}
}

// TestParseList_UndocumentedExtraKey: a row with an entirely unrecognized
// extra key is parsed (its known fields are still read) but flagged
// unverified — the extra key is evidence this might be a schema this
// package has never seen, even though name/state happened to be canonical.
func TestParseList_UndocumentedExtraKey(t *testing.T) {
	res, err := ParseList(readFixture(t, "list_undocumented_extra_key.json"))
	if err != nil {
		t.Fatalf("ParseList: %v", err)
	}
	if len(res.Entries) != 1 {
		t.Fatalf("len(Entries) = %d, want 1", len(res.Entries))
	}
	e := res.Entries[0]
	if e.Name != "pix-extra" || e.State != StateRunning {
		t.Fatalf("entry = %+v, want Name=pix-extra State=Running", e)
	}
	if e.IdentityVerified {
		t.Errorf("IdentityVerified = true, want false: an undocumented key was present")
	}
	if res.SchemaVerified {
		t.Errorf("SchemaVerified = true, want false")
	}
}

// TestParseList_Malformed: invalid JSON is a hard parse error, not a silent
// empty result.
func TestParseList_Malformed(t *testing.T) {
	if _, err := ParseList(readFixture(t, "list_malformed.json")); err == nil {
		t.Fatalf("ParseList(malformed) = nil error, want one")
	}
}

// TestParseList_UnresolvableName: a row with none of the documented name-key
// aliases fails the WHOLE parse rather than being silently dropped — a
// silently-skipped row could hide a live sandbox from a caller deciding
// whether one already exists.
func TestParseList_UnresolvableName(t *testing.T) {
	if _, err := ParseList(readFixture(t, "list_unresolvable_name.json")); err == nil {
		t.Fatalf("ParseList(unresolvable name) = nil error, want one")
	}
}

// TestFindByName: present and absent cases against a small in-memory slice
// (no fixture needed — this is a pure lookup over already-parsed Entries).
func TestFindByName(t *testing.T) {
	entries := []Entry{{Name: "pix-a"}, {Name: "pix-b"}}
	if got := FindByName(entries, "pix-b"); got == nil || got.Name != "pix-b" {
		t.Fatalf("FindByName(pix-b) = %v, want pix-b", got)
	}
	if got := FindByName(entries, "pix-missing"); got != nil {
		t.Fatalf("FindByName(pix-missing) = %v, want nil", got)
	}
}
