package uat

import (
	"testing"
)

// TestLegalVocabulary_IsByteForByteStable pins the closed scenario vocabulary.
// Story 0 of docs/design/environments.md extends candidate_smoke with typed
// internal named checks (uatenvmatrix), NOT a new action, need, or assertion —
// this is the byte-for-byte guard that a future change accidentally growing
// the MCP-facing vocabulary (instead of the internal named-check registry)
// must fail.
func TestLegalVocabulary_IsByteForByteStable(t *testing.T) {
	needs, actions, assertions := LegalVocabulary()
	wantNeeds := []string{"browser", "docker", "mcp", "sbx"}
	wantActions := []string{"browser_check", "candidate_smoke", "mcp_add", "mcp_auth", "mcp_remove", "mcp_status"}
	wantAssertions := []string{"artifact_contains", "artifact_exists", "browser_text", "browser_url", "mcp_status", "verdict"}

	assertEqual := func(label string, got, want []string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s = %#v, want %#v", label, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s[%d] = %q, want %q (full: %#v)", label, i, got[i], want[i], got)
			}
		}
	}
	assertEqual("needs", needs, wantNeeds)
	assertEqual("actions", actions, wantActions)
	assertEqual("assertions", assertions, wantAssertions)
}

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
      name: test-mcp
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
      name: test
`,
			wantErr: true,
		},
		{
			name: "Unimplemented named check cannot silently pass",
			data: `
schema: pix.uat/1
name: test
timeout: "5s"
steps:
  - id: step1
    do: check
    with:
      name: services.memory_restart
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
    with:
      name: test
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
    with:
      name: test
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
      name: test
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
      name: test
      nested:
        command: ls
`,
			wantErr: true,
		},
		{
			name: "Candidate smoke valid",
			data: `
schema: pix.uat/1
name: test
timeout: "5s"
steps:
  - id: step1
    do: candidate_smoke
    with: {}
`,
			wantErr: false,
		},
		{
			name: "Candidate smoke invalid with fields",
			data: `
schema: pix.uat/1
name: test
timeout: "5s"
steps:
  - id: step1
    do: candidate_smoke
    with:
      name: invalid_here
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
