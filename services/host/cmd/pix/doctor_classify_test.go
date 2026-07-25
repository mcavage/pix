package main

import (
	"errors"
	"testing"
)

// TestClassifyProbeFailure_Denied: only EXPLICIT policy/permission denial
// tokens classify as denied.
func TestClassifyProbeFailure_Denied(t *testing.T) {
	cases := []string{
		"HTTP 403 Forbidden",
		"error: Forbidden",
		"OAuth error: access_denied",
		"server said: access denied",
		"tool call not allowed by policy",
		"request denied by policy",
		"request denied by org policy",
		"call blocked by your organization's policy",
		"rejected by workspace policies",
		"policy denies tool execution",
		"policy forbids this scope",
		"explicit policy denial for scope drive.readonly",
		"permission denied by workspace policy",
		`status 403: {"error":"request denied"}`,
	}
	for _, out := range cases {
		if got := classifyProbeFailure(out, nil); got != probeDenied {
			t.Errorf("classifyProbeFailure(%q) = %v, want probeDenied", out, got)
		}
	}
	// The error text is classified too, not just stdout.
	if got := classifyProbeFailure("", errors.New("upstream: not allowed by policy")); got != probeDenied {
		t.Errorf("error-carried denial = %v, want probeDenied", got)
	}
}

// TestClassifyProbeFailure_AuthTodo: a bare 401/unauthorized is an auth gap
// for the caller to surface as a credential TODO — NOT an org-policy denial.
func TestClassifyProbeFailure_AuthTodo(t *testing.T) {
	cases := []string{
		"HTTP 401",
		"401 Unauthorized",
		"error: unauthorized",
		"rpc error: unauthenticated",
		"authentication required",
	}
	for _, out := range cases {
		if got := classifyProbeFailure(out, nil); got != probeAuthTodo {
			t.Errorf("classifyProbeFailure(%q) = %v, want probeAuthTodo", out, got)
		}
	}
}

// TestClassifyProbeFailure_Unverifiable: transport/infra/generic failures are
// unverifiable — doctor must never invent a verified failure out of them.
func TestClassifyProbeFailure_Unverifiable(t *testing.T) {
	cases := []string{
		"dial tcp 10.0.0.1:443: connect: connection refused",
		"context deadline exceeded (timeout)",
		"unexpected EOF",
		"lookup api.example.com: no such host",
		"tls: handshake failure",
		"exit status 1",
		"",
		// A bare 403 with NO denial body stays unverifiable (contentless
		// proxy/gateway 403s are transport noise, not a proven policy denial).
		"HTTP status 403",
		// A bare "permission denied" outside a policy context (file perms,
		// ssh) is NOT an org-policy denial.
		"open /etc/foo: permission denied",
	}
	for _, out := range cases {
		if got := classifyProbeFailure(out, nil); got != probeUnverifiable {
			t.Errorf("classifyProbeFailure(%q) = %v, want probeUnverifiable", out, got)
		}
	}
	if got := classifyProbeFailure("", errors.New("signal: killed")); got != probeUnverifiable {
		t.Errorf("generic error = %v, want probeUnverifiable", got)
	}
}

// TestClassifyProbeFailure_NoNaivePolicyMatch is the false-positive gate: help
// or documentation text that merely MENTIONS "policy" (or carries denial words
// in unrelated positions) must not classify as denied.
func TestClassifyProbeFailure_NoNaivePolicyMatch(t *testing.T) {
	cases := []string{
		"usage: tool --policy <file>   set the retry policy",
		"see https://example.com/docs/policy for details",
		"the retention policy defaults to 30 days",
		"error: invalid flag --policy-file (connection closed)",
		// "denied" and "policy" both present but NOT bound together in a
		// denial phrase (separate lines).
		"request was denied\nsee the docs about the retry policy",
	}
	for _, out := range cases {
		if got := classifyProbeFailure(out, nil); got == probeDenied {
			t.Errorf("classifyProbeFailure(%q) = probeDenied; naive policy match", out)
		}
	}
}

// TestClassifyProbeFailure_DeniedTrumpsAuth: an explicit denial that ALSO
// carries auth words classifies as denied (the stronger, positive signal).
func TestClassifyProbeFailure_DeniedTrumpsAuth(t *testing.T) {
	out := "403 Forbidden: token valid but access denied by org policy (was 401 before login)"
	if got := classifyProbeFailure(out, nil); got != probeDenied {
		t.Errorf("classifyProbeFailure(%q) = %v, want probeDenied", out, got)
	}
}

// TestProbeClassString: the machine-readable evidence tokens.
func TestProbeClassString(t *testing.T) {
	for want, p := range map[string]probeClass{
		"denied":       probeDenied,
		"auth-todo":    probeAuthTodo,
		"unverifiable": probeUnverifiable,
	} {
		if got := p.String(); got != want {
			t.Errorf("%v.String() = %q, want %q", p, got, want)
		}
	}
}
