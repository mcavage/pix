package uat

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

type fakeBrowser struct {
	currURL *url.URL
	waitErr error
}

func (f *fakeBrowser) Snapshot(ctx context.Context) (*Snapshot, error) {
	return &Snapshot{Screenshot: []byte("fake-screenshot")}, nil
}
func (f *fakeBrowser) Click(ctx context.Context, refID string) error {
	return nil
}
func (f *fakeBrowser) WaitForURL(ctx context.Context, expectedURL *url.URL) error {
	return f.waitErr
}
func (f *fakeBrowser) CurrentURL(ctx context.Context) (*url.URL, error) {
	return f.currURL, nil
}
func (f *fakeBrowser) Title(ctx context.Context) (string, error) {
	return "Title", nil
}
func (f *fakeBrowser) VisibleText(ctx context.Context) (string, error) {
	return "", nil
}
func (f *fakeBrowser) Close() error {
	return nil
}

type fakeFactory struct {
	b *fakeBrowser
}

func (f *fakeFactory) NewContext(ctx context.Context, runID string, initialURL *url.URL, policy URLValidator) (Browser, error) {
	return f.b, nil
}
func (f *fakeFactory) NewOAuthContext(ctx context.Context, initialURL *url.URL, policy URLValidator) (Browser, error) {
	return f.b, nil
}

func TestCaptureOAuth(t *testing.T) {
	policy := &OAuthPolicy{Provider: ProviderGitHub, LeasedPorts: []int{8080}}

	tests := []struct {
		name    string
		cfg     OAuthConfig
		b       *fakeBrowser
		wantCap OAuthCapture
		wantErr bool
	}{
		{
			name: "success",
			cfg: OAuthConfig{
				RunID:       "run123",
				AuthURL:     "https://github.com/login",
				CallbackURL: "http://localhost:8080/cb",
				Provider:    ProviderGitHub,
				Policy:      policy,
			},
			b: &fakeBrowser{
				currURL: &url.URL{Scheme: "http", Host: "localhost:8080", Path: "/cb", RawQuery: "code=123"},
			},
			wantCap: OAuthCapture{
				RunID:    "run123",
				Provider: ProviderGitHub,
				Callback: "http://localhost:8080/cb?code=123",
			},
		},
		{
			name: "incomplete",
			cfg: OAuthConfig{
				RunID:       "run123",
				AuthURL:     "https://github.com/login",
				CallbackURL: "http://localhost:8080/cb",
				Provider:    ProviderGitHub,
				Policy:      policy,
			},
			b: &fakeBrowser{
				waitErr: ErrIncomplete,
			},
			wantCap: OAuthCapture{
				RunID:  "run123",
				Provider: ProviderGitHub,
				Error:  "incomplete",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			outPath := filepath.Join(dir, "capture.json")

			f := &fakeFactory{b: tt.b}
			err := CaptureOAuth(context.Background(), f, tt.cfg, outPath)
			if (err != nil) != tt.wantErr {
				t.Fatalf("CaptureOAuth() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}

			data, err := os.ReadFile(outPath)
			if err != nil {
				t.Fatalf("ReadFile() error: %v", err)
			}

			info, _ := os.Stat(outPath)
			if info.Mode().Perm() != 0600 {
				t.Errorf("Capture file permissions = %v, want 0600", info.Mode().Perm())
			}

			var got OAuthCapture
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal() error: %v", err)
			}

			if got != tt.wantCap {
				t.Errorf("Capture = %+v, want %+v", got, tt.wantCap)
			}
		})
	}
}
