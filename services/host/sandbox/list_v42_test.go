package sandbox

import (
	"fmt"
	"testing"
)

// Shape observed on sbx v0.42.1-758-gdf5c96ba6, with synthetic identity.
func TestParseListV42LastUsedAt(t *testing.T) {
	for _, tc := range []struct {
		stamp string
		valid bool
	}{
		{`"2026-09-09T22:56:38.792733Z"`, true},
		{`"2026-08-19T00:45:09Z"`, true},
		{`null`, false}, {`123`, false}, {`""`, false}, {`"yesterday"`, false},
	} {
		t.Run(tc.stamp, func(t *testing.T) {
			data := fmt.Sprintf(`{"sandboxes":[{"name":"pix-demo","id":"943fa6d6-7c2e-4d8e-9886-0b2dcec1ca62","agent":"pix","status":"stopped","last_used_at":%s,"workspaces":["/workspace"]}]}`, tc.stamp)
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
