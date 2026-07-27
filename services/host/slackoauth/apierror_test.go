package slackoauth

import (
	"errors"
	"strings"
	"testing"
)

func TestClassifyAPIErrorMapsKnownDeadCredentialCodes(t *testing.T) {
	cases := []struct {
		code string
		want error
	}{
		{"invalid_auth", ErrInvalidAuth},
		{"token_revoked", ErrTokenRevoked},
		{"invalid_refresh_token", ErrInvalidRefreshToken},
	}
	for _, tc := range cases {
		err := ClassifyAPIError("prefix", tc.code)
		if !errors.Is(err, tc.want) {
			t.Errorf("ClassifyAPIError(prefix, %q) = %v, want errors.Is match for %v", tc.code, err, tc.want)
		}
		if !IsDeadCredential(err) {
			t.Errorf("IsDeadCredential(%v) = false, want true for code %q", err, tc.code)
		}
	}
}

func TestClassifyAPIErrorFallsBackForUnknownCodes(t *testing.T) {
	err := ClassifyAPIError("slackoauth: oauth.v2.access failed", "invalid_grant")
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("error = %q, want it to mention the raw code", err.Error())
	}
	if IsDeadCredential(err) {
		t.Error("an unrecognized code must never be treated as a dead credential")
	}
}

func TestIsDeadCredentialCoversGrantExpired(t *testing.T) {
	if !IsDeadCredential(ErrGrantExpired) {
		t.Error("ErrGrantExpired must be treated as a dead credential")
	}
	wrapped := errors.New("outer")
	if IsDeadCredential(wrapped) {
		t.Error("an arbitrary error must never be treated as a dead credential")
	}
}
