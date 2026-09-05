package mcp

import (
	"testing"
)

func TestRemoteMCPRegistrationCurrentRejectsEndpointSubstring(t *testing.T) {
	want := "https://expected.example/mcp"
	payload := `{"url":"https://evil.example/?next=https://expected.example/mcp"}`
	if outputContainsCanonicalEndpoint(payload, want) {
		t.Fatal("an endpoint embedded inside another URL must not count as the registered endpoint")
	}
}

func TestRemoteMCPRegistrationCurrentRequiresEndpointField(t *testing.T) {
	want := "https://expected.example/mcp?a=1&b=2"
	for _, payload := range []string{
		`{"url":"https://evil.example/mcp","note":"https://expected.example/mcp?a=1&b=2"}`,
		`{"callback_url":"https://expected.example/mcp?a=1&b=2"}`,
		`{"url":"https://evil.example/mcp","nested":{"endpoint":"https://expected.example/mcp?a=1&b=2"}}`,
		`url: https://evil.example/?next=https://expected.example/mcp?a=1&b=2`,
	} {
		if outputContainsCanonicalEndpoint(payload, want) {
			t.Fatalf("non-endpoint evidence was trusted: %s", payload)
		}
	}
}

func TestRemoteMCPRegistrationCurrentCanonicalExactMatch(t *testing.T) {
	want := "https://expected.example/mcp?b=2&a=1"
	payload := `{"server":{"remote_url":"HTTPS://EXPECTED.EXAMPLE:443/mcp?a=1&b=2"}}`
	if !outputContainsCanonicalEndpoint(payload, want) {
		t.Fatal("canonically identical endpoint field was not recognized")
	}
}
