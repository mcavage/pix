package mcp

import "testing"

func TestMcpAuthStatus_NotRequiredIsAnAnswer(t *testing.T) {
	for _, s := range []string{
		`MCP server "slack" does not require OAuth`,
		"no oauth required",
	} {
		if got := McpAuthStatus(s); got != McpAuthNotRequired {
			t.Errorf("McpAuthStatus(%q) = %v, want McpAuthNotRequired", s, got)
		}
	}
	// The regression this guards: "does not require OAuth" contains "not", so a
	// naive ordering reads it as unauthenticated.
	if got := McpAuthStatus(`server "x" does not require OAuth`); got == McpAuthFailed {
		t.Error(`"does not require OAuth" was read as a FAILED auth`)
	}
	// And the real verdicts must still be real.
	if got := McpAuthStatus(`server "notion" authorized`); got != McpAuthOK {
		t.Errorf("authorized = %v, want OK", got)
	}
	for _, bad := range []string{"not authenticated", "unauthorized", "token expired", "401"} {
		if got := McpAuthStatus(bad); got != McpAuthFailed {
			t.Errorf("McpAuthStatus(%q) = %v, want Failed", bad, got)
		}
	}
}
