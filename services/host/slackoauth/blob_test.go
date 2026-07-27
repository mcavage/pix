package slackoauth

import (
	"strings"
	"testing"
	"time"
)

func validBlob() Blob {
	return Blob{
		Version:         BlobVersion,
		AccessToken:     "xoxp-access-token",
		RefreshToken:    "xoxe-refresh-token",
		AccessExpiresAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		GrantExpiresAt:  time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC),
		TeamID:          "T0123",
		UserID:          "U0123",
		Scopes:          []string{"channels:read", "chat:write"},
	}
}

// TestParseBlobValid proves a well-formed v1 document round-trips through
// MarshalBlob/ParseBlob byte-for-byte in the fields that matter.
func TestParseBlobValid(t *testing.T) {
	want := validBlob()
	data, err := MarshalBlob(want)
	if err != nil {
		t.Fatalf("MarshalBlob: %v", err)
	}
	got, err := ParseBlob(data)
	if err != nil {
		t.Fatalf("ParseBlob: %v", err)
	}
	if got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken ||
		got.TeamID != want.TeamID || got.UserID != want.UserID {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, want)
	}
	if !got.AccessExpiresAt.Equal(want.AccessExpiresAt) || !got.GrantExpiresAt.Equal(want.GrantExpiresAt) {
		t.Errorf("timestamp round-trip mismatch: got %+v, want %+v", got, want)
	}
	if len(got.Scopes) != 2 {
		t.Errorf("scopes round-trip = %v, want 2 entries", got.Scopes)
	}
}

// TestParseBlobMissingRequiredFields proves every required field is actually
// enforced: dropping any one of them (one at a time) must fail validation,
// and the error must name the missing field so an operator can act on it.
func TestParseBlobMissingRequiredFields(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Blob)
		want   string
	}{
		{"access_token", func(b *Blob) { b.AccessToken = "" }, "access_token"},
		{"refresh_token", func(b *Blob) { b.RefreshToken = "" }, "refresh_token"},
		{"access_expires_at", func(b *Blob) { b.AccessExpiresAt = time.Time{} }, "access_expires_at"},
		{"grant_expires_at", func(b *Blob) { b.GrantExpiresAt = time.Time{} }, "grant_expires_at"},
		{"team_id", func(b *Blob) { b.TeamID = "" }, "team_id"},
		{"user_id", func(b *Blob) { b.UserID = "" }, "user_id"},
		{"scopes", func(b *Blob) { b.Scopes = nil }, "scopes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := validBlob()
			tc.mutate(&b)
			err := b.Validate()
			if err == nil {
				t.Fatalf("Validate() succeeded with %s missing; want an error", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err.Error(), tc.want)
			}
		})
	}
}

// TestParseBlobRejectsWrongVersion proves the schema version is enforced, so
// a future v2 document (or a corrupted one) is never silently accepted as v1.
func TestParseBlobRejectsWrongVersion(t *testing.T) {
	b := validBlob()
	b.Version = 2
	if err := b.Validate(); err == nil {
		t.Fatal("Validate() succeeded with version 2; want a version mismatch error")
	}
}

// TestParseBlobRejectsGarbage proves malformed JSON never becomes a Blob.
func TestParseBlobRejectsGarbage(t *testing.T) {
	if _, err := ParseBlob([]byte("{not json")); err == nil {
		t.Fatal("ParseBlob succeeded on malformed JSON; want an error")
	}
}
