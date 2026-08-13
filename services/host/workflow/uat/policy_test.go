package uat

import (
	"context"
	"net"
	"os"
	"testing"
)

type fakeResolver struct {
	ips map[string][]net.IPAddr
}

func (r *fakeResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	if ips, ok := r.ips[host]; ok {
		return ips, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host}
}

func TestPublicLinkPolicy(t *testing.T) {
	resolver := &fakeResolver{
		ips: map[string][]net.IPAddr{
			"public.com":      {{IP: net.ParseIP("93.184.216.34")}},
			"private.com":     {{IP: net.ParseIP("192.168.1.1")}},
			"loopback.com":    {{IP: net.ParseIP("127.0.0.1")}},
			"multicast.com":   {{IP: net.ParseIP("224.0.0.1")}},
			"unspecified.com": {{IP: net.ParseIP("0.0.0.0")}},
		},
	}
	policy := &PublicLinkPolicy{Resolver: resolver}

	valid := []string{
		"http://public.com",
		"https://public.com/path?q=1",
	}

	for _, v := range valid {
		res, err := policy.Validate(v)
		if err != nil {
			t.Errorf("expected %q to be valid, got %v", v, err)
		} else if res.ResolvedIP != "93.184.216.34" {
			t.Errorf("expected IP %s, got %s", "93.184.216.34", res.ResolvedIP)
		}
	}

	invalid := []string{
		"http://localhost",
		"http://private.com",
		"http://loopback.com",
		"http://multicast.com",
		"http://unspecified.com",
		"http://unknown.com",
		"file:///etc/passwd",
		"chrome://settings",
		"http://user:pass@public.com",
		"ftp://public.com",
	}

	for _, inv := range invalid {
		if _, err := policy.Validate(inv); err == nil {
			t.Errorf("expected %q to be invalid", inv)
		}
	}
}

func TestProfilePath(t *testing.T) {
	p, err := ProfilePath()
	if err != nil {
		t.Fatalf("ProfilePath() error: %v", err)
	}

	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("Stat(%q) error: %v", p, err)
	}

	if !info.IsDir() {
		t.Errorf("ProfilePath %q is not a directory", p)
	}

	if perm := info.Mode().Perm(); perm != 0700 {
		t.Errorf("ProfilePath permissions = %v, want 0700", perm)
	}
}

func TestTempProfilePath(t *testing.T) {
	p, err := TempProfilePath("run123")
	if err != nil {
		t.Fatalf("TempProfilePath() error: %v", err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("Stat(%q) error: %v", p, err)
	}
	if !info.IsDir() {
		t.Errorf("TempProfilePath %q is not a directory", p)
	}
	if perm := info.Mode().Perm(); perm != 0700 {
		t.Errorf("TempProfilePath permissions = %v, want 0700", perm)
	}
}

func TestOAuthPolicy(t *testing.T) {
	policy := OAuthPolicy{Provider: ProviderGitHub, LeasedPorts: []int{8080, 9090}}

	tests := []struct {
		url     string
		wantErr bool
	}{
		{"https://github.com/login/oauth/authorize?client_id=123", false},
		{"http://localhost:8080/callback", false},
		{"http://127.0.0.1:9090/callback", false},
		{"https://gitlab.com/oauth/authorize", true}, // provider mismatch (policy is github)
		{"https://evil.com/login", true},             // not in registry
		{"http://localhost:8081/callback", true},     // port not leased
		{"http://localhost/callback", true},          // no port
		{"http://192.168.1.1:8080/callback", true},   // not localhost
		{"https://github.com/login#hash", true},      // fragment not allowed
		{"https://user@github.com/login", true},      // userinfo not allowed
		{"file:///etc/passwd", true},                 // scheme not allowed
		{"chrome://settings", true},                  // scheme not allowed
	}

	for _, tt := range tests {
		_, err := policy.Validate(tt.url)
		if (err != nil) != tt.wantErr {
			t.Errorf("Validate(%q) with github provider error = %v, wantErr %v", tt.url, err, tt.wantErr)
		}
	}

	// Test all providers
	providers := []struct {
		provider OAuthProvider
		url      string
	}{
		{ProviderGitHub, "https://github.com/login"},
		{ProviderGitLab, "https://gitlab.com/login"},
		{ProviderGoogle, "https://accounts.google.com/o/oauth2/v2/auth"},
		{ProviderNotion, "https://api.notion.com/v1/oauth/authorize"},
		{ProviderNotion, "https://www.notion.so/install-integration"},
		{ProviderNotion, "https://notion.so/install-integration"},
		{ProviderAtlassian, "https://auth.atlassian.com/authorize"},
		{ProviderAtlassian, "https://api.atlassian.com/oauth2/authorize"},
		{ProviderGranola, "https://api.granola.so/oauth/authorize"},
		{ProviderGranola, "https://auth.granola.so/login"},
	}

	for _, pt := range providers {
		p := OAuthPolicy{Provider: pt.provider, LeasedPorts: []int{}}
		if _, err := p.Validate(pt.url); err != nil {
			t.Errorf("Validate(%q) for provider %q unexpected error: %v", pt.url, pt.provider, err)
		}
	}

	// Test unknown refusal
	p := OAuthPolicy{Provider: "unknown", LeasedPorts: []int{}}
	if _, err := p.Validate("https://github.com/login"); err == nil {
		t.Errorf("Expected error for unknown provider")
	}

	// Test empty provider refusal
	pEmpty := OAuthPolicy{Provider: "", LeasedPorts: []int{}}
	if _, err := pEmpty.Validate("https://github.com/login"); err == nil {
		t.Errorf("Expected error for empty provider on https url")
	}
}
