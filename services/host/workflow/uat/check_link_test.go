package uat

import (
	"context"
	"net/url"
	"testing"
)

func TestCheckLink(t *testing.T) {
	policy := &URLPolicy{LeasedPorts: []int{8080}}
	cfg := OAuthConfig{Policy: policy}

	tests := []struct {
		name    string
		rawURL  string
		b       *fakeBrowser
		wantErr bool
	}{
		{
			name:   "success",
			rawURL: "http://localhost:8080/test",
			b: &fakeBrowser{
				currURL: &url.URL{Scheme: "http", Host: "localhost:8080", Path: "/test"},
			},
			wantErr: false,
		},
		{
			name:   "redirect to disallowed",
			rawURL: "http://localhost:8080/redirect",
			b: &fakeBrowser{
				currURL: &url.URL{Scheme: "https", Host: "evil.com", Path: "/"},
			},
			wantErr: true,
		},
		{
			name:    "invalid initial url",
			rawURL:  "http://localhost:9999/test", // not leased
			b:       nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeFactory{b: tt.b}
			res, err := CheckLink(context.Background(), f, cfg, tt.rawURL)
			if (err != nil) != tt.wantErr {
				t.Fatalf("CheckLink() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && res == nil {
				t.Errorf("CheckLink() returned nil result for successful check")
			}
		})
	}
}
