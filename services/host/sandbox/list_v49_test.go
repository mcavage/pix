package sandbox

import "testing"

// Shape observed on sbx v0.49.1 (introduced in v0.48), with synthetic
// identity: `sbx ls --json` is a bare array (no {"sandboxes": [...]} wrapper)
// whose rows are the v0.43 row shape.
const v49BareListing = `[
	{"name":"csbx-demo","id":"75fca117-54df-44c1-b7ce-e7e96b002743","agent":"claude","status":"stopped","last_used_at":"2026-10-08T06:16:36.863875Z","created_at":"2026-10-01T05:34:44Z","workspaces":["/workspace","/context"]},
	{"name":"shell-demo","id":"ce4eecf4-33eb-4296-bc2c-55c33ef228d3","agent":"shell","status":"running","last_used_at":"2026-08-20T05:14:15Z","created_at":"2026-08-20T05:14:15Z","workspace_missing":true},
	{"name":"pix-demo","id":"a19c9c54-d554-4acd-b9b3-0f9f2bc421c1","agent":"pix","status":"stopped","last_used_at":"2026-10-21T16:12:21.736933Z","created_at":"2026-10-01T05:34:44Z","workspaces":["/workspace","/context"]}
]`

// The regression this fixes: with sbx v0.48+ every row of a bare array was
// judged against the legacy profile, where agent/workspaces/created_at are
// undocumented keys and "id"/"status" are aliases, so IdentityVerified was
// false and `pix run` refused to attach with "is not a schema-verified running
// or stopped sandbox".
func TestParseListV49BareArrayStaysVerified(t *testing.T) {
	res, err := ParseList([]byte(v49BareListing))
	if err != nil {
		t.Fatalf("ParseList: %v", err)
	}
	if !res.SchemaVerified {
		t.Error("SchemaVerified = false, want true for a bare v0.49 listing")
	}
	got := FindByName(res.Entries, "pix-demo")
	if got == nil {
		t.Fatal("pix row not found")
	}
	if !got.IdentityVerified {
		t.Error("IdentityVerified = false, want true: the create receipt would refuse")
	}
	if got.InstanceID == nil || *got.InstanceID != "a19c9c54-d554-4acd-b9b3-0f9f2bc421c1" {
		t.Errorf("InstanceID = %v, want the row's id", got.InstanceID)
	}
	if got.State != StateStopped {
		t.Errorf("State = %v, want stopped", got.State)
	}
	if run := FindByName(res.Entries, "shell-demo"); run == nil || run.State != StateRunning {
		t.Errorf("shell row = %+v, want running", run)
	}
}

// A bare array under the v0.38 profile keeps that profile's strictness: a
// malformed row fails the whole parse instead of degrading to the legacy rules
// (a silently dropped or loosely read row could hide a live sandbox).
func TestParseListV49BareArrayIsStrict(t *testing.T) {
	for name, data := range map[string]string{
		"id not a uuid": `[{"name":"pix-demo","id":"abc123","agent":"pix","status":"stopped"}]`,
		"missing id":    `[{"name":"pix-demo","agent":"pix","status":"stopped"}]`,
		"bad created":   `[{"name":"pix-demo","id":"a19c9c54-d554-4acd-b9b3-0f9f2bc421c1","agent":"pix","status":"stopped","created_at":"yesterday"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseList([]byte(data)); err == nil {
				t.Fatal("malformed v0.49 row accepted")
			}
		})
	}
}

// An undocumented extra key or unknown status still downgrades the row, as it
// does for the wrapped profile.
func TestParseListV49BareArrayDowngrades(t *testing.T) {
	for name, data := range map[string]string{
		"extra key":      `[{"name":"pix-demo","id":"a19c9c54-d554-4acd-b9b3-0f9f2bc421c1","agent":"pix","status":"stopped","surprise":1}]`,
		"unknown status": `[{"name":"pix-demo","id":"a19c9c54-d554-4acd-b9b3-0f9f2bc421c1","agent":"pix","status":"hibernating"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			res, err := ParseList([]byte(data))
			if err != nil {
				t.Fatalf("ParseList: %v", err)
			}
			if res.SchemaVerified || res.Entries[0].IdentityVerified {
				t.Errorf("verified = %v/%v, want false/false", res.SchemaVerified, res.Entries[0].IdentityVerified)
			}
		})
	}
}

// The profile is chosen for the whole array. A mix of rows with and without
// "agent" is neither shape, so it fails the parse, in either order, rather
// than judging genuine v0.38 rows by legacy rules and leaving all unverified.
func TestParseListBareArrayMixedFails(t *testing.T) {
	const v38 = `{"name":"pix-demo","id":"a19c9c54-d554-4acd-b9b3-0f9f2bc421c1","agent":"pix","status":"stopped"}`
	const legacy = `{"name":"pix-other","state":"exited"}`
	for name, data := range map[string]string{
		"agent row first":  `[` + v38 + `,` + legacy + `]`,
		"legacy row first": `[` + legacy + `,` + v38 + `]`,
		"non-object row":   `[` + v38 + `,5]`,
	} {
		t.Run(name, func(t *testing.T) {
			if res, err := ParseList([]byte(data)); err == nil {
				t.Fatalf("mixed bare array accepted: %+v", res)
			}
		})
	}
}

// Rows that all carry "agent" take the strict v0.38 profile even when they
// otherwise look legacy, so a null agent or a legacy alias fails the parse.
func TestParseListBareArrayAgentRowsAreStrict(t *testing.T) {
	for name, data := range map[string]string{
		"null agent":   `[{"name":"pix-demo","id":"a19c9c54-d554-4acd-b9b3-0f9f2bc421c1","agent":null,"status":"stopped"}]`,
		"legacy alias": `[{"name":"pix-demo","instance_id":"inst-1","agent":"pix","state":"stopped"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			if res, err := ParseList([]byte(data)); err == nil {
				t.Fatalf("malformed v0.38 row accepted: %+v", res)
			}
		})
	}
}

// An empty bare array has no rows to profile and stays verified, as before.
func TestParseListEmptyBareArray(t *testing.T) {
	res, err := ParseList([]byte(`[]`))
	if err != nil || !res.SchemaVerified || len(res.Entries) != 0 {
		t.Fatalf("parse = %+v, %v", res, err)
	}
}
