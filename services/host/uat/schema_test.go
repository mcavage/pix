package uat

import (
	"testing"
)

func TestUnmarshalScenario_Validation(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		wantErr bool
	}{
		{
			name: "Valid scenario",
			data: `
schema: pix.uat/1
name: valid-scenario
timeout: "5s"
steps:
  - id: step1
    do: mcp_add
    with:
      key: value
`,
			wantErr: false,
		},
		{
			name: "Unknown field",
			data: `
schema: pix.uat/1
name: test
timeout: "5s"
unknown: field
steps: []
`,
			wantErr: true,
		},
		{
			name: "Forbidden action",
			data: `
schema: pix.uat/1
name: test
timeout: "5s"
steps:
  - id: step1
    do: hack_the_mainframe
    with:
      key: value
`,
			wantErr: true,
		},
		{
			name: "Forbidden need",
			data: `
schema: pix.uat/1
name: test
timeout: "5s"
needs:
  - internet
steps:
  - id: step1
    do: mcp_add
`,
			wantErr: true,
		},
		{
			name: "Forbidden assertion",
			data: `
schema: pix.uat/1
name: test
timeout: "5s"
steps:
  - id: step1
    do: mcp_add
    expect:
      unknown_assertion: true
`,
			wantErr: true,
		},
		{
			name: "Empty steps",
			data: `
schema: pix.uat/1
name: test
steps: []
`,
			wantErr: true,
		},
		{
			name: "Forbidden key in with",
			data: `
schema: pix.uat/1
name: test
timeout: "5s"
steps:
  - id: step1
    do: mcp_add
    with:
      shell: /bin/sh
`,
			wantErr: true,
		},
		{
			name: "Nested forbidden key",
			data: `
schema: pix.uat/1
name: test
timeout: "5s"
steps:
  - id: step1
    do: mcp_add
    with:
      nested:
        command: ls
`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := UnmarshalScenario([]byte(tt.data))
			if (err != nil) != tt.wantErr {
				t.Errorf("UnmarshalScenario() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
