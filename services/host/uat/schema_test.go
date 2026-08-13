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
timeout: 5s
steps:
  - id: step1
    do: action
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
unknown: field
steps: []
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
timeout: 5s
steps:
  - id: step1
    do: action
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
timeout: 5s
steps:
  - id: step1
    do: action
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
