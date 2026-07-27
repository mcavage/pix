// Package slackoauth is the foundation for Slack's rotating PKCE OAuth
// credentials (Slack's "token rotation" beta): a public PKCE client (no
// client_secret, ever), a v1 JSON credential blob, an OPStore that persists
// the whole blob as a single 1Password document, and a Manager that reads a
// cached access token, refreshes it exactly once under a lock when it is
// close to expiry, and never hands back a token that was not durably
// written first.
package slackoauth

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// BlobVersion is the current credential blob schema version. ParseBlob and
// Blob.Validate refuse anything else, so a future v2 document (or a
// corrupted v1 one) is never silently treated as v1.
const BlobVersion = 1

// Blob is the JSON credential document persisted for one Slack rotating
// PKCE grant. It is the ENTIRE unit of storage — OPStore and Manager always
// read and write the whole blob, never a partial field, so a torn write can
// never leave the access and refresh tokens out of sync with each other.
type Blob struct {
	Version         int       `json:"version"`
	AccessToken     string    `json:"access_token"`
	RefreshToken    string    `json:"refresh_token"`
	AccessExpiresAt time.Time `json:"access_expires_at"`
	GrantExpiresAt  time.Time `json:"grant_expires_at"`
	TeamID          string    `json:"team_id"`
	UserID          string    `json:"user_id"`
	Scopes          []string  `json:"scopes"`
}

// Validate checks that every field required to trust and use the blob is
// present. It does NOT check expiry — an access token that expired an hour
// ago is still a well-FORMED blob; expiry policy belongs to the Manager, not
// the shape check.
func (b Blob) Validate() error {
	if b.Version != BlobVersion {
		return fmt.Errorf("slackoauth: unsupported blob version %d (want %d)", b.Version, BlobVersion)
	}
	var missing []string
	if b.AccessToken == "" {
		missing = append(missing, "access_token")
	}
	if b.RefreshToken == "" {
		missing = append(missing, "refresh_token")
	}
	if b.AccessExpiresAt.IsZero() {
		missing = append(missing, "access_expires_at")
	}
	if b.GrantExpiresAt.IsZero() {
		missing = append(missing, "grant_expires_at")
	}
	if b.TeamID == "" {
		missing = append(missing, "team_id")
	}
	if b.UserID == "" {
		missing = append(missing, "user_id")
	}
	if len(b.Scopes) == 0 {
		missing = append(missing, "scopes")
	}
	if len(missing) > 0 {
		return fmt.Errorf("slackoauth: blob missing required field(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

// MarshalBlob serializes b as the canonical v1 JSON document. It does not
// validate b first; callers that need a guaranteed-well-formed document
// should call Validate (or use ParseBlob to read one back).
func MarshalBlob(b Blob) ([]byte, error) {
	data, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("slackoauth: encode blob: %w", err)
	}
	return data, nil
}

// ParseBlob decodes and validates a v1 JSON credential document. A malformed
// document, or one missing a required field, is refused.
func ParseBlob(data []byte) (Blob, error) {
	var b Blob
	if err := json.Unmarshal(data, &b); err != nil {
		return Blob{}, fmt.Errorf("slackoauth: decode blob: %w", err)
	}
	if err := b.Validate(); err != nil {
		return Blob{}, err
	}
	return b, nil
}
