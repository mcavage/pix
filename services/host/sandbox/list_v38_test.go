package sandbox

import "testing"

// TestParseList_V38Canonical: the exact top-level/row shape sbx v0.38 emits
// for `sbx ls --json` — object-wrapped `{"sandboxes": [...]}`, rows keyed
// name/id/agent/status/workspaces/workspace_missing — parses with every
// entry (and the overall result) SchemaVerified/IdentityVerified, and the
// UUID "id" lands as the entry's InstanceID.
func TestParseList_V38Canonical(t *testing.T) {
	res, err := ParseList(readFixture(t, "list_v38_canonical.json"))
	if err != nil {
		t.Fatalf("ParseList: %v", err)
	}
	if !res.SchemaVerified {
		t.Fatalf("SchemaVerified = false, want true for a canonical v0.38 listing")
	}
	if len(res.Entries) != 2 {
		t.Fatalf("len(Entries) = %d, want 2", len(res.Entries))
	}

	e0 := res.Entries[0]
	if e0.Name != "pix-demo" || e0.State != StateRunning || !e0.IdentityVerified {
		t.Errorf("entry 0 = %+v, want Name=pix-demo State=Running Verified=true", e0)
	}
	if e0.InstanceID == nil || *e0.InstanceID != "5c2b6e0a-1f3d-4a9b-8e21-7d4f2b6c9a10" {
		t.Errorf("entry 0 InstanceID = %v, want the UUID id", e0.InstanceID)
	}

	e1 := res.Entries[1]
	if e1.Name != "pix-other" || e1.State != StateStopped || !e1.IdentityVerified {
		t.Errorf("entry 1 = %+v, want Name=pix-other State=Stopped Verified=true", e1)
	}
	if e1.InstanceID == nil || *e1.InstanceID != "9d1e4f2a-8b3c-4d5e-9f6a-1b2c3d4e5f60" {
		t.Errorf("entry 1 InstanceID = %v, want the UUID id", e1.InstanceID)
	}
}

// TestParseList_V38MutationTable is the negative half of the v0.38 profile:
// every documented way a row can fail to be the pinned canonical shape,
// each mutated from the SAME verbatim positive fixture by exactly one field.
func TestParseList_V38MutationTable(t *testing.T) {
	cases := []struct {
		name     string
		fixture  string
		wantErr  bool
		verified bool // only checked when wantErr is false
	}{
		{
			name:    "canonical row is a hard pass",
			fixture: "list_v38_canonical.json",
			wantErr: false, verified: true,
		},
		{
			name:    "extra key downgrades but does not error",
			fixture: "list_v38_extra_key.json",
			wantErr: false, verified: false,
		},
		{
			name:    "id not shaped like a UUID fails closed",
			fixture: "list_v38_id_bad.json",
			wantErr: true,
		},
		{
			name:    "wrong type (workspace_missing as string) fails closed",
			fixture: "list_v38_type_bad.json",
			wantErr: true,
		},
		{
			name:    "missing required field (agent) fails closed",
			fixture: "list_v38_missing_field.json",
			wantErr: true,
		},
		{
			name:    "unrecognized status downgrades but does not error",
			fixture: "list_v38_unknown_status.json",
			wantErr: false, verified: false,
		},
		{
			name:    "unknown wrapper key fails closed",
			fixture: "list_v38_unknown_wrapper.json",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := ParseList(readFixture(t, tc.fixture))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseList(%s) = nil error, want one", tc.fixture)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseList(%s): %v", tc.fixture, err)
			}
			if len(res.Entries) == 0 {
				t.Fatalf("ParseList(%s): no entries", tc.fixture)
			}
			if res.Entries[0].IdentityVerified != tc.verified {
				t.Errorf("ParseList(%s): entry IdentityVerified = %v, want %v", tc.fixture, res.Entries[0].IdentityVerified, tc.verified)
			}
			if res.SchemaVerified != tc.verified {
				t.Errorf("ParseList(%s): SchemaVerified = %v, want %v", tc.fixture, res.SchemaVerified, tc.verified)
			}
		})
	}
}

// TestParseList_V38DoesNotAcceptLegacyAliasKeys: a v0.38-wrapped row using a
// LEGACY alias key ("Name"/"Status"/"ID" — accepted under the legacy
// profile's aliasing) is not silently reinterpreted; the alias is outside
// the SELECTED (v0.38) profile, and here it means "name" is simply absent —
// a required field, so the whole parse fails closed rather than guessing.
func TestParseList_V38DoesNotAcceptLegacyAliasKeys(t *testing.T) {
	data := []byte(`{"sandboxes": [{"Name": "pix-bar", "Status": "running", "ID": "5c2b6e0a-1f3d-4a9b-8e21-7d4f2b6c9a10"}]}`)
	if _, err := ParseList(data); err == nil {
		t.Fatalf("ParseList(legacy-alias-keyed v0.38 wrapper) = nil error, want one")
	}
}

// TestFindByName_V38TrustedInstanceID: FindByName over a v0.38-parsed,
// verified listing returns an entry whose InstanceID is the trusted UUID —
// this is the exact shape RecordSessionCreation (workflow/launch/session.go)
// depends on to write its record/fingerprint/invocation.
func TestFindByName_V38TrustedInstanceID(t *testing.T) {
	res, err := ParseList(readFixture(t, "list_v38_canonical.json"))
	if err != nil {
		t.Fatalf("ParseList: %v", err)
	}
	found := FindByName(res.Entries, "pix-demo")
	if found == nil {
		t.Fatalf("FindByName(pix-demo) = nil")
	}
	if !found.IdentityVerified {
		t.Fatalf("IdentityVerified = false, want true")
	}
	if found.InstanceID == nil || *found.InstanceID == "" {
		t.Fatalf("InstanceID = %v, want a non-empty UUID", found.InstanceID)
	}
}
