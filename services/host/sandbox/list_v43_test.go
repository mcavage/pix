package sandbox

import (
	"fmt"
	"testing"
)

// Shape observed on sbx v0.43.0-829-gf749bb121, with synthetic identity.
// created_at rides alongside v0.42's last_used_at on every row.
func TestParseListV43CreatedAt(t *testing.T) {
	for _, tc := range []struct {
		stamp string
		valid bool
	}{
		{`"2026-09-17T18:55:55Z"`, true},
		{`"2026-09-17T18:56:29.490788Z"`, true},
		{`null`, false}, {`123`, false}, {`""`, false}, {`"yesterday"`, false},
	} {
		t.Run(tc.stamp, func(t *testing.T) {
			data := fmt.Sprintf(`{"sandboxes":[{"name":"pix-demo","id":"943fa6d6-7c2e-4d8e-9886-0b2dcec1ca62","agent":"pix","status":"stopped","last_used_at":"2026-09-17T18:55:57.018942Z","created_at":%s,"workspaces":["/workspace"]}]}`, tc.stamp)
			res, err := ParseList([]byte(data))
			if tc.valid {
				if err != nil || !res.SchemaVerified || res.Entries[0].State != StateStopped {
					t.Fatalf("parse = %+v, %v", res, err)
				}
			} else if err == nil {
				t.Fatal("malformed timestamp accepted")
			}
		})
	}
}

// The regression this fixes, on a synthetic listing in the v0.43 shape: a
// mixed listing must leave the pix row positively identified, because
// workflow/launch's create receipt (PromoteSessionCreation) refuses to
// attach to any row whose IdentityVerified is false. Before created_at was
// documented, every row parsed but reported IdentityVerified=false, and
// `pix run` failed with "was created but reports no verifiable instance id".
func TestParseListV43CreateReceiptStaysVerified(t *testing.T) {
	const data = `{"sandboxes":[
		{"name":"csbx-demo","id":"75fca117-54df-44c1-b7ce-e7e96b002743","agent":"claude","status":"running","last_used_at":"2026-09-17T18:56:29.490788Z","workspaces":["/workspace"],"created_at":"2026-09-17T18:56:19Z"},
		{"name":"shell-demo","id":"ce4eecf4-33eb-4296-bc2c-55c33ef228d3","agent":"shell","status":"stopped","last_used_at":"2026-08-20T05:14:15Z","workspaces":["/workspace"],"created_at":"2026-08-20T05:14:15Z","workspace_missing":true},
		{"name":"pix-demo","id":"a19c9c54-d554-4acd-b9b3-0f9f2bc421c1","agent":"pix","status":"stopped","last_used_at":"2026-09-17T18:55:57.018942Z","workspaces":["/workspace","/context"],"created_at":"2026-09-17T18:55:55Z"}
	]}`
	res, err := ParseList([]byte(data))
	if err != nil {
		t.Fatalf("ParseList: %v", err)
	}
	if !res.SchemaVerified {
		t.Error("SchemaVerified = false, want true for a canonical v0.43 listing")
	}
	got := FindByName(res.Entries, "pix-demo")
	if got == nil {
		t.Fatal("pix row not found")
	}
	// These three are exactly what PromoteSessionCreation gates the attach on.
	if !got.IdentityVerified {
		t.Error("IdentityVerified = false, want true: the create receipt would refuse")
	}
	if got.InstanceID == nil || *got.InstanceID != "a19c9c54-d554-4acd-b9b3-0f9f2bc421c1" {
		t.Errorf("InstanceID = %v, want the row's id", got.InstanceID)
	}
	if got.State != StateStopped {
		t.Errorf("State = %v, want stopped", got.State)
	}
}
