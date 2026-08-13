package uat

import (
	"context"
	"net/url"
	"testing"
)

func TestCheckLink(t *testing.T) {
	policy := &OAuthPolicy{LeasedPorts: []int{8080}}
	cfg := CheckLinkConfig{Policy: policy}

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
