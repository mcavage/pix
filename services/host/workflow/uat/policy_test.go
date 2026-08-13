package uat

import (
	"os"
	"testing"
)

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

func TestURLPolicy(t *testing.T) {
	policy := URLPolicy{LeasedPorts: []int{8080, 9090}}

	tests := []struct {
		url     string
		wantErr bool
	}{
		{"https://github.com/login/oauth/authorize?client_id=123", false},
		{"https://gitlab.com/oauth/authorize", false},
		{"http://localhost:8080/callback", false},
		{"http://127.0.0.1:9090/callback", false},
		{"https://evil.com/login", true},           // not in registry
		{"http://localhost:8081/callback", true},   // port not leased
		{"http://localhost/callback", true},        // no port
		{"http://192.168.1.1:8080/callback", true}, // not localhost
		{"https://github.com/login#hash", true},    // fragment not allowed
		{"https://user@github.com/login", true},    // userinfo not allowed
		{"file:///etc/passwd", true},               // scheme not allowed
		{"chrome://settings", true},                // scheme not allowed
	}

	for _, tt := range tests {
		_, err := policy.Validate(tt.url)
		if (err != nil) != tt.wantErr {
			t.Errorf("Validate(%q) error = %v, wantErr %v", tt.url, err, tt.wantErr)
		}
	}
}
