package sandbox

import (
	"fmt"
	"testing"
)

// Captured shape from sbx v0.39.0-1112-gad2ac89f4, with synthetic names/paths.
func TestParseListV39OptionalMetadata(t *testing.T) {
	for _, tc := range []struct {
		metadata string
		valid    bool
	}{
		{``, true},
		{`,"workspaces":["/workspace"]`, true},
		{`,"ports":[{"host_ip":"127.0.0.1","host_port":36631,"sandbox_port":4173,"protocol":"tcp4"}]`, true},
		{`,"workspaces":null`, false},
		{`,"workspaces":[123]`, false},
		{`,"ports":{}`, false},
		{`,"ports":[{"host_ip":"127.0.0.1","host_port":1.5,"sandbox_port":4173,"protocol":"tcp4"}]`, false},
		{`,"ports":[{}]`, false},
	} {
		t.Run(tc.metadata, func(t *testing.T) {
			data := fmt.Sprintf(`{"sandboxes":[{"name":"example","id":"943fa6d6-7c2e-4d8e-9886-0b2dcec1ca62","agent":"shell","status":"running"%s}]}`, tc.metadata)
			res, err := ParseList([]byte(data))
			if tc.valid {
				if err != nil || !res.SchemaVerified {
					t.Fatalf("parse = %+v, %v", res, err)
				}
			} else if err == nil {
				t.Fatal("malformed metadata accepted")
			}
		})
	}
}
